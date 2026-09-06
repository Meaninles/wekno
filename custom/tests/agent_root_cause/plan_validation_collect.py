"""Read-only collection of local diagnostic sessions and model observations."""
import base64
import json
from pathlib import Path
import subprocess
import urllib.request

ROOT=Path(__file__).resolve().parents[3]
OUT=ROOT/'.local-data/agent-audit-20260905/plan-validation'
def collect(session_id):
    import uuid
    uuid.UUID(session_id)
    sql=f"SELECT coalesce(json_agg(m ORDER BY created_at),'[]') FROM messages m WHERE session_id='{session_id}';"
    db=subprocess.run(['docker','exec','-i','WeKnora-agent-eval-postgres-dev','psql','-X','-q','-U','postgres','-d','WeKnora','-At'],input=sql,capture_output=True,text=True,encoding='utf-8',check=True)
    messages=json.loads(db.stdout)
    (OUT/f'messages-{session_id}.json').write_text(json.dumps(messages,ensure_ascii=False,indent=2),encoding='utf-8')
    env={}
    for path in [Path('C:/weknora/.env'),ROOT/'custom/services/agent-eval/eval.env']:
        for line in path.read_text(encoding='utf-8-sig').splitlines():
            if '=' in line and not line.lstrip().startswith('#'):
                k,v=line.split('=',1);env[k]=v.strip().strip('"').strip("'")
    auth=base64.b64encode((env['LANGFUSE_PUBLIC_KEY']+':'+env['LANGFUSE_SECRET_KEY']).encode()).decode()
    observations=[];cursor=''
    while True:
        url='http://localhost:13001/api/public/v2/observations?sessionId='+session_id+'&fields=core,basic,time,io,model,usage,metadata&limit=1000'+('&cursor='+cursor if cursor else '')
        with urllib.request.urlopen(urllib.request.Request(url,headers={'Authorization':'Basic '+auth}),timeout=60) as response:data=json.load(response)
        observations.extend(data.get('data',[]));cursor=(data.get('meta') or {}).get('cursor')
        if not cursor:break
    (OUT/f'observations-{session_id}.json').write_text(json.dumps(observations,ensure_ascii=False,indent=2),encoding='utf-8')
    return messages,observations

if __name__=='__main__':
    import sys
    for session in sys.argv[1:]:
        messages,obs=collect(session)
        print(session,'messages',len(messages),'observations',len(obs))
        for o in obs:
            raw=o.get('input')
            try:inp=json.loads(raw) if isinstance(raw,str) else raw
            except ValueError:inp=None
            if isinstance(inp,dict):print(o['name'],o['id'],list(inp))
