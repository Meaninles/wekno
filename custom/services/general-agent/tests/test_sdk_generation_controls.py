import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch

from app.schemas import ChatPayload
from app.sdk_runtime import run_sdk


class SDKGenerationTests(unittest.IsolatedAsyncioTestCase):
    async def test_effort_tracks_model_configuration_and_thinking_switch(self):
        observed=[]
        async def query(*,prompt,options):
            observed.append(options)
            result=type('ResultMessage',(),{})()
            result.subtype='success';result.stop_reason='end_turn';result.is_error=False;result.result='fixture answer'
            yield result
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory)
            tools=SimpleNamespace(workspace=SimpleNamespace(root=root),entries={},sdk_server=lambda:{},sdk_before_tool=lambda:None)
            for enabled in (True,False):
                payload=ChatPayload.model_validate(dict(run_id='fixture',session_id='s',assistant_message_id='a',query='fixture',
                    llm={'model_name':'configured-route','reasoning_effort':'xhigh'},runtime_config={'thinking':enabled},tool_callback_url='http://platform/tools/call'))
                with patch('claude_agent_sdk.query',query),patch('app.runner.claude_auth_env',return_value=({},'configured-route','{}')):
                    events=[event async for event in run_sdk(payload,'system','prompt',tools,{},root)]
                self.assertEqual(observed[-1].effort,'xhigh' if enabled else None)
                self.assertEqual(observed[-1].thinking['type'],'adaptive' if enabled else 'disabled')
                self.assertEqual(events[-1].type,'terminal_answer')


if __name__=='__main__':unittest.main()
