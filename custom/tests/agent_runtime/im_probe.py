"""Exercise the real IM callback/adapter using a local Mattermost HTTP fixture.

No real IM account or external recipient is contacted. App, DB and harness are
the existing development services; only the platform's receiving API is mocked.
"""
import json
import threading
import time
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import httpx
from dev_client import DevClient, ROOT, KB, MODEL
from dev_probe import sql


def main():
    posts={};updates=[];token=uuid.uuid4().hex;outgoing=uuid.uuid4().hex
    class Receiver(BaseHTTPRequestHandler):
        def log_message(self,*args):pass
        def respond(self,value,status=200):
            body=json.dumps(value).encode();self.send_response(status);self.send_header('Content-Type','application/json');self.send_header('Content-Length',str(len(body)));self.end_headers();self.wfile.write(body)
        def do_GET(self):self.respond({'id':self.path.rsplit('/',1)[-1],'root_id':''})
        def do_POST(self):self.record()
        def do_PUT(self):self.record()
        def record(self):
            if self.headers.get('Authorization')!='Bearer '+token:return self.respond({},403)
            body=json.loads(self.rfile.read(int(self.headers['Content-Length'])))
            identity=uuid.uuid4().hex if self.command=='POST' else self.path.split('/')[-2]
            posts[identity]=body;updates.append({'method':self.command,'id':identity,'body':body});self.respond({'id':identity})
    receiver=ThreadingHTTPServer(('0.0.0.0',0),Receiver);threading.Thread(target=receiver.serve_forever,daemon=True).start()
    c=DevClient();agent=None;channel=None;reports=[]
    try:
        config=next(a['config'] for a in c.client.get('/agents').json()['data'] if a['id']=='builtin-knowledge-qa')
        config.update({'kb_selection_mode':'selected','knowledge_bases':[KB],'model_id':MODEL})
        r=c.client.post('/agents',json={'name':'统一内核 IM 接口模拟','description':'仅连接本机测试接收端','config':config});r.raise_for_status();agent=r.json()['data']['id']
        r=c.client.post('/agents/'+agent+'/im-channels',json={'platform':'mattermost','name':'本机模拟接收端','mode':'webhook','output_mode':'stream','credentials':{'site_url':f'http://host.docker.internal:{receiver.server_port}','bot_token':token,'outgoing_token':outgoing,'bot_user_id':'fixture-bot'}});r.raise_for_status();channel=r.json()['data']['id']
        root=uuid.uuid4().hex;user='fixture-'+uuid.uuid4().hex
        with httpx.Client(base_url='http://localhost:8080/api/v1',timeout=60) as callback:
            bad=callback.post('/im/callback/'+channel,json={'token':'wrong','post_id':uuid.uuid4().hex,'user_id':user,'channel_id':'fixture-room','text':'should be denied'})
            assert bad.status_code==403
            for index,query in enumerate(['按公司制度，国内实用新型专利一次性奖励多少，税前还是税后？附依据。','如果刚才同一项又取得国际授权，变成多少？可以重复领取吗？'],1):
                before=len(updates);post=uuid.uuid4().hex;started=time.monotonic()
                body={'token':outgoing,'post_id':post,'root_id':root,'user_id':user,'user_name':'接口模拟用户','channel_id':'fixture-room','text':query}
                callback.post('/im/callback/'+channel,json=body).raise_for_status()
                run=None
                while time.monotonic()-started<420:
                    rows=sql("select row_to_json(t) from (select r.id,r.session_id,r.status,r.model_requests,r.result,r.error,r.reserved_tokens from custom_agent_runs r join im_channel_sessions s on s.session_id=r.session_id where s.im_channel_id='"+channel+"' order by r.created_at desc limit 1)t")
                    if rows and rows[0]['id'] not in [x['run']['id'] for x in reports] and rows[0]['status'] in ('completed','failed','cancelled'):
                        run=rows[0];break
                    time.sleep(2)
                assert run is not None,'IM callback did not produce a terminal harness run'
                time.sleep(2)
                delivered=updates[before:];expected='2000' if index==1 else '4000'
                texts=[u['body'].get('message','') for u in delivered]
                assert run['status']=='completed',run['error']
                assert any(expected in text.replace(',','') for text in texts),texts
                assert texts and '<src ' not in texts[-1]
                report={'turn':index,'query':query,'seconds':round(time.monotonic()-started,2),'run':run,'outbound':delivered,'signature_rejection':True}
                reports.append(report)
                print(json.dumps({'im_turn':index,'session':run['session_id'],'status':run['status'],'model_requests':run['model_requests'],'outbound_updates':len(delivered)},ensure_ascii=False),flush=True)
                callback.post('/im/callback/'+channel,json=body).raise_for_status()
                time.sleep(2)
                count=sql("select row_to_json(t) from (select count(*) n from custom_agent_runs where session_id='"+run['session_id']+"')t")[0]['n']
                assert count==index,'Duplicate IM delivery created another run'
            assert reports[0]['run']['session_id']==reports[1]['run']['session_id']
    finally:
        ROOT.mkdir(parents=True,exist_ok=True);(ROOT/'im-simulation.json').write_text(json.dumps(reports,ensure_ascii=False,indent=2),encoding='utf-8')
        if channel:c.client.delete('/im-channels/'+channel).raise_for_status()
        if agent:c.client.delete('/agents/'+agent).raise_for_status()
        receiver.shutdown();receiver.server_close()


if __name__=='__main__':main()
