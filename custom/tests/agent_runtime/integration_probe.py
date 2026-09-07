"""Real MCP and webpage access through existing dev application, local fixture only."""
import json
import threading
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from dev_client import DevClient


class Fixture(BaseHTTPRequestHandler):
    def log_message(self, *_): pass
    def do_GET(self):
        self.send_response(200);self.send_header('Content-Type','text/html; charset=utf-8');self.end_headers()
        self.wfile.write('<html><title>开发核对页面</title><main><h1>仓库盘点</h1><p>盘点编号 WEB-58317。A 区 37 箱，B 区 26 箱，总计 63 箱。</p></main></html>'.encode())
    def do_POST(self):
        request=json.loads(self.rfile.read(int(self.headers['Content-Length'])))
        method=request.get('method')
        if 'id' not in request:
            self.send_response(202);self.end_headers();return
        if method=='initialize':
            result={'protocolVersion':'2024-11-05','capabilities':{'tools':{}},'serverInfo':{'name':'dev-ledger','version':'1'}}
        elif method=='tools/list':
            result={'tools':[{'name':'read_ledger','description':'Read the exact fixture ledger; total CNY amount for audit code MCP-7426.',
                'inputSchema':{'type':'object','properties':{},'additionalProperties':False},'annotations':{'readOnlyHint':True,'destructiveHint':False,'idempotentHint':True}}]}
        elif method=='tools/call':
            result={'content':[{'type':'text','text':json.dumps({'audit_code':'MCP-7426','paid_cny':167,'rows':2})}],'isError':False}
        else: result={}
        self.send_response(200);self.send_header('Content-Type','application/json');self.end_headers()
        self.wfile.write(json.dumps({'jsonrpc':'2.0','id':request['id'],'result':result}).encode())


def main():
    c=DevClient();server=ThreadingHTTPServer(('0.0.0.0',0),Fixture)
    threading.Thread(target=server.serve_forever,daemon=True).start()
    service=agent=None
    try:
        base=f'http://host.docker.internal:{server.server_port}'
        r=c.client.post('/mcp-services',json={'name':'runtime-ledger-'+uuid.uuid4().hex[:8],'enabled':True,'transport_type':'http-streamable','url':base+'/mcp'});r.raise_for_status();service=r.json()['data']['id']
        config=next(a['config'] for a in c.client.get('/agents').json()['data'] if a['id']=='builtin-general-agent')
        config.update({'mcp_selection_mode':'selected','mcp_services':[service],'kb_selection_mode':'none','knowledge_bases':[],
            'professional_skills_selection_mode':'none','selected_professional_skills':[], 'web_search_enabled':True})
        r=c.client.post('/agents',json={'name':'开发集成验证 '+uuid.uuid4().hex[:8],'config':config});r.raise_for_status();agent=r.json()['data']['id']
        report=c.qa('mcp-integration','请调用所选 MCP 的 read_ledger 工具，报告核对编号、已付金额和行数，必须以实际工具返回为准。',agent=agent,extra={'mcp_service_ids':[service]},expected=('MCP-7426','167'))
        assert all(report['checks'].values()),report['checks']
        assert any('read_ledger' in str(e.get('data',{}).get('tool_name','')) for e in report['events'])
        report=c.qa('web-integration',f'请直接读取这个页面 {base}/inventory ，给出盘点编号，复核 A 区、B 区和总箱数，并附页面来源。',agent=agent,expected=('WEB-58317','63'),extra={'web_search_enabled':True})
        assert all(report['checks'].values()),report['checks']
    finally:
        if agent:c.client.delete('/agents/'+agent).raise_for_status()
        if service:c.client.delete('/mcp-services/'+service).raise_for_status()
        server.shutdown();server.server_close()


if __name__=='__main__':main()
