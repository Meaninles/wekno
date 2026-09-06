"""Additional local end-to-end observations; no implementation/config changes.

All answers and tool traces are retained for manual review, without semantic
pass/fail scoring. New sessions use the existing eval account and agents.
"""
import base64
import json
from pathlib import Path
import runpy
import sys
import time

REPO=Path(__file__).resolve().parents[3]
AUDIT=REPO/'.local-data/agent-audit-20260905'
OUT=AUDIT/'plan-validation';OUT.mkdir(exist_ok=True)
sys.argv=['live_probe.py','inspect']
P=runpy.run_path(str(AUDIT/'live_probe.py'));C=P['C']
P['run'].__globals__['ROOT']=OUT

def main():
    fixture=json.loads((AUDIT/'fix-extended-resources.json').read_text(encoding='utf-8'))
    kb=fixture['kb']['data']['id']
    hybrid=fixture['agents']['hybrid-rag-wiki']['id']
    P['run']('pv-hybrid',hybrid,[
        '白鹭培训预算是否已经确认？请说明依据。',
        '培训现在的时间、地点和联系人是什么？',
        '投影仪用完以后应该交到哪里？',
    ],[kb])
    mysql=json.loads((AUDIT/'probe-fix-v5-mysql.json').read_text(encoding='utf-8'))[0]
    for repeat in range(2):
        P['run'](f'pv-mysql-{repeat+1}',mysql['agent'],[mysql['query']])
    doc=OUT/'document-conversion.json'
    original=AUDIT/'remaining/safety-original.docx'
    session=C.create_session();timeline=[];start=time.perf_counter()
    query='请把附件转换成可下载的 PDF，保留全部正文、表格和图片的内容及原有顺序。'
    payload={'query':query,'agent_id':'builtin-document-processing','agent_enabled':True,'web_search_enabled':False,'summary_model_id':P['MODEL'],'disable_title':True,'channel':'web','knowledge_base_ids':[],'knowledge_ids':[],'attachment_uploads':[{'file_name':'安全生产教育培训管理规定.docx','file_size':original.stat().st_size,'data':base64.b64encode(original.read_bytes()).decode()}]}
    def event(e):
        timeline.append({'t_s':time.perf_counter()-start,'event':e})
        with (OUT/'document-events.jsonl').open('a',encoding='utf-8') as f:f.write(json.dumps(timeline[-1],ensure_ascii=False)+'\n')
    print(json.dumps({'start':'pv-document','session_id':session},ensure_ascii=False),flush=True)
    events,ttfb,total=C.stream(f'/agent-chat/{session}',payload,on_event=event)
    time.sleep(1)
    messages=C.request('GET',f'/messages/{session}/load?limit=100').get('data') or []
    row={'session_id':session,'query':query,'total_s':total/1000,'first_event_s':ttfb/1000,'answer':P['streamed_production_candidate'](events),'messages':messages,'timeline':timeline}
    doc.write_text(json.dumps(row,ensure_ascii=False,indent=2),encoding='utf-8')
    print(json.dumps({k:v for k,v in row.items() if k not in ['messages','timeline']},ensure_ascii=False),flush=True)

if __name__=='__main__':main()
