"""Run inside the exact LiteLLM 1.92 image, with the staged sitecustomize loaded.

A loopback HTTP server records the actual upstream JSON; no model is contacted.
"""
import asyncio
import os
os.environ.setdefault("LITELLM_SALT_KEY", "fixture-only-not-a-real-key")
import copy
import json
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import sitecustomize as adapter
from generation_policy import ROUTES
import generation_policy
from litellm import Router
from litellm.types.utils import ModelResponseStream, ModelResponse
from litellm.llms.anthropic.experimental_pass_through.adapters.streaming_iterator import AnthropicStreamWrapper

WIRE = []


class MockUpstream(BaseHTTPRequestHandler):
    def log_message(self, *args): pass
    def do_POST(self):
        data = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
        WIRE.append(data)
        if data.get('stream'):
            self.send_response(200);self.send_header('Content-Type','text/event-stream');self.end_headers()
            for delta,finish in [({'role':'assistant','reasoning_content':'fixture reasoning'},None),({},'stop')]:
                chunk={'id':'fixture','object':'chat.completion.chunk','created':1,'model':data['model'],
                    'choices':[{'index':0,'delta':delta,'finish_reason':finish}]}
                self.wfile.write(('data: '+json.dumps(chunk)+'\n\n').encode());self.wfile.flush()
            self.wfile.write(b'data: [DONE]\n\n');return
        body = json.dumps({'id':'fixture','object':'chat.completion','created':1,'model':data['model'],
            'choices':[{'index':0,'message':{'role':'assistant','content':'fixture answer'},'finish_reason':'stop'}],
            'usage':{'prompt_tokens':10,'completion_tokens':3,'total_tokens':13}}).encode()
        self.send_response(200);self.send_header('Content-Type','application/json');self.end_headers();self.wfile.write(body)


class AdapterTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.server = ThreadingHTTPServer(('127.0.0.1',0), MockUpstream)
        threading.Thread(target=cls.server.serve_forever,daemon=True).start()
        names = list(ROUTES) + ['DeepSeek-V4-Flash','Qwen3.6-27B']
        cls.router = Router(model_list=[{'model_name':name,'litellm_params':{
            'model':'hosted_vllm/fixture', 'api_base':f'http://127.0.0.1:{cls.server.server_port}/v1',
            'api_key':'fixture', 'drop_params':True,
            'extra_body':{'chat_template_kwargs': {'thinking' if name.startswith('Deep') else 'enable_thinking':False}}
        }} for name in names], num_retries=0)

    @classmethod
    def tearDownClass(cls): cls.server.shutdown()

    def test_actual_wire_for_both_protocols_and_native_efforts(self):
        for name, (kind, _, default) in ROUTES.items():
            for effort in (None, 'low', default):
                for anthropic in (False, True):
                    controls={'thinking':{'type':'adaptive' if effort else 'disabled'}}
                    if effort: controls['output_config' if anthropic else 'reasoning_effort'] = {'effort':effort} if anthropic else effort
                    if anthropic:
                        request={'model':name,'messages':[{'role':'user','content':'wire fixture'}], 'max_tokens':256, **controls}
                        (adapter._validate_ds_v4_anthropic_request if kind=='deepseek0731' else adapter._validate_qwen36_27b_anthropic_request)(request)
                        kwargs, _ = adapter.A().translate_anthropic_to_openai(request)
                    else:
                        kwargs={'model':name,'messages':[{'role':'user','content':'wire fixture'}], 'max_tokens':256, **controls}
                    # Both sync and async Router paths must preserve the same policy.
                    for sync in (False, True):
                        if sync: self.router.completion(**copy.deepcopy(kwargs))
                        else: asyncio.run(self.router.acompletion(**copy.deepcopy(kwargs)))
                        sent=WIRE[-1];ctk=sent['chat_template_kwargs']
                        effective_thinking=bool(effort)
                        self.assertEqual(ctk['thinking' if kind=='deepseek0731' else 'enable_thinking'],effective_thinking,sent)
                        if effective_thinking: self.assertEqual(ctk['reasoning_effort'],effort,sent)
                        else: self.assertNotIn('reasoning_effort',ctk)
                        self.assertNotIn('reasoning_effort',sent)
                        self.assertEqual(sent['temperature'], 1.0 if kind=='deepseek0731' or effective_thinking else .7,sent)
                        self.assertEqual(sent['top_p'], .95 if kind=='deepseek0731' or effective_thinking else .8,sent)
                        if kind=='qwen38':
                            for key,value in {'top_k':20,'min_p':0.0,'presence_penalty':0.0 if effective_thinking else 1.5,'repetition_penalty':1.0}.items():
                                self.assertEqual(sent.get(key),value,(key,sent))

    def test_legacy_wire_keeps_requested_sampling_and_default_off(self):
        for model in ['DeepSeek-V4-Flash','Qwen3.6-27B']:
            self.router.completion(model=model,messages=[{'role':'user','content':'fixture'}],temperature=.1,max_tokens=50)
            sent=WIRE[-1]
            self.assertEqual(sent['temperature'],.1)
            self.assertFalse(sent['chat_template_kwargs'].get('thinking',sent['chat_template_kwargs'].get('enable_thinking')))

    def test_anthropic_route_identity_survives_router_model_rewrite(self):
        token=generation_policy.route_context.set('Qwen3.8-27B-Agent')
        try:
            request={'model':'hosted_vllm/fixture','messages':[{'role':'user','content':'fixture'}], 'max_tokens':256,
                'thinking':{'type':'adaptive'},'output_config':{'effort':'xhigh'}}
            adapter._validate_qwen36_27b_anthropic_request(request)
            kwargs={}
            self.assertTrue(generation_policy.translate_anthropic(request,kwargs))
            self.assertTrue(kwargs['extra_body']['chat_template_kwargs']['enable_thinking'])
            self.assertEqual(kwargs['extra_body']['chat_template_kwargs']['reasoning_effort'],'xhigh')
        finally:generation_policy.route_context.reset(token)

    def test_anthropic_empty_terminal_and_mixed_payload(self):
        for model in ['Qwen3.6-27B',*ROUTES]:
            for answer in ('','actual final answer'):
                chunks=[ModelResponseStream(id='fixture',model=model,choices=[{'index':0,'delta':{
                    'reasoning_content':'fixture reasoning', 'content':answer},'finish_reason':'stop'}])]
                token=adapter._QWEN36_27B_ANTHROPIC_DISPLAY_CONTEXT.set('omitted')
                try:
                    stream=AnthropicStreamWrapper(iter(chunks),model)
                    if not answer:
                        events=list(stream)
                        self.assertEqual(events[-1]['type'],'error')
                        self.assertIn('upstream_empty_terminal',events[-1]['error']['message'])
                    else:
                        events=list(stream)
                        self.assertTrue(any(e.get('delta',{}).get('text')==answer for e in events))
                finally:adapter._QWEN36_27B_ANTHROPIC_DISPLAY_CONTEXT.reset(token)


if __name__ == '__main__': unittest.main()
