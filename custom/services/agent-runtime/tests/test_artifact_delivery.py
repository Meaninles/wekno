import asyncio
import base64
import copy
import hashlib
import json
from types import SimpleNamespace

import httpx
import pytest
from agentscope.message import TextBlock, ToolCallBlock

from app.artifact_delivery import capture_baseline, finalize, start_finalization
from app.control import ControlUnavailable
from app.run import execute
from test_run import MemoryControl, ScriptedModel, request

class Files:
    def __init__(self, files): self.files=files; self.closed=0; self.commands=[]
    async def close(self): self.closed+=1
    async def exec_shell(self, command, **kwargs):
        self.commands.append(command)
        return SimpleNamespace(exit_code=0,stdout=json.dumps({p:hashlib.sha256(c).hexdigest() for p,c in self.files.items()}).encode())
    async def read_file(self,path): return self.files[path]

class FinalControl(MemoryControl):
    def __init__(self,payload):
        super().__init__(payload); self.client=self; self.uploads={}; self.attempts=[]; self.lose_ack=False
    async def post(self,path,**kwargs):
        if path=='runs/baseline': return {'output_baseline':kwargs['output_baseline']}
        if path=='runs/finalize': self.pending=copy.deepcopy(kwargs);return {'ok':True}
        assert path==self.payload.artifact_upload_url
        raw=kwargs['headers']['X-WeKnora-Artifact-Metadata']
        meta=json.loads(base64.urlsafe_b64decode(raw+'='*(-len(raw)%4)))
        assert hashlib.sha256(kwargs['content']).hexdigest()==meta['sha256']
        token=meta['file_token'];self.attempts.append(token)
        self.uploads[token]={**meta,'artifact_id':token,'download_url':'/api/v1/custom/agent-runtime/artifacts/'+token+'/download'}
        if self.lose_ack:
            self.lose_ack=False
            raise httpx.ReadError('Lost upload acknowledgement')
        return httpx.Response(200,json=self.uploads[token],request=httpx.Request('POST','http://fixture/upload'))
    async def validate(self,result):
        result={**result,'artifacts':list(self.uploads.values())}
        return {'result':result}
    async def commit(self,result):
        if self.committed is not None: assert self.committed==result
        self.committed=copy.deepcopy(result);return {'result':result}


def setup(files=None):
    payload=request(enable_artifacts=True,runtime_config={'agent_type':'document-processing-agent'})
    payload.artifact_upload_url='http://fixture/upload'
    control=FinalControl(payload);backend=Files(files or {})
    workspace=SimpleNamespace(get_backend=lambda:backend)
    return payload,control,backend,workspace

@pytest.mark.asyncio
async def test_only_new_changed_valid_files_are_published_without_answer_paths():
    p,c,b,w=setup({'/workspace/outputs/old.txt':b'old','/workspace/outputs/edit.txt':b'before'})
    await capture_baseline(p,c,w)
    baseline=copy.deepcopy(p.output_baseline)
    b.files.update({'/workspace/outputs/edit.txt':b'after','/workspace/outputs/new.txt':b'new',
                    '/workspace/outputs/empty.txt':b'','/workspace/outputs/broken.pptx':b'broken'})
    await capture_baseline(p,c,w)
    assert p.output_baseline==baseline
    candidate=await execute(p,c,ScriptedModel([[TextBlock(text='完成。')]]))
    assert c.committed is None
    await start_finalization(p,c,result=candidate)
    result=await finalize(p,c,w)
    assert {a.filename for a in result.artifacts}=={'edit.txt','new.txt'}
    assert result.status=='incomplete' and 'empty.txt' in result.artifact_notice and 'broken.pptx' in result.artifact_notice
    assert b.closed==1 and not c.calls
    assert 'old.txt' not in result.answer

@pytest.mark.asyncio
@pytest.mark.parametrize("limit",[50,100])
async def test_last_decision_delivers_file_with_no_publish_tool_or_extra_generation(limit):
    p,c,b,w=setup();p.runtime_config.max_iterations=limit;await capture_baseline(p,c,w)
    p.tools=[__import__('app.contracts',fromlist=['RuntimeToolSpec']).RuntimeToolSpec(name='lookup',parameters={'type':'object','properties':{}})]
    model=ScriptedModel([*[[ToolCallBlock(id=f'call-{i}',name='lookup',input='{}')] for i in range(limit-1)], [TextBlock(text='文件已生成。')]])
    candidate=await execute(p,c,model)
    b.files['/workspace/outputs/report.html']=b'<!DOCTYPE html><html><body>report</body></html>'
    await start_finalization(p,c,result=candidate)
    result=await finalize(p,c,w)
    assert len(model.requests)==limit and len(result.artifacts)==1
    assert result.status=='completed' and '/download' not in result.answer

@pytest.mark.asyncio
async def test_upload_and_commit_recovery_reuse_identity_without_model_calls():
    p,c,b,w=setup();await capture_baseline(p,c,w)
    b.files['/workspace/outputs/a.txt']=b'ready'
    await start_finalization(p,c,result=__import__('app.contracts',fromlist=['RunResult']).RunResult(run_id=p.run_id,answer='完成'))
    c.lose_ack=True
    with pytest.raises(ControlUnavailable):await finalize(p,c,w)
    assert c.committed is None and p.finalization
    result=await finalize(p,c,w)
    assert len(c.uploads)==1 and len(c.attempts)==2 and c.attempts[0]==c.attempts[1]
    assert (await finalize(p,c,w)).model_dump()==result.model_dump()

@pytest.mark.asyncio
async def test_execution_failure_preserves_valid_outputs_and_marks_incomplete():
    p,c,b,w=setup();await capture_baseline(p,c,w);b.files['/workspace/outputs/result.txt']=b'usable'
    await start_finalization(p,c,error=TimeoutError('private details'))
    result=await finalize(p,c,w)
    assert result.status=='incomplete' and len(result.artifacts)==1
    assert 'private details' not in result.answer

@pytest.mark.asyncio
async def test_knowledge_qa_and_unchanged_files_are_never_published():
    p,c,b,w=setup({'/workspace/outputs/a.txt':b'old'});await capture_baseline(p,c,w)
    from app.contracts import RunResult
    await start_finalization(p,c,result=RunResult(run_id=p.run_id,answer='42'))
    result=await finalize(p,c,w)
    assert not result.artifacts and result.answer=='42'
    p.runtime_config.agent_type='knowledge-qa';b.files['/workspace/outputs/new.txt']=b'new'
    c.committed=None
    assert not (await finalize(p,c,w)).artifacts


@pytest.mark.asyncio
async def test_same_bytes_at_a_new_path_are_new_but_post_snapshot_changes_cannot_publish():
    p,c,b,w=setup({'/workspace/outputs/old.txt':b'old'});await capture_baseline(p,c,w)
    b.files['/workspace/outputs/copy.txt']=b'old'
    from app.contracts import RunResult
    await start_finalization(p,c,result=RunResult(run_id=p.run_id,answer='完成'))
    assert [a.filename for a in (await finalize(p,c,w)).artifacts]==['copy.txt']
    p,c,b,w=setup();await capture_baseline(p,c,w);b.files['/workspace/outputs/a.txt']=b'initial'
    async def changed(path):return b'changed after snapshot'
    b.read_file=changed
    await start_finalization(p,c,result=RunResult(run_id=p.run_id,answer='完成'))
    with pytest.raises(RuntimeError,match='changed after execution'):
        await finalize(p,c,w)
    assert not c.uploads and c.committed is None


def test_inventory_excludes_inputs_hidden_files_and_directories(tmp_path):
    import subprocess,sys
    from app.artifact_delivery import INVENTORY
    root=tmp_path/'outputs';root.mkdir()
    (root/'a.txt').write_text('a');(root/'.hidden').write_text('hidden')
    (root/'.cache').mkdir();(root/'.cache'/'b.txt').write_text('cache')
    (tmp_path/'input.txt').write_text('input')
    script=INVENTORY.replace("root='/workspace/outputs'",'root='+repr(str(root)))
    result=json.loads(subprocess.check_output([sys.executable,'-c',script],text=True))
    assert list(result)==[str(root/'a.txt')]


@pytest.mark.asyncio
async def test_presentation_hint_tracks_actual_changes_without_polluting_context():
    from app.artifact_delivery import OutputPresentation
    p,c,b,w=setup({'/workspace/outputs/old.txt':b'old'})
    await capture_baseline(p,c,w)
    middleware=OutputPresentation(p,w)
    received=[]
    async def model(**kwargs):
        received.append(kwargs['messages']); return 'answer'
    original=[__import__('agentscope.message',fromlist=['Msg']).Msg(name='user',role='user',content=[TextBlock(text='hello')])]
    async def check():
        assert await middleware.on_model_call(None,{'messages':original},model)=='answer'
    await check()
    assert received[-1] is original
    b.files['/workspace/outputs/old.txt']=b'changed'
    await check()
    assert received[-1][0].name=='output_presentation' and len(original)==1
    b.files['/workspace/outputs/old.txt']=b'old'
    await check()
    assert received[-1] is original
    b.files['/workspace/outputs/new.txt']=b'new'
    await check()
    assert received[-1][0].name=='output_presentation'
    del b.files['/workspace/outputs/new.txt']
    await check()
    assert received[-1] is original
    assert not c.attempts and len(received)==5


@pytest.mark.asyncio
async def test_plain_completion_without_workspace_mutations_does_not_provision_or_scan():
    from app.contracts import RunResult
    p,c,b,w=setup()
    await start_finalization(p,c,result=RunResult(run_id=p.run_id,answer="不客气"))
    result=await finalize(p,c,w)
    assert result.answer=="不客气" and not result.artifacts and not b.commands and b.closed==0


@pytest.mark.asyncio
async def test_output_notice_reuses_scan_until_a_mutation_and_finalizer_always_reads_fresh():
    from app.artifact_delivery import OutputPresentation
    from app.contracts import RunResult
    from agentscope.message import Msg
    p,c,b,w=setup();await capture_baseline(p,c,w)
    w.output_revision=0
    middleware=OutputPresentation(p,w)
    messages=[Msg(name="system",role="system",content=[TextBlock(text="stable")])]
    async def receive(**kwargs): return kwargs["messages"]
    await middleware.on_model_call(None,{"messages":messages},receive)
    scans=len(b.commands)
    await middleware.on_model_call(None,{"messages":messages},receive)
    assert len(b.commands)==scans
    b.files["/workspace/outputs/created.txt"]=b"new"
    w.output_revision+=1
    projected=await middleware.on_model_call(None,{"messages":messages},receive)
    assert projected[0]==messages[0] and projected[1].name=="output_presentation"
    b.files["/workspace/outputs/late.txt"]=b"late writer"
    await start_finalization(p,c,result=RunResult(run_id=p.run_id,answer="完成"))
    result=await finalize(p,c,w)
    assert {a.filename for a in result.artifacts}=={"created.txt","late.txt"}
