#!/usr/bin/env python3
"""Check document contracts and synthetic fixtures, not the proposed product runtime."""
from __future__ import annotations
import copy, hashlib, hmac, json, re, sqlite3, struct, sys
from pathlib import Path
try:
    from jsonschema import Draft202012Validator
except ImportError:
    raise SystemExit('Install document-check dependencies: python -m pip install -r tools/speccheck/requirements.txt')
from layout import path as _path
def load(name): return json.loads(_path(name).read_text(encoding='utf-8'))
def lp(value):
    raw=str(value).encode('utf-8')
    return len(raw).to_bytes(8,'big')+raw

def main():
    results=[]
    def passed(name, details): results.append({'check':name,'status':'passed','detail':details})
    protocol=load('schemas/protocol.schema.json'); defs=protocol['$defs']
    def v(name): return Draft202012Validator({'$defs':defs,'$ref':'#/$defs/'+name})
    for file,name in [('start_result.json','StartResult'),('resume_result.json','ResumeResult'),('intake_receipt.json','IntakeReceipt'),('core_relay_start.json','StartInput'),('progress_reset.json','ProgressReset')]:
        v(name).validate(load('examples/'+file))
    for x in load('examples/model_attempt_receipts.json'):v('ModelAttemptReceipt').validate(x)
    ls=load('schemas/llm_contract.schema.json')
    for file in ['gateway_attempt_receipt.json','recovery_request.json']:Draft202012Validator(ls).validate(load('examples/'+file))
    bad=copy.deepcopy(load('examples/model_attempt_receipts.json')[1]);bad['ordinal']=2
    assert not v('ModelAttemptReceipt').is_valid(bad)
    bad=copy.deepcopy(load('examples/model_attempt_receipts.json')[1]);bad['stage']='work_summary'
    assert not v('ModelAttemptReceipt').is_valid(bad)
    bad=load('examples/gateway_attempt_receipt.json');bad['hidden_retry']=True
    assert not Draft202012Validator(ls).is_valid(bad)
    bad=copy.deepcopy(load('examples/model_attempt_receipts.json')[2]);bad['backend_attempts']=0
    assert not v('ModelAttemptReceipt').is_valid(bad)
    resume=next(x['params'] for x in load('examples/native_requests.json') if x['method']=='run/resume')
    bad=copy.deepcopy(resume);bad.pop('limits');assert not v('ResumeInput').is_valid(bad)
    block=load('examples/context_revision_vectors.json')[0]['block']
    bad=dict(block,digest='0'*64);assert not v('ContextBlock').is_valid(bad)
    bad=load('examples/core_start.json');bad['context_blocks'].append(dict(block,kind='user_message'))
    assert not v('StartInput').is_valid(bad)
    passed('new_public_types_and_rejections',{'positive_files':8,'negative_cases':7})

    # Compare source tables with schema. Do not claim that this validates prose meaning.
    text=_path('PROTOCOL.md').read_text()
    checked=[]
    for name in ['ContextBlock','OriginProof','Limits','StartResult','ResumeInput','ResumeResult','InputReceipt','RunInfo','RecoveryPolicy','IntakeReceipt','ModelAttemptReceipt','ProgressDelta','ProgressReset','ProgressGap']:
        section=re.search(r'### '+name+r'\n(.*?)(?=\n### |\n## |\Z)',text,re.S)
        assert section, name
        fields=re.findall(r'^\| `([^`]+)` \|',section.group(1),re.M)
        assert fields==list(defs[name]['properties']), (name,fields,list(defs[name]['properties']))
        checked.append(name)
    assert load('schemas/config.schema.json')['$defs']==defs
    impl=_path('RenCrow_Harness_IMPLEMENTATION_SPEC.md').read_text()
    assert 'rh-origin-v1' in impl and '旧draftのrh-origin-v1' in impl
    assert 'Recovering, RetryWaiting, Terminal' in impl
    passed('public_table_schema_and_shared_defs',checked)

    # Independently recompute the fixed byte contracts from the public vectors.
    revisions=[]
    for rec in load('examples/context_revision_vectors.json'):
        b=rec['block'];src=b['source']
        if src is None: tail=b'\0'
        else:
            vals=[src['owner'],src['source_id'],src['raw_hash'],src['projection_version'],src['range']['start'],src['range']['end'],src['origin'],src['sequence']]
            tail=b'\1'+b''.join(lp(s) for s in vals)
        raw=b'rencrow-context-block/v1\0'+lp(b['kind'])+lp(b['text'])+tail
        actual='ctx-v1:'+hashlib.sha256(raw).hexdigest()
        assert actual==b['revision']
        assert hashlib.sha256(b['text'].encode()).hexdigest()==rec['computed_text_digest']
        revisions.append(actual)
    assert len(set(revisions))==len(revisions),'newlines and Unicode variants must not normalize silently'
    vector=load('examples/origin_proof_vector.json');proof=vector['proof']
    keys=['issuer','key_id','audience','origin','source_message_id','source_thread_id','destination_thread_id','mutation_key','raw_hash','sequence','issued_at','expires_at','nonce']
    raw=b'rencrow-origin-proof/v1\0'+b''.join(lp(proof[k]) for k in keys)
    assert hashlib.sha256(raw).hexdigest()==vector['mac_input_sha256']
    assert hashlib.sha256(vector['text'].encode()).hexdigest()==proof['raw_hash']
    expected=hmac.new(bytes.fromhex(vector['key_hex']),raw,'sha256').hexdigest()
    assert hmac.compare_digest(expected,proof['mac'])
    v('OriginProof').validate(proof)
    for field in ['audience','destination_thread_id','mutation_key','key_id','raw_hash']:
        changed=dict(proof);changed[field]=str(changed[field])+'x'
        changed_raw=b'rencrow-origin-proof/v1\0'+b''.join(lp(changed[k]) for k in keys)
        assert not hmac.compare_digest(hmac.new(bytes.fromhex(vector['key_hex']),changed_raw,'sha256').hexdigest(),proof['mac'])
    passed('context_revision_and_hmac_vectors',{'context_vectors':len(revisions),'tampering_cases':5,'scope':'byte-contract fixtures, not actual authentication'})

    # Work dependency topology and function coverage.
    plan=load('work_packages.json')['packages']; byid={x['work_id']:x for x in plan}
    matrix=load('acceptance_matrix.json'); fids={x['function_id'] for x in matrix['functions']};tids={x['test_id'] for x in matrix['tests']}
    seen=set();active=set()
    def visit(wid):
        if wid in seen:return
        assert wid not in active,'dependency cycle'
        active.add(wid)
        for dep in byid[wid]['dependencies']:
            assert dep in byid;visit(dep)
        active.remove(wid);seen.add(wid)
    coverage=set()
    for row in plan:
        visit(row['work_id']);coverage.update(row['functions'])
        assert set(row['functions'])<=fids and set(row['tests'])<=tids
        assert row['implementation_owner']=='Claude' and row['design_owner']=='ルミナ'
        assert row['status']=='not_started' and not row['completion_evidence']
    assert coverage==fids
    assert next(x for x in matrix['tests'] if x['test_id']=='A37')['required_for']=='P7_when_requested'
    assert all(x['status']=='not_run' and not x['execution_evidence'] for x in matrix['tests'])
    passed('work_dependency_and_scope_checks',{'packages':len(plan),'functions':len(coverage),'mandatory_runtime_tests':len(tids)-1,'conditional_migration_test':1})

    # DDL semantic checks on a new in-memory SQLite database, no product writes.
    db=sqlite3.connect(':memory:');db.executescript(_path('sql/001_initial.sql').read_text())
    db.execute("INSERT INTO sessions VALUES('s','p','time','/fixture','pol','structured_only')")
    db.execute("INSERT INTO threads(thread_id,session_id,binding_json,policy_revision,binding_revision) VALUES('t','s','{}','1','1')")
    db.execute("INSERT INTO tasks(task_id,thread_id,kind,status,created_at) VALUES('k','t','work','open','now')")
    db.execute("INSERT INTO runs(run_id,task_id,trace_id,phase,status,writer_epoch,started_at,limits_json,deadline_at,recovery_policy_json,recovery_policy_revision) VALUES('r','k','tr','Generating','running',1,'now','{}','later','{}',?)",('0'*64,))
    db.execute("INSERT INTO actions(action_id,run_id,kind,name,args_bytes,args_hash,status,created_at) VALUES('a','r','model','act',x'00',?,'open','now')",('0'*64,))
    for i in range(8):db.execute("INSERT INTO attempts(attempt_id,action_id,ordinal,state,started_at) VALUES(?,?,?,'prepared','now')",('att'+str(i),'a',i))
    def insert_mc(idx,stage,ordinal,state,count,profile='same_request'):
        db.execute('''INSERT INTO model_calls(request_id,run_id,action_id,attempt_id,stage,request_digest,binding_fingerprint,logical_requests,backend_attempts,generation_state,attempt_ordinal,recovery_profile,recovery_profile_revision,base_request_digest,applied_transformations_json) VALUES(?,?,?,?,?,?,?,1,?,?,?,?,?,?,?)''',('req'+str(idx),'r','a','att'+str(idx),stage,'0'*64,'fp',count,state,ordinal,profile,'v1','0'*64,'[]'))
    insert_mc(0,'act',0,'not_started',0)
    insert_mc(1,'act',1,'terminal',1)
    insert_mc(2,'act',0,'unknown',None)
    def refuses(fn):
        try:fn()
        except sqlite3.DatabaseError:return
        raise AssertionError('invalid SQL state was accepted')
    refuses(lambda:insert_mc(3,'act',0,'terminal',None))
    refuses(lambda:insert_mc(4,'act',0,'terminal',2))
    refuses(lambda:insert_mc(5,'work_summary',1,'terminal',1))
    refuses(lambda:insert_mc(6,'act',2,'terminal',1))
    refuses(lambda:db.execute("UPDATE runs SET generation_attempts_unknown=1 WHERE run_id='r'"))
    assert db.execute('PRAGMA foreign_key_check').fetchall()==[]
    db.close()
    passed('sqlite_attempt_budget_constraints',{'valid_cases':3,'negative_cases':5,'scope':'DDL only; no Go/durability/runtime verification'})

    # Boundary values are bytes, not Unicode code point counts or an aggregate cap.
    limit=lambda c,o:len(c)<=2048 and len(o)<=2048
    assert limit(b'x'*2048,b'y'*2048)
    assert not limit(b'x'*2049,b'y')
    assert not limit(b'x',b'y'*2049)
    assert limit(('あ'*682).encode(),b'y')
    assert not limit(('あ'*683).encode(),b'y')
    passed('completion_link_byte_boundary_examples',5)
    result={'scope':'design_contract_validation_only','runtime_tests_executed':False,'checks':results,'limitations':['No product Go compilation','No real LLM retries or tokenization','No real HMAC issuer or CLI intake','No OS, crash, or CORE production verification']}
    _path('CONTRACT_VALIDATION.json').write_text(json.dumps(result,ensure_ascii=False,indent=2)+'\n')
    return result
if __name__=='__main__':
    try: print(json.dumps(main(),ensure_ascii=False,indent=2))
    except Exception as exc:
        print('CONTRACT CHECK FAILED: '+str(exc),file=sys.stderr);raise
