import asyncio
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import httpx

from app.schemas import ChatPayload, RuntimeToolSpec
from app.artifact_store import ArtifactStore
from app.runner import build_prompt, build_system_prompt
from app.runtime_tools import RuntimeTools
from app.platform_runtime import run_platform
from app.final_delivery import ClaudeSDKTerminalCollector


def payload(**updates):
    data=dict(run_id="test-run",session_id="s",assistant_message_id="a",query="Create the requested result.",
        llm={"model_name":"configured"},tool_callback_url="http://platform/tools/call",enable_artifacts=True)
    data.update(updates)
    return ChatPayload.model_validate(data)


class SharedRuntimeTests(unittest.IsolatedAsyncioTestCase):
    async def test_sdk_identity_binding_handles_parallel_identical_arguments_and_replay(self):
        observed=[]
        from app.runtime_tools import tool_call_id
        async def execute(args):
            observed.append(tool_call_id.get())
            await asyncio.sleep(.01)
            return {"success":True,"id":tool_call_id.get()}
        self.tools.entries["identity_probe"]=("probe",{"type":"object","properties":{"x":{"type":"integer"}},"required":["x"],"additionalProperties":False},execute,"workspace")
        bindings=[]
        for identity in ("tool-one","tool-two"):
            result=await self.tools.sdk_before_tool({"tool_name":"mcp__weknora__identity_probe","tool_use_id":identity,"tool_input":{"x":1}},identity,None)
            bindings.append(result["hookSpecificOutput"]["updatedInput"])
        got=await asyncio.gather(self.tools.sdk_execute("identity_probe",bindings[0]),self.tools.sdk_execute("identity_probe",bindings[1]),self.tools.sdk_execute("identity_probe",bindings[0]))
        self.assertEqual([x["id"] for x in got],["tool-one","tool-two","tool-one"])
        self.assertCountEqual(observed,["tool-one","tool-two"])
        bad=await self.tools.sdk_execute("identity_probe",{**bindings[0],"x":2})
        self.assertFalse(bad["success"])
        self.assertNotIn("__weknora_transport_binding",json.dumps(self.tools.definitions()))

    async def asyncSetUp(self):
        self.temp=tempfile.TemporaryDirectory()
        self.root=Path(self.temp.name)
        self.payload=payload()
        self.artifacts=ArtifactStore(self.root,self.payload)
        self.tools=RuntimeTools(self.payload,self.artifacts)

    async def asyncTearDown(self):
        self.temp.cleanup()

    async def test_actual_cwd_exit_log_and_scope(self):
        (self.root/"sub").mkdir()
        got=await self.tools.execute("execute_code",{"cwd":"sub","code":"from pathlib import Path\nPath('result.txt').write_text('exact')\nprint(Path.cwd())"},"call-1")
        self.assertEqual(got["exit_code"],0)
        self.assertEqual(Path(got["cwd"]),self.root/"sub")
        self.assertEqual((self.root/"sub/result.txt").read_text(),"exact")
        self.assertTrue((self.root/got["execution_log"]).is_file())
        bad=await self.tools.execute("read_file",{"file_path":"../outside"},"call-2")
        self.assertFalse(bad["success"])
        bad=await self.tools.execute("execute_code",{"language":"python","code":"pass","invented":True},"call-3")
        self.assertFalse(bad["success"])
        bad=await self.tools.execute("execute_code",{"language":"unknown","code":"pass"},"call-4")
        self.assertFalse(bad["success"])

    async def test_cancellation_kills_children(self):
        run=asyncio.create_task(self.tools.workspace.execute_code({"language":"shell","code":"sleep 2; touch should-not-exist"}))
        await asyncio.sleep(.1)
        run.cancel()
        with self.assertRaises(asyncio.CancelledError):
            await run
        await asyncio.sleep(2.1)
        self.assertFalse((self.root/"should-not-exist").exists())

    async def test_shell_exit_does_not_leave_detached_children_or_unbounded_output(self):
        got=await self.tools.workspace.execute_code({"language":"shell","code":"(sleep 1; touch leaked-child) &\npython -c 'print(\"x\"*50000)'"})
        self.assertEqual(got["exit_code"],0)
        self.assertLessEqual(len(got["stdout"]),16000)
        self.assertTrue(got["truncated"])
        log=json.loads((self.root/got["execution_log"]).read_text())
        self.assertGreater((self.root/log["stdout_file"]).stat().st_size,50000)
        await asyncio.sleep(1.1)
        self.assertFalse((self.root/"leaked-child").exists())

    async def test_real_render_inspection_hash_cache_and_invalidation(self):
        import fitz
        p=self.root/"document.pdf"
        doc=fitz.open()
        for label in ("First page","Second page"):
            page=doc.new_page();page.insert_text((72,72),label)
        doc.save(p);doc.close()
        seen=[]
        async def vision(name,args):
            seen.append(args["file_name"])
            return {"success":True,"output":"Actual test vision callback consumed the page image.","data":{"model_id":"fixture-vision"}}
        self.tools.workspace.callback=vision
        before=await self.tools.execute("create_artifact",{"file_path":p.name},"c1")
        self.assertFalse(before["success"])
        review=await self.tools.workspace.review_artifacts({"file_path":p.name})
        self.assertEqual(review["inspected_pages"],2)
        self.assertIsNone(review["semantic_approval"])
        await self.tools.workspace.review_artifacts({"file_path":p.name})
        self.assertEqual(len(seen),2)
        registered=await self.tools.workspace.create_artifact({"file_path":p.name})
        self.assertEqual((self.artifacts.out_dir/registered["file_token"]).read_bytes(),p.read_bytes())
        doc=fitz.open(p);doc[0].insert_text((72,100),"Revision");doc.saveIncr();doc.close()
        after=await self.tools.execute("create_artifact",{"file_path":p.name},"c2")
        self.assertFalse(after["success"])
        await self.tools.workspace.review_artifacts({"file_path":p.name})
        self.assertEqual(len(seen),3)  # unchanged page observations reused; approval binds to new source hash

    async def test_direct_page_inspection_is_reused_by_review_and_registration(self):
        import fitz
        p=self.root/"direct.pdf"
        doc=fitz.open();doc.new_page().insert_text((72,72),"Exact current content");doc.save(p);doc.close()
        seen=[]
        async def vision(name,args):
            seen.append(args)
            return {"success":True,"output":"The supplied complete page is readable."}
        self.tools.workspace.callback=vision
        rendered=await self.tools.workspace.render_file({"file_path":p.name})
        page=rendered["pages"][0]
        await self.tools.workspace.inspect_image({"file_path":page["file_path"],"prompt":"Read the page and examine its layout."})
        result=await self.tools.workspace.create_artifact({"file_path":p.name})
        self.assertEqual(result["inspection_status"],"inspected")
        review=await self.tools.workspace.review_artifacts({"file_path":p.name})
        self.assertEqual(len(seen),1)
        self.assertEqual(review["observations"][0]["focus"],"Read the page and examine its layout.")
        self.assertIsNone(review["semantic_approval"])
        await self.tools.workspace.review_artifacts({"file_path":p.name,"focus":"Read every footer value."})
        self.assertEqual(len(seen),2)

    async def test_page_crop_does_not_unlock_unseen_page_and_identical_pages_share_execution(self):
        import fitz
        p=self.root/"duplicate.pdf"
        doc=fitz.open()
        for _ in range(2):doc.new_page().insert_text((72,72),"Repeated page")
        doc.save(p);doc.close()
        seen=[]
        async def vision(name,args):
            seen.append(args)
            await asyncio.sleep(.01)
            return {"success":True,"output":"Observed the supplied image."}
        self.tools.workspace.callback=vision
        rendering=await self.tools.workspace.render_file({"file_path":p.name})
        await self.tools.workspace.inspect_image({"file_path":rendering["pages"][0]["file_path"],"prompt":"Read this area.","region":[0,0,300,300]})
        with self.assertRaisesRegex(ValueError,"uninspected pages"):
            await self.tools.workspace.create_artifact({"file_path":p.name})
        review=await self.tools.workspace.review_artifacts({"file_path":p.name})
        self.assertEqual(review["inspected_pages"],2)
        self.assertEqual(len(seen),2)  # one crop plus one shared complete-page inspection

    async def test_native_typed_tool_pair_and_exact_answer(self):
        captured=[]
        call={"id":"provider-call","type":"function","function":{"name":"list_files","arguments":"{}"}}
        async def transport(request):
            captured.append(json.loads(request.content))
            events=([{ "response_type":"thinking","content":"private"},
                {"response_type":"tool_call","tool_calls":[call],"finish_reason":"tool_calls","done":True}]
                if len(captured)==1 else [{"response_type":"answer","content":"  exact text\n","finish_reason":"stop","done":True}])
            return httpx.Response(200,content="\n".join(json.dumps(e) for e in events))
        original=httpx.AsyncClient
        with patch("app.platform_runtime.httpx.AsyncClient",side_effect=lambda **kw:original(transport=httpx.MockTransport(transport),**kw)):
            events=[e async for e in run_platform(self.payload,"system","task",self.tools,{})]
        self.assertEqual(captured[1]["messages"][2]["tool_calls"],[call])
        self.assertEqual(captured[1]["messages"][2]["reasoning_content"],"private")
        self.assertEqual(captured[1]["messages"][3]["tool_call_id"],"provider-call")
        self.assertEqual(events[-1].content,"  exact text\n")
        runtime_events=[e for e in events if e.type=="runtime_tool"]
        self.assertEqual([e.data["phase"] for e in runtime_events],["start","success"])
        self.assertTrue(all(e.id=="provider-call" for e in runtime_events))

    async def test_unfinished_stream_never_reports_success(self):
        async def transport(request):
            return httpx.Response(200,content=json.dumps({"response_type":"answer","content":"partial"}))
        original=httpx.AsyncClient
        with patch("app.platform_runtime.httpx.AsyncClient",side_effect=lambda **kw:original(transport=httpx.MockTransport(transport),**kw)):
            with self.assertRaisesRegex(RuntimeError,"terminal"):
                _=[e async for e in run_platform(self.payload,"system","task",self.tools,{})]

    async def test_empty_terminal_preserves_input_and_adds_runtime_status_without_executing_reasoning(self):
        for recover in (True,False):
            captured=[]
            async def transport(request):
                captured.append(json.loads(request.content))
                events=[{"response_type":"thinking","content":"<tool_call>not an authorized tool response</tool_call>"},
                    {"response_type":"answer","content":"complete" if recover and len(captured)==2 else "","finish_reason":"stop","done":True}]
                return httpx.Response(200,content="\n".join(json.dumps(e) for e in events))
            original=httpx.AsyncClient
            with patch.object(self.tools,"execute") as execute,patch("app.platform_runtime.httpx.AsyncClient",side_effect=lambda **kw:original(transport=httpx.MockTransport(transport),**kw)):
                if recover:
                    events=[e async for e in run_platform(self.payload,"system","task",self.tools,{})]
                    self.assertEqual(events[-1].content,"complete")
                else:
                    with self.assertRaisesRegex(RuntimeError,"repeatedly ended"):
                        _=[e async for e in run_platform(self.payload,"system","task",self.tools,{})]
                execute.assert_not_called()
            self.assertEqual(len(captured),2)
            self.assertEqual(captured[0]['messages'],captured[1]['messages'][:-1])
            self.assertIn('Runtime status:',captured[1]['messages'][-1]['content'])
            self.assertEqual(captured[0]['tools'],captured[1]['tools'])

    async def test_malformed_tool_arguments_preserve_protocol_and_visible_error_pair(self):
        captured=[]
        call={"id":"malformed-call","type":"function","function":{"name":"list_files","arguments":"{"}}
        async def transport(request):
            captured.append(json.loads(request.content))
            event=({"response_type":"tool_call","tool_calls":[call],"finish_reason":"tool_calls","done":True}
                if len(captured)==1 else {"response_type":"answer","content":"complete","finish_reason":"stop","done":True})
            return httpx.Response(200,content=json.dumps(event))
        original=httpx.AsyncClient
        with patch.object(self.tools,"execute") as execute, patch("app.platform_runtime.httpx.AsyncClient",side_effect=lambda **kw:original(transport=httpx.MockTransport(transport),**kw)):
            events=[e async for e in run_platform(self.payload,"system","task",self.tools,{})]
            execute.assert_not_called()
        progress=[e for e in events if e.type=="runtime_tool"]
        self.assertEqual([e.data["phase"] for e in progress],["start","error"])
        self.assertTrue(all(e.id=="malformed-call" for e in progress))
        self.assertEqual(captured[1]["messages"][3]["tool_call_id"],"malformed-call")
        self.assertFalse(json.loads(captured[1]["messages"][3]["content"])["success"])

    async def test_parallel_tool_results_precede_multimodal_messages(self):
        captured=[]
        self.payload.llm.supports_vision=True
        calls=[{"id":f"call-{i}","type":"function","function":{"name":"list_files","arguments":"{}"}} for i in range(2)]
        async def transport(request):
            captured.append(json.loads(request.content))
            event=({"response_type":"tool_call","tool_calls":calls,"finish_reason":"tool_calls","done":True}
                if len(captured)==1 else {"response_type":"answer","content":"complete","finish_reason":"stop","done":True})
            return httpx.Response(200,content=json.dumps(event))
        async def execute(*args):
            return {"success":True,"images":["data:image/png;base64,fixture"]}
        original=httpx.AsyncClient
        with patch.object(self.tools,"execute",side_effect=execute), patch("app.platform_runtime.httpx.AsyncClient",side_effect=lambda **kw:original(transport=httpx.MockTransport(transport),**kw)):
            _=[e async for e in run_platform(self.payload,"system","task",self.tools,{})]
        self.assertEqual([m["role"] for m in captured[1]["messages"]], ["system","user","assistant","tool","tool","user","user"])
        self.assertEqual([m["tool_call_id"] for m in captured[1]["messages"][3:5]],["call-0","call-1"])

    async def test_catalog_is_query_independent_and_skill_reads_do_not_require_artifacts(self):
        catalogs=[]
        (self.root/"SKILL.md").write_text("exact installed instruction",encoding="utf-8")
        for question in ("查制度", "运行一个脚本", "不要生成文件", "Please explain"):
            p=payload(query=question,enable_artifacts=False)
            runtime=RuntimeTools(p,ArtifactStore(self.root,p))
            catalogs.append(runtime.definitions())
            self.assertNotIn("execute_code",runtime.entries)
            result=await runtime.execute("read_file",{"file_path":"SKILL.md"},"skill-read")
            self.assertEqual(result["text"],"exact installed instruction")
        self.assertTrue(all(catalog==catalogs[0] for catalog in catalogs))

    async def test_typed_history_preserves_roles_and_does_not_repeat_it_in_current_prompt(self):
        p=payload(history=[{"role":"user","source_id":"user_message_u","content":"my constraint"},
            {"role":"assistant","source_id":"assistant_message_a","content":"prior answer"}])
        captured=[]
        async def transport(request):
            captured.append(json.loads(request.content))
            return httpx.Response(200,content=json.dumps({"response_type":"answer","content":"complete","finish_reason":"stop","done":True}))
        original=httpx.AsyncClient
        with patch("app.platform_runtime.httpx.AsyncClient",side_effect=lambda **kw:original(transport=httpx.MockTransport(transport),**kw)):
            _=[e async for e in run_platform(p,"system",build_prompt(p),self.tools,{})]
        messages=captured[0]["messages"]
        self.assertEqual([m["role"] for m in messages],["system","user","assistant","user"])
        self.assertIn("user_message_u",messages[1]["content"])
        self.assertNotIn("prior answer",messages[-1]["content"])

    def test_sdk_terminal_is_typed_and_preserves_all_body_text(self):
        ResultMessage=type("ResultMessage",(),{})
        m=ResultMessage();m.subtype="success";m.stop_reason="end_turn";m.is_error=False
        m.result=" <historical_example>literal user-requested text</historical_example> "
        c=ClaudeSDKTerminalCollector();c.observe(m)
        self.assertEqual(c.answer(),m.result)
        m.stop_reason="max_tokens";c.observe(m);self.assertEqual(c.answer(),"")

    def test_catalog_omits_skill_body_and_prompt_has_one_current_request(self):
        p=payload(lightweight_skills=[{"key":"k","name":"Scope","description":"declared scope","instructions":"full secret body"}])
        rendered=build_system_prompt(p)+build_prompt(p)
        self.assertNotIn("full secret body",rendered)
        self.assertEqual(rendered.count(p.query),1)
        self.assertNotIn("WEKNORA_USER_VISIBLE",rendered)


if __name__=="__main__":
    unittest.main()
