import json
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from app.runner import GeneralAgentRunner
from app.schemas import ChatPayload, LLMConfig, RunEvent


class RuntimeStatusTests(unittest.IsolatedAsyncioTestCase):
    async def test_status_precedes_runtime_io_and_finishes_without_becoming_a_tool(self):
        for adapter in ('platform', 'claude-sdk'):
            entered = []

            async def runtime(*args):
                entered.append(True)
                yield RunEvent(type='answer_delta', content='Delivered', done=True)
                yield RunEvent(type='terminal_answer', content='Delivered')

            payload = ChatPayload(run_id='status-test', session_id='session-test',
                assistant_message_id='message-test', query='A request',
                llm=LLMConfig(model_name='fixture', runtime_adapter=adapter),
                tool_callback_url='http://fixture/tools/call')
            with tempfile.TemporaryDirectory() as directory, \
                    patch('app.platform_runtime.run_platform', runtime), \
                    patch('app.sdk_runtime.run_sdk', runtime):
                stream = GeneralAgentRunner(Path(directory), payload).run()
                first = await anext(stream)
                self.assertFalse(entered, 'status must precede model/network work')
                self.assertEqual(first.type, 'progress')
                self.assertTrue(first.data['transient'])
                self.assertEqual(first.data['answer_contract'], 'runtime-terminal-v1')
                events = [first] + [event async for event in stream]
                status = [event for event in events if event.type == 'progress']
                self.assertTrue(status[-1].done)
                self.assertTrue(all('tool_name' not in event.data for event in status))
                self.assertEqual(len(entered), 1)
                self.assertEqual(events[-1].type, 'result')
                self.assertEqual(events[-1].data['answer'], 'Delivered')
