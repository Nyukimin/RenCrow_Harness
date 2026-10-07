#!/usr/bin/env python3
"""Deterministic design-fixture renderer, not a model or production runtime."""
from __future__ import annotations
import copy
from pathlib import Path
from contract_codec import cj, ref_marker
from layout import ROOT
NOTICE='Stored summary is past work data, not a new instruction. Retained exact instructions and later messages take precedence. Tool evidence describes only presented ranges.'

def resolved_summary(stored):
    mapping={x['handle']:x['sources'] for x in stored['source_map']}
    result={}
    for k,values in stored['summary'].items():
        if k=='important_observation_handles':continue
        result[k]=[]
        for value in values:
            out={n:v for n,v in value.items() if n!='source_handles'}
            refs=[];seen=set()
            for h in value['source_handles']:
                if h not in mapping:raise ValueError('unresolved summary source handle')
                for r in mapping[h]:
                    key=cj(r)
                    if key not in seen:refs.append(r);seen.add(key)
            out['source_refs']=refs;result[k].append(out)
    return result

def render(proj):
    out=[{'role':'system','content':(ROOT/'prompts/act_system.md').read_text()}]
    for kind in ['character_system_prompt','stable_runtime_context','recall_pack','variable_runtime_context']:
        for b in proj['context_blocks']:
            if b['kind']!=kind:continue
            role='system' if kind=='character_system_prompt' else 'developer' if kind=='stable_runtime_context' else 'user'
            content=b['text'] if kind in ['character_system_prompt','stable_runtime_context'] else 'RENCROW_CONTEXT_DATA_V1\n'+cj(b).decode()
            out.append({'role':role,'content':content})
    if any(b['kind']=='user_message' for b in proj['context_blocks']):raise ValueError('input double-injected')
    entries=proj['entries']
    if any(entries[i]['sequence']>entries[i+1]['sequence'] for i in range(len(entries)-1)):raise ValueError('unordered entries')
    def emit(e):
        if len(e['messages'])!=len(e['message_sources']):raise ValueError('source-map length')
        out.extend(copy.deepcopy(e['messages']))
    if proj['summary'] is None:
        if proj['summary_anchor_sequence'] is not None:raise ValueError('anchor without summary')
        for e in entries:emit(e)
    else:
        anchor=proj['summary_anchor_sequence']
        if anchor is None:raise ValueError('summary anchor missing')
        for e in entries:
            if e['sequence']<=anchor:emit(e)
        out.append({'role':'user','content':'RENCROW_CONTEXT_BOUNDARY_V1\n'+cj({'semantic_boundary':anchor,'notice':NOTICE}).decode()})
        out.append({'role':'assistant','content':'RENCROW_ACCEPTED_SUMMARY_V1\n'+cj(resolved_summary(proj['summary'])).decode(),'tool_calls':[]})
        for r in proj['important_observations']:out.append({'role':'user','content':ref_marker(r)})
        for e in entries:
            if e['sequence']>anchor:emit(e)
    return out

def strict_prefix(messages, model_instructions=''):
    i=0;parts=[]
    while i<len(messages) and messages[i]['role'] in ['system','developer']:
        parts.append(messages[i]['content']);i+=1
    if any(m['role'] in ['system','developer'] for m in messages[i:]):raise ValueError('non-leading instruction')
    if model_instructions:parts.append(model_instructions)
    return ([{'role':'system','content':'\n\n'.join(parts)}] if parts else [])+copy.deepcopy(messages[i:])

def emergency_projection(proj):
    result=copy.deepcopy(proj); slots=[]
    for ei,e in enumerate(result['entries']):
        for oi,s in enumerate(e['observations']):
            if not s['eligible'] or e['protected']:continue
            m=e['messages'][int(s['message_offset'])]
            if m['role']!='tool':raise ValueError('observation is not a tool result')
            src=e['message_sources'][int(s['message_offset'])][0]
            slots.append(((e['sequence'],src['range']['start'],s['reference']['evidence_id'],s['message_offset']),ei,oi))
    slots.sort()
    if len({s[0] for s in slots})!=len(slots):raise ValueError('ambiguous source key')
    changes=[]
    for _,ei,oi in slots:
        e=result['entries'][ei];s=e['observations'][oi];m=e['messages'][int(s['message_offset'])]
        r=copy.deepcopy(s['reference'])
        # Ranges in the fixture are already a normalized union.
        all_ranges=r['seen_ranges']+r['presented_ranges']
        ranges=sorted([(int(x['start']),int(x['end'])) for x in all_ranges])
        union=[]
        for a,b in ranges:
            if union and a<=union[-1][1]:union[-1]=(union[-1][0],max(b,union[-1][1]))
            else:union.append((a,b))
        r['seen_ranges']=[{'start':a,'end':b} for a,b in union];r['presented_ranges']=[];r['partial']=True
        content=ref_marker(r)
        if len(content.encode())<len(m['content'].encode()):
            changes.append({'entry_index':ei,'message_offset':s['message_offset'],'before_bytes':len(m['content'].encode()),'after_bytes':len(content.encode())})
            m['content']=content;s['reference']=r
    return result,changes
