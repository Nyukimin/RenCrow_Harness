#!/usr/bin/env python3
"""Reference byte-contract encoder. Not a production Harness or a tokenizer.

No float-based number conversion. Input numeric lexemes are parsed as Decimal.
The normative text is BYTE_CONTRACTS.md. Product code must independently pass
these vectors; importing this Python module is not a product dependency.
"""
from __future__ import annotations
import hashlib, json, re, struct
from decimal import Decimal
from typing import Any

MAX_NUMBER_BYTES=4096
MAX_NUMBER_LEXEME=1024
MAX_DEPTH=64

def lp(raw: bytes | str) -> bytes:
    if isinstance(raw,str): raw=raw.encode('utf-8','strict')
    return struct.pack('>Q',len(raw))+raw

def _pairs(pairs):
    d={}
    for k,v in pairs:
        if k in d: raise ValueError('duplicate JSON key')
        d[k]=v
    return d

def _number(s):
    if len(s)>MAX_NUMBER_LEXEME: raise ValueError('number lexeme too long')
    if not re.fullmatch(r'-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?',s):
        raise ValueError('invalid JSON number')
    exponent=s.lower().split('e',1)[1] if 'e' in s.lower() else '0'
    if abs(int(exponent))>MAX_NUMBER_BYTES: raise ValueError('number exponent too large')
    v=Decimal(s)
    # Limit before expansion, including zero with a hostile exponent.
    if abs(v.as_tuple().exponent)>MAX_NUMBER_BYTES: raise ValueError('number exponent too large')
    return v

def loads(raw: bytes | str) -> Any:
    if isinstance(raw,bytes): raw=raw.decode('utf-8','strict')
    if raw.startswith('\ufeff'): raise ValueError('BOM forbidden')
    def reject(s): raise ValueError('nonfinite number')
    obj=json.loads(raw,object_pairs_hook=_pairs,parse_int=_number,parse_float=_number,parse_constant=reject)
    cj(obj) # validate scalars/depth, even after escaped input
    return obj

def number_bytes(v: Decimal | int) -> bytes:
    if isinstance(v,bool) or isinstance(v,float): raise TypeError('not an exact decimal')
    v=Decimal(v) if isinstance(v,int) else v
    if not v.is_finite(): raise ValueError('nonfinite number')
    if abs(v.as_tuple().exponent)>MAX_NUMBER_BYTES: raise ValueError('number exponent too large')
    if v.is_zero(): return b'0'
    s=format(v,'f')
    if '.' in s: s=s.rstrip('0').rstrip('.')
    if len(s.encode('ascii'))>MAX_NUMBER_BYTES: raise ValueError('normalized number too long')
    return s.encode('ascii')

def string_bytes(s: str) -> bytes:
    s.encode('utf-8','strict')
    out=bytearray(b'"')
    for c in s:
        n=ord(c)
        if c=='"': out.extend(b'\\"')
        elif c=='\\': out.extend(b'\\\\')
        elif n<32: out.extend(('\\u%04x'%n).encode('ascii'))
        else: out.extend(c.encode('utf-8'))
    return bytes(out+b'"')

def cj(v: Any, depth=0) -> bytes:
    if depth>MAX_DEPTH: raise ValueError('JSON depth limit')
    if v is None:return b'null'
    if v is True:return b'true'
    if v is False:return b'false'
    if isinstance(v,str):return string_bytes(v)
    if isinstance(v,(int,Decimal)) and not isinstance(v,bool):return number_bytes(v)
    if isinstance(v,float): raise TypeError('load JSON numeric lexemes with Decimal, not float')
    if isinstance(v,list):return b'['+b','.join(cj(x,depth+1) for x in v)+b']'
    if isinstance(v,dict):
        if not all(isinstance(k,str) for k in v):raise TypeError('JSON keys must be strings')
        keys=sorted(v,key=lambda k:k.encode('utf-8','strict'))
        return b'{'+b','.join(string_bytes(k)+b':'+cj(v[k],depth+1) for k in keys)+b'}'
    raise TypeError('unsupported canonical type: '+type(v).__name__)

def digest(domain: str, *values: Any) -> str:
    return hashlib.sha256(lp(domain)+b''.join(lp(cj(v)) for v in values)).hexdigest()

def logical_input(chat: dict) -> dict:
    r=chat['rencrow']; h=r['harness']; rec=h['recovery']
    return {
        'model':chat['model'],'messages':chat['messages'],'tools':chat['tools'],
        'tool_choice':chat['tool_choice'],'response_format':chat['response_format'],
        'stream':chat['stream'],'stream_options':chat['stream_options'],
        'max_tokens':chat['max_tokens'],'temperature':chat['temperature'],
        'top_p':chat['top_p'],'seed':chat['seed'],'stop':chat['stop'],
        'routing':{k:r[k] for k in ['agent_id','execution_role','execution_alias']},
        'harness':{'contract_version':h['contract_version'],'stage':h['stage'],
          'max_backend_attempts':h['max_backend_attempts'],
          'recovery':{k:rec[k] for k in ['profile_id','profile_revision']}}
    }

def input_digest(chat: dict) -> str:
    return digest('rencrow-model-input/v1',logical_input(chat))

def final_request_digest(normalized: dict) -> str:
    return digest('rencrow-model-request/v1',normalized)

def mutation_digest(principal: str, method: str, params: dict) -> str:
    clean={k:v for k,v in params.items() if k!='idempotency_key'}
    return digest('rencrow-mutation/v1',principal,method,clean)

def candidate_bytes(candidate: dict) -> bytes:
    return b'rencrow-checkpoint-candidate/v1\0'+cj(candidate)

def candidate_hash(candidate: dict) -> str:
    return hashlib.sha256(candidate_bytes(candidate)).hexdigest()

def caller_digest(caller: dict) -> str:
    relays=[{k:r[k] for k in ['issuer','key_id','audience']} for r in caller['relay_issuers']]
    relays.sort(key=lambda r:(r['issuer'].encode(),r['key_id'].encode(),r['audience'].encode()))
    value={ 'principal':caller['principal'], 'default_origin':caller['default_origin'],
      'readable_session_owners':sorted(caller['readable_session_owners'],key=lambda s:s.encode()),
      'controllable_session_owners':sorted(caller['controllable_session_owners'],key=lambda s:s.encode()),
      'relay_issuers':relays}
    return digest('rencrow-caller-profile/v1',value)

def split_utf8(text: str, limit: int=8000) -> list[str]:
    if limit<4: raise ValueError('limit must accommodate a scalar')
    b=text.encode('utf-8','strict');out=[]
    while b:
        n=min(limit,len(b))
        while n<len(b) and b[n]&0xc0==0x80:n-=1
        out.append(b[:n].decode('utf-8'));b=b[n:]
    return out or ['']

def ref_marker(record: dict) -> str:
    return 'RENCROW_OBSERVATION_REFERENCE_V1\n'+cj(record).decode('utf-8')

if __name__=='__main__':
    import sys
    sys.stdout.buffer.write(cj(loads(sys.stdin.buffer.read()))+b'\n')
