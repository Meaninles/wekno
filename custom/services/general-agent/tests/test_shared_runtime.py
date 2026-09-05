import asyncio
import hashlib
import sys
import tempfile
import types
import unittest
from pathlib import Path
from unittest import mock
from dataclasses import dataclass

sys.path.insert(0,str(Path(__file__).resolve().parents[1]))
from app.runner import GeneralAgentRunner, ArtifactStore, build_prompt, build_weknora_server
from app.final_delivery import TerminalTextStream, terminal_binding_marker, TERMINAL_ANSWER_OPEN, TERMINAL_ANSWER_CLOSE
from app.schemas import ChatPayload, LLMConfig, RuntimeConfigSpec

@dataclass
class StreamEvent:
    event: dict
@dataclass
class TextBlock:
    text: str
@dataclass
class AssistantMessage:
    content: list
    message_id: str = "main"
@dataclass
class ResultMessage:
    result: str
    subtype: str = "success"
    stop_reason: str = "end_turn"
    is_error: bool = False
    usage: dict = None

class SharedRuntimeTests(unittest.TestCase):
    def payload(self, agent_type="general-agent"):
        return ChatPayload(run_id="shared-run",session_id="shared-session",user_message_id="persisted-user-id",assistant_message_id="reply",query="An arbitrary request",llm=LLMConfig(model_name="test",api_key="test"),runtime_config=RuntimeConfigSpec(agent_type=agent_type),tool_callback_url="http://127.0.0.1/callback")

    def test_current_source_identity_and_single_copy(self):
        p=self.payload();text=build_prompt(p)
        self.assertEqual(text.count(p.query),1)
        self.assertIn('source_id="user_message_persisted-user-id"',text)
        self.assertNotIn("runtime_preflight",text)
        self.assertNotIn("current_user_request_replay",text)

    def test_terminal_projection_handles_every_split(self):
        body="第一段完整内容。第二段保留 Unicode 与空格。"
        raw=terminal_binding_marker("run")+TERMINAL_ANSWER_OPEN+body+TERMINAL_ANSWER_CLOSE
        for split in range(len(raw)+1):
            stream=TerminalTextStream("run")
            result=stream.push(raw[:split])+stream.push(raw[split:])+stream.push("",final=True)
            self.assertEqual(result,body)
        wrong=TerminalTextStream("another-run")
        self.assertEqual(wrong.push(raw,final=True),"")

    def test_all_sdk_profiles_stream_and_stop_at_result_without_extra_calls(self):
        for kind in ["general-agent","document-processing-agent","data-analysis","table-analysis","knowledge-base-manager"]:
            with self.subTest(agent=kind):
                self._run_single_profile(kind)

    def _run_single_profile(self, kind):
        calls=[]; tools=[]; received_after_result=[]
        class Options:
            def __init__(self,**kwargs):self.values=kwargs
        def tool(name,description,schema):
            tools.append(name);return lambda fn:fn
        body="".join(f"第 {i} 个通用测试片段保留文本。" for i in range(12))
        raw=terminal_binding_marker("shared-run")+TERMINAL_ANSWER_OPEN+body+TERMINAL_ANSWER_CLOSE
        async def query(prompt,options):
            calls.append(options.values)
            yield AssistantMessage([{"type":"tool_use","id":"local-file","name":"mcp__weknora__create_artifact","input":{"filename":"arbitrary.txt"}}])
            yield types.SimpleNamespace(content=[{"type":"tool_result","tool_use_id":"local-file","content":"exact result","is_error":False}])
            for offset in range(0,len(raw),13):
                yield StreamEvent({"type":"content_block_delta","delta":{"type":"text_delta","text":raw[offset:offset+13]}})
                await asyncio.sleep(.002)
            yield AssistantMessage([TextBlock(raw)])
            yield ResultMessage(raw,usage={"input_tokens":100,"output_tokens":40})
            received_after_result.append(True)
            await asyncio.sleep(5)
        sdk=types.SimpleNamespace(ClaudeAgentOptions=Options,HookMatcher=Options,query=query,tool=tool,create_sdk_mcp_server=lambda *a,**k:{})
        async def collect(runner):return [e async for e in runner.run()]
        with tempfile.TemporaryDirectory() as root,mock.patch.dict(sys.modules,{"claude_agent_sdk":sdk}):
            events=asyncio.run(collect(GeneralAgentRunner(Path(root),self.payload(kind))))
        answer_events=[e for e in events if e.type=="answer_delta" and e.content]
        self.assertGreater(len(answer_events),1)
        self.assertEqual("".join(e.content for e in answer_events),body)
        self.assertEqual(len(calls),1)
        self.assertEqual(calls[0]["extra_args"],{"no-session-persistence":None})
        self.assertNotIn("resume",calls[0])
        self.assertNotIn("final_answer",tools)
        self.assertFalse(received_after_result)
        result=next(e for e in events if e.type=="result")
        self.assertEqual(result.data["answer"],body)
        self.assertEqual(result.data["prompt_observation"]["sdk_usage"][0]["input_tokens"],100)
        local_results=[e for e in events if e.type=="progress" and e.done and e.id=="local-file"]
        self.assertEqual(len(local_results),1)
        self.assertEqual(local_results[0].data["output"],"exact result")
        self.assertEqual(local_results[0].data["arguments"],{"filename":"arbitrary.txt"})

    def test_failed_or_changed_bytes_cannot_reuse_review(self):
        p=self.payload("document-processing-agent").model_copy(update={"enable_artifacts":True})
        with tempfile.TemporaryDirectory() as root:
            store=ArtifactStore(Path(root),p);file=Path(root)/"artifact.txt";file.write_text("first")
            files=[{"filename":"artifact.txt","file_path":str(file)}]
            store.review_artifacts(files,False,[{"issue":"wrong content"}],"request","template")
            with self.assertRaises(RuntimeError):store.ensure_reviewed("artifact.txt",str(file))
            with self.assertRaises(RuntimeError):store.review_artifacts(files,True,[],"request","template")
            file.write_text("corrected")
            store.review_artifacts(files,True,[],"request","template")
            result=store.register_file("artifact.txt",str(file))
            self.assertEqual(result["sha256"],hashlib.sha256(file.read_bytes()).hexdigest())
            file.write_text("modified after approval")
            with self.assertRaises(RuntimeError):store.ensure_reviewed("artifact.txt",str(file))
