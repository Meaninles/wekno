import asyncio
import hashlib
import json
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

import httpx
from claude_agent_sdk import ClaudeAgentOptions
from app.artifact_store import ArtifactStore
from app.runtime_tools import RuntimeTools
from app.schemas import ChatPayload
from app.platform_runtime import run_platform
from app.sdk_runtime import sdk_response_stream
from app.working_context import ToolResultStore, visible_context_view


class WorkingContextTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.root = Path(self.tmp.name)
        self.payload = ChatPayload.model_validate(dict(run_id='context-test', session_id='s', assistant_message_id='a',
            query='exact request', llm={'model_name':'configured'}, enable_artifacts=True,
            tool_callback_url='http://mock/tools/call'))
        self.tools = RuntimeTools(self.payload, ArtifactStore(self.root,self.payload))

    async def asyncTearDown(self):
        self.tmp.cleanup()

    async def test_archive_is_exact_readable_and_retains_source_ids(self):
        text = '[source: exact-id]\r\n' + '制度原文\r\n'*10000 + '\n最后限定条件'
        view = json.loads(self.tools.model_result({'success':True,'output':text},'knowledge_search'))
        saved = self.root/view['file_path']
        self.assertEqual(saved.read_bytes(),text.encode('utf-8'))
        self.assertFalse(view['complete'])
        chunks=[];offset=0
        while offset is not None:
            page=await self.tools.execute('read_file',{'file_path':view['file_path'],'offset':offset},'page-'+str(offset))
            chunks.append(page['text']);offset=page['next_offset']
            self.assertNotIn('Preview only',self.tools.model_result(page,'read_file'))
        self.assertEqual(''.join(chunks),text)
        self.assertEqual(view['sha256'],hashlib.sha256(text.encode()).hexdigest())
        # A reader remains reachable even if its serialized page exceeds the threshold.
        self.assertEqual(ToolResultStore(self.root).view(text,'read_conversation'),text)

    async def test_compact_catalog_keeps_every_identity_and_full_details(self):
        context={'agent':{'id':'a','name':'agent','agent_type':'general','description':'detail'},
            'visible_resources':{'knowledge_bases':[{'id':str(i),'name':'name-'+str(i),'description':'detail'*100} for i in range(150)]},
            'effective_configuration':{'knowledge_base_ids':['selected'],'max_iterations':30,'mcp_service_ids':['tool-server']}}
        before=json.dumps(context,ensure_ascii=False)
        view=visible_context_view(context,self.root)
        self.assertEqual(json.dumps(context,ensure_ascii=False),before)
        self.assertEqual(len(view['visible_resources']['knowledge_bases']),150)
        self.assertEqual(view['visible_resources']['knowledge_bases'][-1],{'id':'149','name':'name-149'})
        self.assertEqual(json.loads((self.root/view['configuration_details']['file_path']).read_text()),context)
        self.assertEqual(view['effective_configuration']['mcp_service_ids'],['tool-server'])
        self.assertLess(len(json.dumps(view)),len(before)/5)

    async def test_incremental_edit_handles_stale_parallel_and_ambiguous_writes(self):
        created=await self.tools.execute('write_file',{'file_path':'sub/script.py','content':'a\r\na\r\n'},'create')
        args={'file_path':'sub/script.py','expected_sha256':created['sha256'],'old_text':'a','new_text':'b'}
        bad=await self.tools.execute('edit_file',args,'ambiguous')
        self.assertFalse(bad['success'])
        args.update(old_text='',new_text='end\r\n')
        results=await asyncio.gather(*(self.tools.execute('edit_file',args,'edit-'+str(i)) for i in range(2)))
        self.assertEqual(sum(r.get('success') is True for r in results),1)
        self.assertEqual((self.root/'sub/script.py').read_bytes(),b'a\r\na\r\nend\r\n')
        page=await self.tools.execute('read_file',{'file_path':'sub/script.py'},'read')
        self.assertEqual(page['text'],'a\r\na\r\nend\r\n')
        bad=await self.tools.execute('write_file',{'file_path':'../outside','content':'no'},'escape')
        self.assertFalse(bad['success'])
        bad=await self.tools.execute('write_file',{'file_path':'sub/script.py','content':'overwrite'},'overwrite')
        self.assertFalse(bad['success'])

    async def test_large_text_can_be_read_without_execute_permission(self):
        path=self.root/'large.txt'
        path.write_bytes(b'x'*(9*1024*1024)+b'final')
        page=await self.tools.workspace.read_file({'file_path':path.name,'offset':9*1024*1024})
        self.assertEqual(page['text'],'final')
        self.assertIsNone(page['next_offset'])

    async def test_platform_length_does_not_replay_completed_or_execute_partial_tools(self):
        requests=[]
        good={'id':'written','type':'function','function':{'name':'write_file','arguments':json.dumps({'file_path':'once.txt','content':'exact'})}}
        incomplete={'id':'partial','type':'function','function':{'name':'write_file','arguments':'{"content":"unfinished'}}
        async def transport(request):
            requests.append(json.loads(request.content))
            events=[{'response_type':'tool_call','tool_calls':[good],'finish_reason':'tool_calls','done':True},
                {'response_type':'tool_call','tool_calls':[incomplete],'finish_reason':'length','done':True},
                {'response_type':'answer','content':'complete','finish_reason':'stop','done':True}]
            return httpx.Response(200,content=json.dumps(events[len(requests)-1]))
        original=httpx.AsyncClient
        observation={}
        with patch('app.platform_runtime.httpx.AsyncClient',side_effect=lambda **kw:original(transport=httpx.MockTransport(transport),**kw)):
            events=[e async for e in run_platform(self.payload,'system','exact request',self.tools,observation)]
        self.assertEqual((self.root/'once.txt').read_text(),'exact')
        self.assertEqual(requests[1]['messages'],requests[2]['messages'][:-1])
        self.assertNotIn('unfinished',json.dumps(requests[2]['messages']))
        self.assertEqual([e.id for e in events if e.type=='runtime_tool'],['written','written'])
        self.assertEqual(len(observation['response_recoveries']),1)
        self.assertEqual(events[-1].content,'complete')

    async def test_sdk_resume_uses_original_session_remaining_turns_and_budget(self):
        seen=[];closed=[]
        async def query(*,prompt,options):
            seen.append((prompt,options))
            try:
                result=type('ResultMessage',(),{})()
                result.subtype='success';result.is_error=False;result.session_id='native-session';result.num_turns=3
                result.stop_reason='max_tokens' if len(seen)==1 else 'end_turn'
                result.result='' if len(seen)==1 else 'complete'
                yield result
            finally:closed.append(len(seen))
        options=ClaudeAgentOptions(max_turns=10,thinking={'type':'adaptive'},effort='xhigh',
            env={'CLAUDE_CODE_MAX_OUTPUT_TOKENS':'8192'},extra_args={'name':'fixture'})
        with patch('claude_agent_sdk.query',query):
            events=[e async for e in sdk_response_stream('original',options,{})]
        self.assertEqual(closed,[1,2])
        self.assertEqual(seen[1][1].resume,'native-session')
        self.assertEqual(seen[1][1].max_turns,7)
        self.assertEqual(seen[0][1].env,seen[1][1].env)
        self.assertEqual(seen[0][1].thinking,seen[1][1].thinking)
        self.assertEqual(events[-1].result,'complete')

    async def test_sdk_does_not_retry_http_error_or_exhausted_turns(self):
        for subtype,is_error,turns in [('error_during_execution',True,1),('success',False,10)]:
            seen=[]
            async def query(*,prompt,options):
                seen.append(prompt)
                result=type('ResultMessage',(),{})()
                result.subtype=subtype;result.is_error=is_error;result.session_id='session';result.num_turns=turns
                result.stop_reason='max_tokens';result.result=''
                yield result
            with patch('claude_agent_sdk.query',query):
                _=[e async for e in sdk_response_stream('original',ClaudeAgentOptions(max_turns=10),{})]
            self.assertEqual(len(seen),1)
