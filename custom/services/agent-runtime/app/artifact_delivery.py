"""Code-only settlement of this run's changed outputs, independent of the LLM."""
import asyncio
import base64
import copy
import hashlib
import json
import shlex
import time
import uuid
from pathlib import PurePosixPath

import httpx

from .artifact_validation import validate_artifact
from .contracts import RunResult
from .control import ControlUnavailable
from .delivery import DeliveryError

INVENTORY = r'''
import hashlib,json,os,stat
root='/workspace/outputs'
items={}
if os.path.isdir(root) and not os.path.islink(root):
    for folder,dirs,files in os.walk(root,followlinks=False):
        dirs[:]=[d for d in dirs if not d.startswith('.') and not os.path.islink(os.path.join(folder,d))]
        for name in files:
            if name.startswith('.'): continue
            path=os.path.join(folder,name)
            if os.path.islink(path) or not stat.S_ISREG(os.stat(path,follow_symlinks=False).st_mode): continue
            with open(path,'rb') as f:
                digest=hashlib.file_digest(f,'sha256').hexdigest()
            items[path]=digest
print(json.dumps(items,ensure_ascii=False))
'''

async def inventory(backend):
    result = await backend.exec_shell('python3 -c ' + shlex.quote(INVENTORY), timeout=120)
    if result.exit_code:
        raise RuntimeError('Output inventory failed')
    return json.loads(result.stdout)

async def capture_baseline(payload, control, workspace):
    if payload.output_baseline is not None or not file_capable(payload):
        return
    baseline = await inventory(workspace.get_backend())
    result = await control.post('runs/baseline', output_baseline=baseline)
    payload.output_baseline = result['output_baseline']

def file_capable(payload):
    return payload.enable_artifacts and payload.runtime_config.agent_type != 'knowledge-qa'

async def commit_candidate(control, result):
    validation = await control.validate(result.model_dump(mode='json'))
    if validation.get('violations'):
        raise DeliveryError('Delivery requirements could not be satisfied: ' + json.dumps(validation['violations']))
    result = RunResult.model_validate(validation.get('result') or result.model_dump())
    committed = await control.commit(result.model_dump(mode='json'))
    return RunResult.model_validate(committed.get('result') or result.model_dump())

async def upload(payload, control, path, content, media):
    name = PurePosixPath(path).name
    digest = hashlib.sha256(content).hexdigest()
    metadata = {'tenant_id':payload.tenant_id,'user_id':payload.user_id,'session_id':payload.session_id,
                'assistant_message_id':payload.assistant_message_id,'run_id':payload.run_id,'owner_epoch':payload.owner_epoch,
                'file_token':str(uuid.uuid5(uuid.NAMESPACE_URL,payload.run_id+':'+path+':'+digest)),
                'filename':name,'file_type':PurePosixPath(path).suffix.lstrip('.'),'file_size':len(content),
                'sha256':digest,'content_type':media}
    encoded = base64.urlsafe_b64encode(json.dumps(metadata).encode()).decode().rstrip('=')
    # Go verifies persisted bytes and deduplicates this token. An uncertain
    # response leaves the durable finalization pending for reclaim.
    try:
        response = await control.client.post(payload.artifact_upload_url,content=content,
            headers={'Authorization':'Bearer '+payload.tool_callback_api_key,'X-WeKnora-Artifact-Metadata':encoded}, timeout=120)
        if response.status_code in (409,410):
            raise asyncio.CancelledError('Output publication ownership lost')
        response.raise_for_status()
    except httpx.HTTPError as exc:
        raise ControlUnavailable('control_unavailable','Output publication will resume') from exc
    return response.json()

async def finalize(payload, control, workspace):
    """Called after durable transition to finalizing, including worker recovery."""
    pending = payload.finalization
    result = RunResult.model_validate(pending['result'])
    artifacts, rejected = [], []
    if file_capable(payload) and payload.output_baseline is not None:
        # Freeze writers by removing the execution process boundary. Persistent
        # run volumes survive this close and are mounted by a fresh container.
        await workspace.get_backend().close()
        current = await inventory(workspace.get_backend())
        baseline = payload.output_baseline
        if baseline is None and current:
            raise RuntimeError('Output baseline is missing; cannot establish run ownership')
        for path, digest in sorted(current.items()):
            if (baseline or {}).get(path) == digest:
                continue
            try:
                content = await workspace.get_backend().read_file(path)
            except ValueError:
                rejected.append(PurePosixPath(path).name)
                continue
            if hashlib.sha256(content).hexdigest() != digest:
                raise RuntimeError('Output changed after execution stopped')
            try:
                if len(content) > result.artifact_limit_bytes:
                    raise ValueError('Output exceeds upload size limit')
                media = validate_artifact(PurePosixPath(path).name, content)
            except Exception:
                rejected.append(PurePosixPath(path).name)
                continue
            artifacts.append(await upload(payload, control, path, content, media))
    failed = bool(pending.get('error_code') or pending.get('error'))
    if failed or rejected:
        result.status = 'incomplete'
        result.failure_code = pending.get('error_code') or 'execution_failed'
    if failed:
        from .failures import error_message
        result.answer = error_message(result.failure_code)
        result.citations = []
        if artifacts:
            result.answer += '已保存的文件可下载查看。'
    elif not result.answer.strip():
        result.answer = '已生成以下文件。' if artifacts else ''
    if rejected:
        result.artifact_notice = '部分文件无法正常打开或超过大小限制，未发布：' + '、'.join(rejected)
        result.answer += '\n\n' + result.artifact_notice
    if not result.answer.strip():
        result.status, result.failure_code = 'incomplete', 'empty_response'
        result.answer = '这次没有生成有效回复，请稍后重试。'
    return await commit_candidate(control, result)

async def start_finalization(payload, control, result=None, error=None):
    from .failures import error_code
    candidate = result or RunResult(run_id=payload.run_id, answer='')
    value = {'result':candidate.model_dump(mode='json'), 'error':type(error).__name__ if error else '',
             'error_code':error_code(error) if error else ''}
    await control.post('runs/finalize', **value)
    payload.finalization = value
    payload.deadline_unix = time.time() + 600


from agentscope.middleware import MiddlewareBase
from .answer import ANSWER_TOOL

class OutputPresentation(MiddlewareBase):
    def __init__(self, payload, workspace):
        self.payload, self.workspace = payload, workspace
        self.revision = None
        self.current = {}

    async def on_model_call(self, agent, input_kwargs, next_handler):
        if file_capable(self.payload) and self.workspace is not None and self.payload.output_baseline is not None:
            revision = getattr(self.workspace, "output_revision", None)
            if revision is None or revision != self.revision:
                self.current = await inventory(self.workspace.get_backend())
                self.revision = revision
            current = self.current
            changed = [PurePosixPath(path).name for path, digest in current.items()
                       if self.payload.output_baseline.get(path) != digest]
            if changed:
                text = ("本轮已有新建或修改的交付文件。系统会在本轮结束后检查文件可用性，"
                        "并将通过检查的文件自动展示为回答末尾的产物卡片。最终正文只说明文件内容、使用方式与实际未完成事项。"
                        "不要描述内部处理步骤、工作路径、校验方法及结果；不要生成文件链接，用户通过产物卡片访问文件。当前待检查文件：" +
                        json.dumps(sorted(changed), ensure_ascii=False))
                # Attach presentation to the field it constrains. This changes
                # only the current provider schema, never dialogue or SDK state.
                tools = copy.deepcopy(input_kwargs["tools"])
                for tool in tools:
                    if tool["function"]["name"] == ANSWER_TOOL:
                        field = tool["function"]["parameters"]["properties"]["answer"]
                        field["description"] = field.get("description", "") + "\n" + text
                input_kwargs = {**input_kwargs, "tools": tools}
        return await next_handler(**input_kwargs)
