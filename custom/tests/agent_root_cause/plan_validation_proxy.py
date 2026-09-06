"""Transparent diagnostic proxy for isolated test model configurations only.

Does not change prompts, schemas, sampling parameters, or response bytes.
Records bodies and timing in a private directory; never records header values.
Run inside the eval SDK container, with upstream URL passed as an argument.
"""
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.request import Request, urlopen
from urllib.error import HTTPError
import itertools
import json
import sys
import time

ROOT = Path(sys.argv[1]); ROOT.mkdir(parents=True, exist_ok=True)
UPSTREAM = sys.argv[2].rstrip('/')
COUNTER = itertools.count(1)

class Proxy(BaseHTTPRequestHandler):
    protocol_version = 'HTTP/1.0'
    def log_message(self, *args): pass
    def do_GET(self):
        body=b'{"ok":true}'
        self.send_response(200); self.send_header('Content-Length',str(len(body))); self.end_headers(); self.wfile.write(body)
    def do_POST(self):
        number=next(COUNTER); prefix=ROOT/f'{number:04}'
        raw=self.rfile.read(int(self.headers.get('Content-Length',0)))
        path=self.path
        if UPSTREAM.endswith('/v1') and path.startswith('/v1/'): path=path[3:]
        headers={k:v for k,v in self.headers.items() if k.lower() not in {'host','connection','content-length','accept-encoding'}}
        request_json=json.loads(raw)
        prefix.with_suffix('.request.json').write_text(json.dumps({'path':self.path,'header_names':list(headers),'body':request_json},ensure_ascii=False,indent=2),encoding='utf-8')
        started=time.perf_counter(); meta={'number':number,'path':self.path,'first_byte_s':None,'response_bytes':0}
        try:
            try: response=urlopen(Request(UPSTREAM+path,raw,headers),timeout=300)
            except HTTPError as error: response=error
            meta['status']=response.status
            self.send_response(response.status)
            self.send_header('Content-Type',response.headers.get('Content-Type','application/json'))
            self.send_header('Connection','close'); self.end_headers()
            with response, prefix.with_suffix('.response').open('wb') as output:
                while True:
                    chunk=response.read1(16384)
                    if not chunk: break
                    if meta['first_byte_s'] is None: meta['first_byte_s']=time.perf_counter()-started
                    output.write(chunk); output.flush(); self.wfile.write(chunk); self.wfile.flush()
                    meta['response_bytes']+=len(chunk)
        except Exception as error:
            meta['error_type']=type(error).__name__
        finally:
            meta['elapsed_s']=time.perf_counter()-started
            prefix.with_suffix('.meta.json').write_text(json.dumps(meta,indent=2),encoding='utf-8')
            print(json.dumps(meta),flush=True)
            self.close_connection=True

server=ThreadingHTTPServer(('0.0.0.0',8099),Proxy)
print('Diagnostic proxy ready on 8099',flush=True)
server.serve_forever()
