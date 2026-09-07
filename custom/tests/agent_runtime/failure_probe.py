"""Exercise failure reporting through the real dev API and worker with a local provider fixture."""
import json
import threading
import time
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from dev_client import DevClient, ROOT
from dev_probe import sql


class Provider(BaseHTTPRequestHandler):
    def log_message(self, *_): pass

    def do_POST(self):
        body=json.loads(self.rfile.read(int(self.headers.get('Content-Length','0'))))
        case=body.get('model','').split('-')[1]
        if case == 'empty':
            self.send_response(200);self.send_header('Content-Type','text/event-stream');self.end_headers()
            event={'id':'fixture','object':'chat.completion.chunk','created':int(time.time()),'model':body['model'],
                   'choices':[{'index':0,'delta':{'role':'assistant','content':''},'finish_reason':'stop'}],
                   'usage':{'prompt_tokens':1,'completion_tokens':0,'total_tokens':1}}
            self.wfile.write(('data: '+json.dumps(event)+'\n\ndata: [DONE]\n\n').encode())
        else:
            status,message={'busy':(429,'rate limit exceeded'), 'timeout':(504,'upstream timed out'),
                            'unknown':(400,'PRIVATE_PROVIDER_DIAGNOSTIC_SENTINEL')}[case]
            self.send_response(status);self.send_header('Content-Type','application/json');self.end_headers()
            self.wfile.write(json.dumps({'error':{'message':message,'type':'fixture_error','code':str(status)}}).encode())


def main():
    c=DevClient();server=ThreadingHTTPServer(('0.0.0.0',0),Provider)
    threading.Thread(target=server.serve_forever,daemon=True).start()
    reports=[]
    try:
        for case,code in [('empty','empty_response'),('busy','busy'),('timeout','timeout'),('unknown','unknown')]:
            r=c.client.post('/models',json={'name':f'fixture-{case}-{uuid.uuid4().hex[:8]}','type':'KnowledgeQA','source':'remote',
                'parameters':{'base_url':f'http://host.docker.internal:{server.server_port}/v1','api_key':'local-fixture',
                              'interface_type':'openai','provider':'generic','extra_config':{'thinking_control':'none'}}})
            if not r.is_success: raise RuntimeError(f'Fixture model create HTTP {r.status_code}: {r.text[:400]}')
            model=r.json()['data']['id']
            try:
                report=c.qa('failure-'+case,'请用一句话打招呼。',agent='builtin-general-agent',model=model)
                session=report['session']
                persisted=sql("SELECT row_to_json(t) FROM (SELECT content,error_code,is_completed FROM messages WHERE session_id='"+session+"' AND role='assistant' ORDER BY created_at DESC LIMIT 1)t;")[0]
                failure=next((e for e in reversed(report['events']) if e.get('response_type')=='error' and e.get('done')),None)
                assert persisted['is_completed'] and persisted['error_code']==code, persisted
                assert failure and 'PRIVATE_PROVIDER_DIAGNOSTIC_SENTINEL' not in json.dumps(report['events']), failure
                assert 'Runtime failed' not in persisted['content']
                reports.append({'case':case,'session':session,'code':code,'history':persisted,'stream':failure,'seconds':report['seconds']})
                print(json.dumps({'failure_case':case,'code':code,'message':persisted['content'],'passed':True},ensure_ascii=False),flush=True)
            finally:
                c.client.delete('/models/'+model).raise_for_status()
    finally:
        server.shutdown();server.server_close()
        (ROOT/'failure-categories.json').write_text(json.dumps(reports,ensure_ascii=False,indent=2),encoding='utf-8')


if __name__=='__main__': main()
