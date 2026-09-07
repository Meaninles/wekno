"""Real cancellation, disconnected clients and worker/API replacement in dev."""
import json
import subprocess
import threading
import time

from dev_client import DevClient, ROOT, MODEL
from dev_probe import sql


def row(session):
    return next(iter(sql("SELECT row_to_json(t) FROM (SELECT id,message_id,status,owner_epoch,model_requests,reserved_tokens,error,result,checkpoint->>'boundary' AS boundary FROM custom_agent_runs WHERE session_id='"+session+"' ORDER BY created_at DESC LIMIT 1)t;")), None)


def wait_for(fn, timeout=180):
    end=time.monotonic()+timeout
    while time.monotonic()<end:
        value=fn()
        if value:return value
        time.sleep(.4)
    raise TimeoutError("Expected development run state was not reached")


def run_case(c, case):
    session=c.session('恢复测试 '+case)
    events=[];errors=[];started=time.monotonic()
    query=('请在工作区只执行一次这个操作：运行 Python 先等待 12 秒，再向 /workspace/outputs/recovery.txt 追加一行 RECOVERY-OK。然后读取此文件，确认只有一行并发布下载文件。'
           if case in ('worker-restart','api-restart') else
           '请详细说明计算机网络的分层结构，逐层给出职责、协议例子与故障排查方式。')
    def read():
        try:
            with c.client.stream('POST','/knowledge-chat/'+session,json={'query':query,'agent_id':'builtin-general-agent','summary_model_id':MODEL,'enable_memory':False,'web_search_enabled':False}) as r:
                r.raise_for_status()
                for line in r.iter_lines():
                    if line.startswith('data:') and line[5:].strip()!='[DONE]':
                        events.append(json.loads(line[5:]))
                        if case=='disconnect' and events:break
        except Exception as exc:
            errors.append(type(exc).__name__)
    thread=threading.Thread(target=read,daemon=True);thread.start()
    current=wait_for(lambda:row(session))
    if case=='cancel':
        wait_for(lambda:(r if r and r['reserved_tokens']>0 else None) if (r:=row(session)) else None)
        r=c.client.post('/sessions/'+session+'/stop',json={'message_id':current['message_id']});r.raise_for_status()
    elif case in ('worker-restart','api-restart'):
        wait_for(lambda:any(e.get('response_type')=='tool_call' and (e.get('data') or {}).get('tool_name')=='Bash' for e in events))
        wait_for(lambda: not sql("SELECT row_to_json(t) FROM (SELECT id FROM custom_agent_runs WHERE status IN ('queued','running') AND session_id <> '"+session+"')t;"))
        names=['weknora-agent-runtime'] if case=='worker-restart' else ['weknora-agent-eval-runtime-api-'+str(i) for i in (1,2,3)]
        subprocess.run(['docker','restart',*names],check=True,capture_output=True)
    current=wait_for(lambda:(r if r and r['status'] in ('completed','failed','cancelled') and r['reserved_tokens']==0 else None) if (r:=row(session)) else None,600)
    assert current['status']==('cancelled' if case=='cancel' else 'completed'),current
    replay=[]
    with c.client.stream('GET','/sessions/continue-stream/'+session,params={'message_id':current['message_id']}) as r:
        r.raise_for_status()
        for line in r.iter_lines():
            if line.startswith('data:') and line[5:].strip()!='[DONE]':replay.append(json.loads(line[5:]))
    if case!='cancel':
        complete=[e for e in replay if e.get('response_type')=='complete']
        assert len(complete)==1
        assert complete[0]['data']['final_answer']==current['result']['answer']
    if case=='worker-restart':assert current['owner_epoch']>=2
    if case in ('worker-restart','api-restart'):
        paths=c.artifacts({'run':current})
        assert paths
        for p in paths:
            if p['file'].endswith('recovery.txt'):
                from pathlib import Path
                assert Path(p['file']).read_text().strip().splitlines()==['RECOVERY-OK']
    report={'case':case,'session':session,'seconds':round(time.monotonic()-started,2),'run':current,'stream_errors':errors,'replay_events':len(replay),'passed':True}
    (ROOT/('recovery-'+case+'.json')).write_text(json.dumps(report,ensure_ascii=False,indent=2),encoding='utf-8')
    print(json.dumps({k:v for k,v in report.items() if k!='run'},ensure_ascii=False),flush=True)
    thread.join(3)


if __name__=='__main__':
    c=DevClient()
    for case in ('cancel','disconnect','worker-restart','api-restart'):run_case(c,case)
