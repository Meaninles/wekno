"""Local task reproduction; record complete evidence for human review, not prose grading."""
import argparse
import json
from pathlib import Path
import sys
import time

REPO=Path(__file__).resolve().parents[3]
sys.path.insert(0,str(REPO/'custom/services/agent-eval'))
from weknora_eval.client import WeKnoraClient,event_type,streamed_production_candidate


def main():
    p=argparse.ArgumentParser();p.add_argument('case');p.add_argument('model');p.add_argument('query');p.add_argument('--web',action='store_true')
    p.add_argument('--agent',default='builtin-document-processing');a=p.parse_args()
    env=dict(line.split('=',1) for line in (REPO/'custom/services/agent-eval/runner.env').read_text(encoding='utf-8-sig').splitlines() if '=' in line and not line.startswith('#'))
    client=WeKnoraClient('http://localhost:8080/api/v1',env['WEKNORA_E2E_TENANT_API_KEY'].strip().strip('"'),timeout=900)
    root=REPO/'.local-data/gateway-policy/tasks';root.mkdir(parents=True,exist_ok=True)
    session=client.create_session();started=time.monotonic()
    print(json.dumps({'case':a.case,'session':session,'model':a.model},ensure_ascii=False),flush=True)
    payload={'query':a.query,'knowledge_base_ids':[],'knowledge_ids':[],'agent_id':a.agent,'agent_enabled':True,
        'web_search_enabled':a.web,'summary_model_id':a.model,'channel':'web'}
    def record(event):
        with (root/(a.case+'.jsonl')).open('a',encoding='utf-8') as stream:
            stream.write(json.dumps({'seconds':round(time.monotonic()-started,3),'event':event},ensure_ascii=False)+'\n')
    result={'case':a.case,'session':session,'model':a.model,'agent':a.agent,'query':a.query}
    try:
        events,ttfb,total=client.stream('/agent-chat/'+session,payload,on_event=record)
        result.update(seconds=round(total/1000,3),first_event_ms=ttfb,answer=streamed_production_candidate(events))
        result['messages']=client.request('GET',f'/messages/{session}/load?limit=100')
    except Exception as exc:result.update(error=str(exc)[:1000],seconds=round(time.monotonic()-started,3))
    (root/(a.case+'.json')).write_text(json.dumps(result,ensure_ascii=False,indent=2),encoding='utf-8')
    print(json.dumps({k:v for k,v in result.items() if k!='messages'},ensure_ascii=False),flush=True)


if __name__=='__main__':main()
