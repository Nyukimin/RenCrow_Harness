#!/usr/bin/env python3
"""Validate design artifacts only. This does not run the proposed Harness."""
from __future__ import annotations
import hashlib, json, re, sqlite3, sys, tempfile
from pathlib import Path
try:
    from jsonschema import Draft202012Validator
except ImportError:
    raise SystemExit('jsonschema is required for document checks: python -m pip install -r tools/speccheck/requirements.txt')
from layout import ROOT, DOCS, path

def load(name:str):
    return json.loads(path(name).read_text(encoding='utf-8'))

def main()->dict:
    checks=[]
    def ok(name,detail): checks.append({'check':name,'status':'passed','detail':detail})
    schemas={p.name:json.loads(p.read_text()) for p in path('schemas/x').parent.glob('*.json')}
    for name,s in schemas.items(): Draft202012Validator.check_schema(s)
    ok('schema_metaschema',len(schemas))
    ps=schemas['protocol.schema.json']; defs=ps['$defs']
    def check_refs(v):
        if isinstance(v,dict):
            if '$ref' in v and v['$ref'].startswith('#/$defs/'):
                assert v['$ref'].split('/')[-1] in defs, v['$ref']
            for z in v.values():check_refs(z)
        elif isinstance(v,list):
            for z in v:check_refs(z)
    check_refs(ps);ok('native_type_refs',len(defs))
    validator=Draft202012Validator(ps)
    requests=load('examples/native_requests.json')
    api=load('api_contract.json')['methods']
    assert {r['method'] for r in requests}=={m['method'] for m in api}
    for r in requests:
        validator.validate(r)
        bad=dict(r,unexpected=True)
        assert not validator.is_valid(bad),r['method']
    for m in api:
        assert m['input_type'] in defs and m['result_type'] in defs
    def validate_type(name,x):
        Draft202012Validator({'$ref':'#/$defs/'+name,'$defs':defs}).validate(x)
    validate_type('StartInput',load('examples/core_start.json'))
    validate_type('RunResult',load('examples/run_result.json'))
    for x in load('examples/compact_results.json'):validate_type('CompactResult',x)
    ok('all_native_methods_and_examples',len(api))
    for file,schema in [('selection_revoke.json','selection.schema.json'),('summary.json','summary.schema.json'),('config.json','config.schema.json'),('measure.json','measure.schema.json')]:
        Draft202012Validator(schemas[schema]).validate(load('examples/'+file))
    bad=load('examples/selection_revoke.json');bad['operations'][0].pop('replacement_handle')
    assert not Draft202012Validator(schemas['selection.schema.json']).is_valid(bad)
    bad=load('examples/summary.json');bad['current_work'][0]['source_handles']=['bogus-0']
    assert not Draft202012Validator(schemas['summary.schema.json']).is_valid(bad)
    bad=load('examples/compact_results.json')[0];bad['checkpoint_id']=None
    assert not Draft202012Validator({'$ref':'#/$defs/CompactResult','$defs':defs}).is_valid(bad)
    bad=load('examples/run_result.json');bad['verification']['status']='passed'
    assert not Draft202012Validator({'$ref':'#/$defs/RunResult','$defs':defs}).is_valid(bad)
    ok('negative_schema_fixtures',4)
    # Request-local source handles are checked separately from JSON Schema.
    sel=load('examples/selection_revoke.json')['operations'][0]
    source={'presented-0':('human',0,'方式Aを実装する。'),'presented-1':('human',1,'方式Aはやめる。')}
    target=source[sel['target_handle']];proof=source[sel['replacement_handle']]
    assert proof[1]>target[1] and target[2].count(sel['target_quote'])==1 and proof[2].count(sel['replacement_quote'])==1
    assert sel['basis']=='revocation'
    handles={'work-0','instruction-0','observation-0','summary-0','completion-0'}
    summary=load('examples/summary.json')
    for key,values in summary.items():
        if key=='important_observation_handles':assert set(values)<=handles
        else:
            for x in values:assert set(x['source_handles'])<=handles
    ok('synthetic_handle_and_revocation_examples','schema-independent fixture check only')
    matrix=load('acceptance_matrix.json');tests=matrix['tests'];funcs=matrix['functions']
    tm={t['test_id']:t for t in tests};fm={f['function_id']:f for f in funcs}
    assert len(tm)==len(tests) and len(fm)==len(funcs)
    assert len([t for t in tests if t['test_id'].startswith('H')])==26
    assert [tm[f'H{i:02}']['source']['history_row'] for i in range(1,27)]==list(range(1,27))
    req=set(matrix['requirement_ids']);covered=set()
    for t in tests:
        assert t['functions'] and t['requirements'] and t['status']=='not_run' and not t['execution_evidence']
        covered.update(t['requirements']);assert set(t['requirements'])<=req
        for fid in t['functions']:assert t['test_id'] in fm[fid]['tests']
    for f in funcs:
        assert f['phase'] in ['P0','P1','P2','P3','P4','P5','P6','P7'] and f['tests']
        for tid in f['tests']:assert f['function_id'] in tm[tid]['functions']
    assert covered==req
    for x in load('review_resolution.json')['items']:
        assert x['tests'] and set(x['tests'])<=set(tm)
    ok('bidirectional_traceability',{'tests':len(tests),'functions':len(funcs),'requirements':len(req),'review_items':len(load('review_resolution.json')['items'])})
    # SQLite in-memory design check; no production database is opened.
    db=sqlite3.connect(':memory:');db.executescript(path('sql/001_initial.sql').read_text())
    assert db.execute('PRAGMA foreign_keys').fetchone()[0]==1
    db.execute("INSERT INTO sessions VALUES('s','p','time','/fixture','pol','structured_only')")
    db.execute("INSERT INTO threads(thread_id,session_id,binding_json,policy_revision,binding_revision) VALUES('t','s','{}','1','1')")
    db.execute("INSERT INTO evidence(evidence_id,principal,owner,state,media_type,metadata_json) VALUES('e','p','h','building','text/plain','{}')")
    db.execute("INSERT INTO evidence_chunks VALUES('e',0,0,?)",(b'abc',))
    db.execute("UPDATE evidence SET state='sealed',capture_complete=1,total_bytes=3,raw_hash=? WHERE evidence_id='e'",(hashlib.sha256(b'abc').hexdigest(),))
    def refuses(sql,params=()):
        try:db.execute(sql,params)
        except sqlite3.DatabaseError:return
        raise AssertionError('SQL unexpectedly accepted: '+sql)
    refuses("UPDATE evidence SET total_bytes=4 WHERE evidence_id='e'")
    refuses("INSERT INTO evidence_chunks VALUES('e',1,3,x'64')")
    refuses("UPDATE evidence_chunks SET data=x'64' WHERE evidence_id='e'")
    refuses("DELETE FROM evidence WHERE evidence_id='e'")
    db.execute("INSERT INTO receipts VALUES('r','p','key','op',?,'terminal','{}',NULL,'t','t')",('0'*64,))
    refuses("INSERT INTO receipts VALUES('r2','p','key','op',?,'terminal','{}',NULL,'t','t')",('0'*64,))
    assert db.execute('PRAGMA foreign_key_check').fetchall()==[]
    ok('sqlite_ddl_constraints',{'tables':db.execute("SELECT count(*) FROM sqlite_master WHERE type='table'").fetchone()[0],'negative_constraints':5,'scope':'SQLite schema only; no Go/OS durability test'})
    db.close()
    # All local Markdown targets exist. URLs and source anchors are not network-tested here.
    nlinks=0
    for p in DOCS.glob('*.md'):
        text=p.read_text()
        for link in re.findall(r'\]\(([^)]+)\)',text):
            if re.match(r'\w+://',link) or link.startswith('#'):continue
            target=link.split('#')[0]
            if target:assert (p.parent/target).exists(),(p.name,link)
            nlinks+=1
    ok('local_document_links',nlinks)
    for p in ROOT.rglob('*.json'):json.loads(p.read_text())
    ok('all_json_syntax',len(list(ROOT.rglob('*.json'))))
    return {'scope':'design_asset_validation_only','runtime_implemented':False,'runtime_tests_executed':False,'checks':checks,'limitations':['No product Go compilation or execution','No real Model/Tool E2E','No OS sandbox/durability/crash benchmark','No production changes','Schema and trace tests do not prove semantic correctness']}

if __name__=='__main__':
    try:
        result=main()
        path('DOCUMENT_VALIDATION.json').write_text(json.dumps(result,ensure_ascii=False,indent=2)+'\n')
        print(json.dumps(result,ensure_ascii=False,indent=2))
    except Exception as exc:
        print(f'VALIDATION FAILED: {exc}',file=sys.stderr)
        raise
