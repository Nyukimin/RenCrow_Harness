#!/usr/bin/env python3
"""Check design encodings, input datasets, event shapes and stream fixtures.

This is not a product, a tokenizer, or a Runtime E2E test. Expected byte strings
and fixture hashes are checked here; the product Go implementation must pass
these vectors independently. No network, tool execution, or production DB I/O.
"""
from __future__ import annotations
import copy
import hashlib
import json
import re
import struct
import sys
from pathlib import Path
from typing import Any

from jsonschema import Draft202012Validator
import contract_codec as c
import projection_reference as p

from layout import path as _path
W=_path('examples/wire')
checks=[]

def load(path: str) -> Any:
    return json.loads(_path(path).read_text(encoding='utf-8'))

def wire(name: str) -> Any:
    return load('examples/wire/'+name)

def ok(name: str, detail: Any) -> None:
    checks.append({'check':name,'status':'passed','detail':detail})

def reject(fn, *args) -> None:
    try: fn(*args)
    except (ValueError, TypeError, UnicodeError, AssertionError, RecursionError): return
    raise AssertionError('expected rejection: '+getattr(fn,'__name__',repr(fn)))

def schema_validator(file: str, definition: str | None=None):
    s=load('schemas/'+file+'.schema.json')
    Draft202012Validator.check_schema(s)
    root=s if definition is None else {'$schema':s.get('$schema'),'$defs':s['$defs'],'$ref':'#/$defs/'+definition}
    return Draft202012Validator(root)

def validates(file: str, definition: str | None, value: Any):
    errors=list(schema_validator(file,definition).iter_errors(value))
    assert not errors, f'{file}:{definition}: '+ '; '.join(e.message for e in errors[:3])

def rejects_schema(file: str, definition: str | None, value: Any):
    assert list(schema_validator(file,definition).iter_errors(value)),f'unexpected schema acceptance {file}:{definition}'

def separate_d(domain: str,*value_bytes: bytes)->str:
    # Independent assembly against stored canonical bytes, not codec.digest.
    domain_b=domain.encode('utf-8')
    result=struct.pack('>Q',len(domain_b))+domain_b
    for b in value_bytes: result+=struct.pack('>Q',len(b))+b
    return hashlib.sha256(result).hexdigest()

def check_canonical():
    vectors=wire('canonical_vectors.json')
    for v in vectors['positive']:
        raw=c.cj(c.loads(v['input_json']))
        expected=v['expected_cj_utf8'].encode('utf-8')
        assert raw==expected==bytes.fromhex(v['expected_cj_hex']),v['name']
        assert hashlib.sha256(expected).hexdigest()==v['expected_sha256']
        assert c.cj(c.loads(raw))==raw
    for s in vectors['negative_json']: reject(c.loads,s)
    for s in [b'"\xff"', b'{}\xff', '['*66+'0'+']'*66, '9'*1025, '1e4096']:
        reject(c.loads,s)
    assert c.cj(c.loads('1'))==c.cj(c.loads('1.0'))==c.cj(c.loads('1e0'))==b'1'
    assert c.cj(c.loads('1e-4094'))==b'0.'+b'0'*4093+b'1'
    reject(c.loads,'1e-4095')  # normal form would exceed 4096 bytes
    assert c.cj({'x':None})!=c.cj({})!=c.cj({'x':[]})
    ok('cj1_exact_bytes_and_negative_inputs',{'positive_vectors':len(vectors['positive']),'negative_cases':len(vectors['negative_json'])+6})

def check_requests():
    chat=wire('generation_request.json');normalized=wire('normalized_request.json');v=wire('request_vector.json')
    validates('llm_contract','GenerationRequest',chat)
    validates('llm_contract','NormalizedRequest',normalized)
    validates('measure',None,wire('measure_result.json'))
    assert c.cj(c.logical_input(chat)).hex()==v['logical_input_cj_hex']
    assert c.cj(normalized).hex()==v['normalized_cj_hex']
    assert separate_d('rencrow-model-input/v1',bytes.fromhex(v['logical_input_cj_hex']))==v['input_digest']==c.input_digest(chat)
    assert separate_d('rencrow-model-request/v1',bytes.fromhex(v['normalized_cj_hex']))==v['request_digest']==c.final_request_digest(normalized)
    assert 'bfp-v1:'+separate_d('rencrow-binding/v1',c.cj(v['identity_descriptor']))==v['binding_fingerprint']
    assert v['input_digest']!=v['request_digest']
    for field in ['request_id','trace_id','task_id','session_id','initiator','caller','purpose']:
        changed=copy.deepcopy(chat);changed['rencrow'][field]='different';assert c.input_digest(changed)==v['input_digest']
    for field in ['retry_of_request_id','trigger_code']:
        changed=copy.deepcopy(chat);changed['rencrow']['harness']['recovery'][field]='different';assert c.input_digest(changed)==v['input_digest']
    for field,value in [('temperature',0),('max_tokens',2048),('stream',False),('stop',['STOP'])]:
        changed=copy.deepcopy(chat);changed[field]=value;assert c.input_digest(changed)!=v['input_digest']
    changed=copy.deepcopy(chat);changed['rencrow']['harness']['recovery']['profile_revision']='different';assert c.input_digest(changed)!=v['input_digest']
    changed=copy.deepcopy(chat);changed['messages'][0]['content']+=' ';assert c.input_digest(changed)!=v['input_digest']
    changed=copy.deepcopy(chat);changed['unexpected_option']=True;rejects_schema('llm_contract','GenerationRequest',changed)
    changed=copy.deepcopy(chat);changed['stream_options']=None;rejects_schema('llm_contract','GenerationRequest',changed)
    changed=copy.deepcopy(chat);changed['rencrow']['harness']['stage']='work_summary';rejects_schema('llm_contract','GenerationRequest',changed)
    changed=copy.deepcopy(normalized);changed['runtime_overrides']['thinking_enabled']=False;assert c.final_request_digest(changed)!=v['request_digest']
    ok('request_digest_ownership_and_field_selection',{'positive_request_sets':1,'metadata_exclusions':9,'included_mutations':7,'schema_rejections':3,'final_input_owned_by':'RenCrow_LLM'})

def check_mutation_and_caller():
    vs=wire('mutation_vectors.json')
    if isinstance(vs,dict):vs=vs.get('vectors',vs.get('cases'))
    for v in vs:
        params=v['params'];hash_key='expected_hash'
        expected=v[hash_key]
        clear={k:x for k,x in params.items() if k!='idempotency_key'}
        got=separate_d('rencrow-mutation/v1',c.cj(v['principal']),c.cj(v['method']),c.cj(clear))
        assert expected==got==c.mutation_digest(v['principal'],v['method'],params)
        changed=copy.deepcopy(params);changed['idempotency_key']='another-key';assert c.mutation_digest(v['principal'],v['method'],changed)==expected
        assert c.mutation_digest('core:other',v['method'],params)!=expected
    cv=wire('caller_vector.json');assert c.caller_digest(cv['caller'])==cv['expected_digest']
    changed=copy.deepcopy(cv['caller']);changed['readable_session_owners'].reverse();assert c.caller_digest(changed)==cv['expected_digest']
    principal=schema_validator('protocol','Principal')
    for x in ['user:ren','core:local','service:worker_01']:assert principal.is_valid(x)
    for x in ['ren',' user:ren','user:ren ','User:ren','user:れん','user:','user:../x']:assert not principal.is_valid(x)
    key=(W/'synthetic_origin_key.hex').read_bytes()
    def decode_key(b):
        assert re.fullmatch(rb'[0-9a-f]{64}\n?',b)
        return bytes.fromhex(b.rstrip(b'\n').decode())
    assert decode_key(key)==bytes(range(32))
    for bad in [key.rstrip(b'\n')+b'\r\n',key.upper(),b' '+key,key+b'\n',bytes(range(32)),b'00'*31,b'\xef\xbb\xbf'+key]:reject(decode_key,bad)
    ok('native_mutation_and_caller_key_contract',{'mutation_vectors':len(vs),'principal_positive':3,'principal_negative':7,'keyfile_negative':7})

def validate_dataset(d):
    validates('stage_data',None,d)
    selection=d['format_version']=='rencrow-selection-data/v1'
    groups=[('presented_sources','presented')] if selection else [('work','work'),('instructions','instruction')]
    for name,prefix in groups:
        for i,piece in enumerate(d[name]):
            assert piece['handle']==f'{prefix}-{i}'
            assert len(piece['text'].encode('utf-8'))<=8000
            assert 0<=piece['chunk_index']<piece['chunk_count']
    if selection:
        valid={p['handle'] for p in d['presented_sources']}
        for i,link in enumerate(d['completion_links']):
            assert link['handle']==f'completion-{i}' and link['target_handle'] in valid
            assert len(link['call_text'].encode())<=2048 and len(link['output_text'].encode())<=2048
    else:
        for i,o in enumerate(d['observations']):
            assert o['handle']==f'observation-{i}'
            end=0;coverage=0
            for part in o['excerpts']:
                a,b=part['range']['start'],part['range']['end']
                assert 0<=a<=b<=o['total_bytes'] and a>=end
                assert len(part['text'].encode())==b-a
                end=b;coverage+=b-a
            assert coverage<=2048
            if not o['partial']:assert coverage==o['total_bytes'] and o['capture_complete']
        assert len(d['prior_summaries'])<=1
        if d['prior_summaries']:assert d['prior_summaries'][0]['handle']=='summary-0'
        valid={p['handle'] for p in d['instructions']}
        for i,x in enumerate(d['completions']):
            assert x['handle']==f'completion-{i}'
            assert set(x['instruction_handles'])<=valid
            assert len(x['call_text'].encode())<=2048 and len(x['output_text'].encode())<=2048
    assert len(c.cj(d))<=8*1024*1024

def check_stages():
    for f in ['selection_dataset.json','summary_dataset.json']:validate_dataset(wire(f))
    sm=wire('stage_messages.json')
    for name,data,prompt in [('instruction_selection','selection_dataset.json','instruction_selection.md'),('work_summary','summary_dataset.json','work_summary.md')]:
        assert sm[name]==[{'role':'system','content':(_path('prompts/'+prompt)).read_text()},{'role':'user','content':'RENCROW_STAGE_DATA_V1\n'+c.cj(wire(data)).decode()}]
    vs=wire('split_vectors.json');vs=vs if isinstance(vs,list) else vs.get('vectors',vs.get('cases'))
    for v in vs:
        text=v['input'];parts=c.split_utf8(text)
        assert ''.join(parts).encode()==text.encode()
        lens=[len(x.encode())for x in parts]
        assert all(n<=8000 for n in lens)
        assert lens==v.get('expected_byte_lengths',v.get('chunk_byte_lengths',v.get('expected_chunk_bytes')))
        for i,part in enumerate(parts[:-1]):assert len(part.encode())+len(parts[i+1][0].encode())>8000
    base=wire('selection_dataset.json')
    for mode in ['namespace','gap','origin','byte_oversize','completion_length']:
        bad=copy.deepcopy(base)
        if mode=='namespace':bad['presented_sources'][0]['handle']='work-0'
        if mode=='gap':bad['presented_sources'][0]['handle']='presented-8'
        if mode=='origin':bad['presented_sources'][0]['origin']='unknown'
        if mode=='byte_oversize':bad['presented_sources'][0]['text']='あ'*2667
        if mode=='completion_length':
            if not bad['completion_links']:
                bad['completion_links']=[{'handle':'completion-0','target_handle':'presented-0','call_text':'x','output_text':'x','exit_code':0}]
            bad['completion_links'][0]['call_text']='x'*2049
        reject(validate_dataset,bad)
    ok('stage_shapes_utf8_splits_and_no_tools_messages',{'datasets':2,'split_vectors':len(vs),'negative_cases':5})

def check_projection_candidate():
    proj=wire('projection_input.json');gold=wire('projection_golden.json')
    actual=p.render(proj)
    assert actual==gold
    for msg in actual:validates('llm_contract','ChatMessage',msg)
    assert not any('source_handles' in x.get('content','') for x in actual if (x.get('content') or '').startswith('RENCROW_ACCEPTED_SUMMARY_V1'))
    coalesced=p.strict_prefix([{'role':'system','content':'  S\n'},{'role':'developer','content':'D  '},{'role':'user','content':'U  '}])
    assert coalesced==[{'role':'system','content':'  S\n\n\nD  '},{'role':'user','content':'U  '}]
    reject(p.strict_prefix,[{'role':'user','content':'u'},{'role':'system','content':'s'}])
    original=wire('projection_input.json');before=copy.deepcopy(original)
    after,changes=p.emergency_projection(original)
    assert original==before and after==wire('emergency_projection.json') and p.render(after)==wire('emergency_golden.json') and changes==wire('emergency_changes.json')
    assert after['summary']==original['summary'] and after['summary_anchor_sequence']==original['summary_anchor_sequence']
    assert all(x['after_bytes']<x['before_bytes'] for x in changes)
    # Marker-size comparison is a byte prefilter only; no actual token claim.
    candidate=wire('checkpoint_candidate.json');validates('checkpoint','CheckpointCandidate',candidate)
    b=c.candidate_bytes(candidate);assert b==(W/'checkpoint_candidate.bin').read_bytes()
    v=wire('checkpoint_vector.json');assert len(b)==v['candidate_length'] and hashlib.sha256(b).hexdigest()==v['candidate_sha256']
    prefix=b'rencrow-checkpoint-candidate/v1\0'
    def valid_bytes(raw):
        assert raw.startswith(prefix)
        obj=c.loads(raw[len(prefix):]);assert prefix+c.cj(obj)==raw
    valid_bytes(b)
    for raw in [b+b'\n',b'\xef\xbb\xbf'+b,prefix+b' '+b[len(prefix):],b.replace(prefix,b'rencrow-checkpoint-candidate/v2\0',1)]:reject(valid_bytes,raw)
    changed=copy.deepcopy(candidate);changed['expected']['control_revision']+=1;assert c.candidate_hash(changed)!=v['candidate_sha256']
    observed=[s['reference'] for e in after['entries'] for s in e['observations'] if s['reference']['presented_ranges']==[]]
    assert observed and c.ref_marker(observed[0]).encode()==(W/'observation_marker.txt').read_bytes()
    ok('context_projection_emergency_and_checkpoint_bytes',{'projection_messages':len(actual),'emergency_changes':len(changes),'noncanonical_checkpoint_rejections':4,'actual_tokenization':False})

def classify_fixture(source: str, proof: str):
    # Explicit fixture-only reference for ERROR_MAPPING evidence branches.
    states={'not_dispatched':'not_started','normalizer_defect':'not_started','terminal':'terminal','terminal_schema_violation':'terminal','runtime_stopped':'terminal','none':'unknown'}
    state=states[proof]
    if state=='unknown':return 'MODEL_GENERATION_OUTCOME_UNKNOWN',state,False
    if source=='CAPACITY_EXCEEDED':return 'QUEUE_TIMEOUT',state,state=='not_started'
    if source=='TARGET_UNAVAILABLE':return ('CONNECT_FAILED' if state=='not_started' else 'UPSTREAM_TRANSIENT'),state,True
    if source=='TARGET_FIRST_OUTPUT_TIMEOUT':return 'UPSTREAM_TRANSIENT',state,state=='terminal'
    if source=='DEGENERATE_OUTPUT':return 'MODEL_OUTPUT_DEGENERATE',state,False
    if source=='NORMALIZATION_ERROR':return ('MODEL_OUTPUT_SCHEMA_INVALID',state,True) if proof=='terminal_schema_violation' else ('MODEL_CONTRACT_FAILED',state,False)
    if source=='MODEL_NOT_ALIVE':return 'MODEL_UNAVAILABLE',state,False
    if source in ['REASONING_ONLY','RAW_TOOL_MARKUP','EMPTY_FINAL_CONTENT','MODEL_OUTPUT_SCHEMA_INVALID']:return source,state,state=='terminal'
    return 'MODEL_CONTRACT_FAILED',state,False

def check_errors():
    vs=wire('error_cases.json')['cases']
    for v in vs:
        assert classify_fixture(v['source_code'],v['proof'])==(v['expected_code'],v['expected_generation_state'],v['expected_retry'])
    rules=load('contracts/error_mapping.json')['rules']
    assert len({r['source_code']for r in rules})==len(rules)
    expected={'CAPACITY_EXCEEDED','TARGET_UNAVAILABLE','NORMALIZATION_ERROR','MODEL_NOT_ALIVE','TARGET_FIRST_OUTPUT_TIMEOUT','DEGENERATE_OUTPUT','EMPTY_FINAL_CONTENT','REASONING_ONLY','RAW_TOOL_MARKUP','BINDING_CHANGED'}
    assert expected<={r['source_code']for r in rules}
    receipt=wire('stream_terminal.json')['harness_receipt']
    for state,num in [('not_started',0),('terminal',1),('unknown',None)]:
        r=copy.deepcopy(receipt);r.update(generation_state=state,backend_attempts=num);validates('llm_contract','GatewayAttemptReceipt',r)
        r['backend_attempts']=1 if num!=1 else 0;rejects_schema('llm_contract','GatewayAttemptReceipt',r)
    ok('gateway_error_evidence_and_receipt_state',{'error_cases':len(vs),'source_codes':len(rules),'receipt_positive':3,'receipt_negative':3})

def parse_sse_fixture(raw: str, request: dict):
    """No network or Tool execution. Returns committed tool intents only at DONE."""
    raw=raw.replace('\r\n','\n')
    assert raw.endswith('\n\n'),'incomplete frame'
    terminal=None;done=False;response_id=None;finish=None;calls=[];content='';audit=[]
    for frame in raw.split('\n\n'):
        if not frame:continue
        lines=frame.split('\n');event='message';data=[]
        for line in lines:
            if line.startswith(':'):continue
            if line.startswith('event:'):event=line[6:].lstrip(' ')
            elif line.startswith('data:'):data.append(line[5:].removeprefix(' '))
        if not data:continue
        payload='\n'.join(data);assert not done,'data after DONE'
        if payload=='[DONE]':assert terminal is not None;done=True;continue
        assert terminal is None,'data after terminal'
        c.loads(payload);obj=json.loads(payload)
        if event=='rencrow.terminal':
            validates('llm_contract','StreamTerminal',obj);terminal=obj
            h=request['rencrow']['harness'];r=obj['harness_receipt']
            for key in ['input_digest','request_digest','binding_fingerprint']:assert r[key]==h['expected_'+key]
            assert r['stage']==h['stage']
            assert r['recovery_profile']==h['recovery']['profile_id'] and r['recovery_profile_revision']==h['recovery']['profile_revision']
            assert obj['provider_response_id']==response_id and obj['finish_reason']==finish
        else:
            assert event=='message'
            rid=obj.get('id');assert rid and (response_id is None or rid==response_id);response_id=rid
            choices=obj['choices']
            if not choices:assert finish is not None and 'usage' in obj;continue
            assert len(choices)==1 and choices[0]['index']==0
            assert finish is None,'multiple finish chunks'
            ch=choices[0];delta=ch.get('delta',{})
            assert set(delta)<={'role','content','reasoning_content','tool_calls'}
            if 'role' in delta:assert delta['role']=='assistant'
            if delta.get('content') is not None:content+=delta['content']
            for x in delta.get('tool_calls',[]):
                i=x['index'];assert 0<=i<=len(calls)
                if i==len(calls):
                    assert x.get('id') and x.get('type')=='function' and x.get('function',{}).get('name')
                    calls.append({'id':x['id'],'type':'function','function':{'name':x['function']['name'],'arguments':''}})
                call=calls[i]
                for k in ['id','type']:
                    if k in x:assert x[k]==call[k]
                if 'name' in x.get('function',{}):assert x['function']['name']==call['function']['name']
                if 'arguments' in x.get('function',{}):call['function']['arguments']+=x['function']['arguments']
            if ch.get('finish_reason') is not None:finish=ch['finish_reason']
        audit.append({'frame_seen':len(audit)+1,'executable_tools':0})
    assert done and terminal is not None,'missing strict end'
    if terminal['outcome']!='completed':return [],audit
    assert terminal['harness_receipt']['generation_state']=='terminal'
    assert finish in ['tool_calls','stop']
    if calls:assert finish=='tool_calls'
    else:assert finish=='stop' and content.strip()
    definitions={t['function']['name']:t['function']['parameters']for t in request['tools']}
    assert len({x['id']for x in calls})==len(calls)
    for call in calls:
        assert request['tool_choice']!='none'
        args=c.loads(call['function']['arguments']);args=json.loads(call['function']['arguments'])
        name=call['function']['name'];assert name in definitions
        assert Draft202012Validator(definitions[name]).is_valid(args)
    return calls,audit

def check_stream():
    raw=(W/'act_tool_stream.sse').read_text();req=wire('stream_request.json')
    validates('llm_contract','GenerationRequest',req)
    assert c.input_digest(req)==req['rencrow']['harness']['expected_input_digest']
    assert c.final_request_digest(wire('stream_normalized_request.json'))==req['rencrow']['harness']['expected_request_digest']
    calls,audit=parse_sse_fixture(raw,req);assert len(calls)==1 and all(x['executable_tools']==0 for x in audit)
    assert calls[0]['function']['arguments']=='{"path":"demo.txt"}'
    assert parse_sse_fixture(raw.replace('\n','\r\n'),req)[0]==calls
    for bad in [raw.replace('data: [DONE]\n\n',''), raw[:raw.index('event: rencrow.terminal')], raw+raw, raw.replace('"index":0','"index":1',1),raw.replace('fixture-read-1','',1)]:reject(parse_sse_fixture,bad,req)
    badreq=copy.deepcopy(req);badreq['tools']=[];badreq['tool_choice']='none';reject(parse_sse_fixture,raw,badreq)
    bad=raw.replace('"recovery_profile_revision":"builtin-v1"','"recovery_profile_revision":"different"');reject(parse_sse_fixture,bad,req)
    # Valid JSON arguments but length terminal must return zero executable calls.
    term=wire('stream_terminal.json');term.update(outcome='incomplete',finish_reason='length',code='LENGTH')
    prefix=raw[:raw.index('event: rencrow.terminal')].replace('"finish_reason":"tool_calls"','"finish_reason":"length"')
    cut=prefix+'event: rencrow.terminal\ndata: '+c.cj(term).decode()+'\n\ndata: [DONE]\n\n'
    assert parse_sse_fixture(cut,req)[0]==[]
    ok('strict_stream_argument_assembly_and_terminal_gate',{'positive_streams':2,'negative_streams':7,'length_rejects_all_tools':True,'tool_execution_performed':False})

def check_events_assets():
    es=wire('event_payloads.json')['events']
    expected=load('contracts/event_types.json')['types']
    assert {e['type']for e in es}==set(expected) and len(es)==15
    for e in es:
        validates('protocol','Event',e);validates('event',None,e)
        assert json.loads(c.cj(e['payload']))==e['payload']
        bad=copy.deepcopy(e);bad['payload']['unknown_field']=True;rejects_schema('protocol','Event',bad)
        bad=copy.deepcopy(e);bad['payload'].pop(next(iter(bad['payload'])));rejects_schema('protocol','Event',bad)
        for k in ['thread_id','run_id','task_id','receipt_id','message_id']:
            if k in e['payload']:assert e['payload'][k]==e[k]
    validates('policy_registry',None,load('examples/policies.json'))
    bad=load('examples/policies.json');bad['unknown_field']=True;rejects_schema('policy_registry',None,bad)
    a=wire('host_assets.json')
    for k,definition in [('skill','SkillMetadata'),('hook_input','HookInput'),('hook_result','HookResult')]:validates('host_assets',definition,a[k])
    bad=copy.deepcopy(a['skill']);bad['name']='bad--name';rejects_schema('host_assets','SkillMetadata',bad)
    bad=copy.deepcopy(a['hook_result']);bad['command']='sh';rejects_schema('host_assets','HookResult',bad)
    bad=copy.deepcopy(a['hook_result']);bad.update(decision='deny',code=None);rejects_schema('host_assets','HookResult',bad)
    ok('all_event_payloads_and_host_assets',{'event_types':15,'event_schema_negative':30,'asset_positive':4,'asset_negative':4})

def main():
    for f in [check_canonical,check_requests,check_mutation_and_caller,check_stages,check_projection_candidate,check_errors,check_stream,check_events_assets]:f()
    report={'scope':'wire_and_projection_design_asset_checks','version':'0.2.2','runtime_implemented':False,'runtime_tests_executed':False,'checks':checks,'limitations':['No product Go build or network execution','No real tokenizer/LLM generation or performance measurement','No actual CORE routing, OS isolation, durability or production deployment','Synthetic expected vectors check byte/projection contracts, not semantic correctness or empirical recovery rates']}
    _path('WIRE_CONTRACT_VALIDATION.json').write_text(json.dumps(report,ensure_ascii=False,indent=2)+'\n')
    print(json.dumps(report,ensure_ascii=False,indent=2))

if __name__=='__main__':main()
