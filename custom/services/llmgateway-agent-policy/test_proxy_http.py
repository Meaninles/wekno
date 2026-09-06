"""Exercise the complete LiteLLM HTTP routing boundary against a loopback fixture."""
import json
import os
from pathlib import Path
import subprocess
import tempfile
import threading
import time
import httpx
import yaml
from test_adapter_runtime import MockUpstream, ThreadingHTTPServer, WIRE

server=ThreadingHTTPServer(('127.0.0.1',0),MockUpstream)
threading.Thread(target=server.serve_forever,daemon=True).start()
with tempfile.TemporaryDirectory() as directory:
    root=Path(directory)
    config={'model_list':[{'model_name':'Qwen3.8-27B-Agent','litellm_params':{'model':'hosted_vllm/fixture',
        'api_base':f'http://127.0.0.1:{server.server_port}/v1','api_key':'fixture','drop_params':True}}],
        'general_settings':{'master_key':'sk-fixture'}}
    (root/'config.yaml').write_text(yaml.safe_dump(config))
    with (root/'server.log').open('w') as log:
        # The fixture runs inside a serving pod; do not inherit its production
        # database, Redis, credentials or callbacks into this temporary proxy.
        fixture_env={k:v for k,v in os.environ.items() if k in {
            'PATH','PYTHONPATH','PYTHONHOME','VIRTUAL_ENV','LD_LIBRARY_PATH',
            'HOME','TMPDIR','LANG','SSL_CERT_FILE','SSL_CERT_DIR'}}
        fixture_env.update(LITELLM_LOCAL_MODEL_COST_MAP='True',LITELLM_SALT_KEY='fixture-only-not-a-real-key')
        process=subprocess.Popen(['litellm','--config',str(root/'config.yaml'),'--port','14000','--host','127.0.0.1'],stdout=log,stderr=log,env=fixture_env)
        try:
            deadline=time.monotonic()+60
            while time.monotonic()<deadline:
                try:
                    if httpx.get('http://127.0.0.1:14000/health/readiness',timeout=1).status_code==200:break
                except httpx.HTTPError:pass
                if process.poll() is not None:raise RuntimeError((root/'server.log').read_text()[-3000:])
                time.sleep(.2)
            else:raise RuntimeError('Fixture proxy did not become ready: '+(root/'server.log').read_text()[-3000:])
            response=httpx.post('http://127.0.0.1:14000/v1/messages',headers={'x-api-key':'sk-fixture','anthropic-version':'2023-06-01'},
                json={'model':'Qwen3.8-27B-Agent','messages':[{'role':'user','content':'fixture'}],'max_tokens':256,
                      'thinking':{'type':'adaptive'},'output_config':{'effort':'xhigh'}},timeout=20)
            assert response.status_code==200,(response.status_code,response.text)
            assert WIRE[-1]['chat_template_kwargs']['enable_thinking'] is True,WIRE[-1]
            assert WIRE[-1]['chat_template_kwargs']['reasoning_effort']=='xhigh',WIRE[-1]
            assert WIRE[-1]['temperature']==1.0 and WIRE[-1]['top_p']==.95,WIRE[-1]
            print('PASS full Anthropic HTTP -> internal model rewrite -> Qwen thinking wire with requested xhigh',flush=True)
            body={'model':'Qwen3.8-27B-Agent','messages':[{'role':'user','content':'fixture'}],'max_tokens':256,
                  'thinking':{'type':'adaptive','display':'omitted'},'output_config':{'effort':'xhigh'},'stream':True}
            response=httpx.post('http://127.0.0.1:14000/v1/messages',headers={'x-api-key':'sk-fixture','anthropic-version':'2023-06-01'},json=body,timeout=20)
            events=[json.loads(line[5:]) for line in response.text.splitlines() if line.startswith('data:')]
            assert events[-1].get('type')=='error',(events,(root/'server.log').read_text()[-4500:])
            assert events[-1]['error']['type']=='api_error',events
            assert not any(e.get('type')=='message_stop' for e in events),events
            print('PASS full HTTP SSE empty terminal emits Anthropic error, not success or broken body',flush=True)
            import anthropic
            client=anthropic.Anthropic(base_url='http://127.0.0.1:14000',api_key='sk-fixture',max_retries=0)
            try:
                with client.messages.stream(**{k:v for k,v in body.items() if k!='stream'}) as stream:
                    list(stream)
            except anthropic.APIError as exc:
                assert 'upstream_empty_terminal' in str(exc),exc
            else:raise AssertionError('Anthropic client silently accepted empty completion')
            finally:client.close()
            print('PASS Anthropic SDK recognizes the typed SSE error',flush=True)
        finally:
            process.terminate();process.wait(timeout=10)
server.shutdown();server.server_close()
