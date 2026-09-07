"""Native SDK providers, with admission at every actual HTTP request boundary."""
from __future__ import annotations

import asyncio
import contextlib
import json
from contextlib import asynccontextmanager
from urllib.parse import urlsplit, urlunsplit

import httpx
from websockets.asyncio.client import connect
from agentscope.credential import AnthropicCredential, DeepSeekCredential, OpenAICredential
from agentscope.model import AnthropicChatModel, DeepSeekChatModel, OpenAIChatModel, OpenAIResponseModel

from .contracts import RunRequest
from .control import Control
from .reasoning import TaggedReasoningOpenAIChatModel


class Admission:
    """A live control connection fences the entire provider response stream.

    No lease handles are stored in an API process for a later callback: losing
    this connection cancels the provider request, and closing it releases quota.
    """
    def __init__(self, control: Control, model_role=""):
        self.control = control
        self.model_role = model_role
        self.estimated_input_tokens = 1
        self.max_output_tokens = 8192
        self.socket = None
        self.monitor = None
        self.owner = None

    async def acquire(self):
        parts = urlsplit(self.control.base_url + "/models/lease")
        url = urlunsplit(("wss" if parts.scheme == "https" else "ws", parts.netloc, parts.path, "", ""))
        self.socket = await connect(url, additional_headers={"Authorization": "Bearer " + self.control.payload.tool_callback_api_key},
                                    open_timeout=15, ping_interval=10, ping_timeout=10, max_size=65536)
        p = self.control.payload
        await self.socket.send(json.dumps({"run_id": p.run_id, "owner_epoch": p.owner_epoch, "model_role":self.model_role,
            "estimated_input_tokens":self.estimated_input_tokens,"max_output_tokens":self.max_output_tokens}))
        result = json.loads(await self.socket.recv())
        if result.get("status") != "acquired":
            await self.socket.close()
            raise RuntimeError(result.get("error", "Model admission denied"))
        self.owner = asyncio.current_task()
        self.monitor = asyncio.create_task(self._watch())

    async def _watch(self):
        try:
            async for message in self.socket:
                if json.loads(message).get("status") != "alive":
                    break
        finally:
            if self.owner is not None:
                self.owner.cancel("Model admission connection lost")

    async def close(self, status: int = 0, error: str = "", usage=None):
        self.owner = None
        if self.monitor:
            self.monitor.cancel()
            with contextlib.suppress(asyncio.CancelledError):
                await self.monitor
        if self.socket:
            try:
                await self.socket.send(json.dumps({"status": status, "error": error, "usage":usage or {}}))
                # Acknowledge circuit accounting and release before a next call.
                async with asyncio.timeout(10):
                    while True:
                        status = json.loads(await self.socket.recv()).get("status")
                        if status == "released":
                            break
                        if status != "alive":
                            raise RuntimeError("Model admission release was not acknowledged")
            finally:
                await self.socket.close()


class ProviderIncomplete(RuntimeError):
    pass


class StreamOutcome:
    """Observe wire termination and usage without rewriting provider messages."""
    def __init__(self, response):
        self.sse="text/event-stream" in response.headers.get("content-type", "")
        self.check=response.is_success and self.sse
        self.pending=b""
        self.completed=False
        self.usage={}

    def feed(self,chunk):
        if not self.check:return
        self.pending+=chunk
        while b"\n" in self.pending:
            line,self.pending=self.pending.split(b"\n",1)
            if not line.startswith(b"data:"):continue
            raw=line[5:].strip()
            if raw==b"[DONE]":continue
            try: item=json.loads(raw)
            except (ValueError,UnicodeDecodeError):continue
            for choice in item.get("choices",[]):
                reason=choice.get("finish_reason")
                if reason in ("stop","tool_calls","function_call"):self.completed=True
                elif reason:raise ProviderIncomplete("Provider terminated with "+reason)
            reason=item.get("delta",{}).get("stop_reason")
            if reason in ("end_turn","tool_use","stop_sequence"):self.completed=True
            elif reason:raise ProviderIncomplete("Provider terminated with "+reason)
            kind=item.get("type")
            if kind=="response.completed":self.completed=True
            if kind in ("error","response.failed","response.incomplete"):
                raise ProviderIncomplete("Provider stream ended without a complete response")
            usage=item.get("usage") or item.get("message",{}).get("usage") or item.get("response",{}).get("usage") or {}
            for target,aliases in {"input_tokens":("input_tokens","prompt_tokens"),"output_tokens":("output_tokens","completion_tokens")}.items():
                for key in aliases:
                    if key in usage:self.usage[target]=int(usage[key]);break

    def finish(self):
        if self.check and not self.completed:
            raise ProviderIncomplete("Provider stream closed without a terminal result")


class LeasedStream(httpx.AsyncByteStream):
    def __init__(self, response: httpx.Response, lease: Admission):
        self.response, self.lease = response, lease
        self.error = ""
        self.closed = False
        self.outcome = StreamOutcome(response)

    async def __aiter__(self):
        try:
            async for chunk in self.response.stream:
                self.outcome.feed(chunk)
                yield chunk
            self.outcome.finish()
        except BaseException as exc:
            self.error = type(exc).__name__
            raise
        finally:
            await self.aclose()

    async def aclose(self):
        if self.closed:
            return
        self.closed = True
        try:
            await self.response.aclose()
        finally:
            await self.lease.close(self.response.status_code, self.error, self.outcome.usage)


class GovernedTransport(httpx.AsyncBaseTransport):
    def __init__(self, control: Control, transport=None, lease_factory=Admission):
        self.control = control
        self.transport = transport or httpx.AsyncHTTPTransport(retries=0)
        self.lease_factory = lease_factory

    async def handle_async_request(self, request):
        if asyncio.current_task().cancelling():
            raise asyncio.CancelledError()
        lease = self.lease_factory(self.control)
        body = json.loads(request.content) if request.content else {}
        lease.estimated_input_tokens = input_budget(body)
        lease.max_output_tokens = int(body.get("max_tokens") or body.get("max_completion_tokens") or body.get("max_output_tokens") or 8192)
        await lease.acquire()
        try:
            response = await self.transport.handle_async_request(request)
        except BaseException as exc:
            await lease.close(error=type(exc).__name__)
            raise
        return httpx.Response(response.status_code, headers=response.headers,
                              stream=LeasedStream(response, lease), extensions=response.extensions)

    async def aclose(self):
        await self.transport.aclose()


def input_budget(value):
    """Conservative reservation, replaced by provider usage when available."""
    if isinstance(value, str):
        if value.startswith("data:image/"):
            return 16384
        return len(value.encode()) + 4
    if isinstance(value, list):
        return sum(input_budget(item) for item in value) + 4
    if isinstance(value, dict):
        if value.get("type") == "base64" and str(value.get("media_type", "")).startswith("image/"):
            return 16384
        return sum(len(key)+input_budget(item) for key,item in value.items())+4
    return 4


@asynccontextmanager
async def model_for(payload: RunRequest, control: Control, *, transport=None, model_role=""):
    config, runtime = payload.llm, payload.runtime_config
    classes = {
        "openai-chat": (OpenAIChatModel, OpenAICredential),
        "openai-responses": (OpenAIResponseModel, OpenAICredential),
        "anthropic": (AnthropicChatModel, AnthropicCredential),
    }
    cls, credential_cls = classes[config.protocol]
    if config.reasoning_format == "think-tags":
        if config.protocol != "openai-chat":
            raise ValueError("Tagged reasoning requires the OpenAI Chat wire protocol")
        cls = TaggedReasoningOpenAIChatModel
    if config.provider == "deepseek" and config.protocol == "openai-chat" and config.reasoning_format == "native":
        cls, credential_cls = DeepSeekChatModel, DeepSeekCredential
    values = {"max_tokens": runtime.max_completion_tokens or 8192}
    # A secondary model has its own capabilities. The primary agent's
    # thinking toggle must never enable unsupported reasoning on a VLM.
    thinking = runtime.thinking if model_role != "vision" else None
    if thinking is not None:
        values["thinking_enable"] = thinking
    if config.reasoning_effort and thinking is not False:
        values["reasoning_effort"] = config.reasoning_effort
    if "temperature" in cls.Parameters.model_fields:
        values["temperature"] = runtime.temperature if config.generation_policy != "gateway" and model_role != "vision" else None
    # Explicit provider parameters are validated; silently dropping them would
    # change configured model behavior and can invalidate thinking signatures.
    extra = dict(config.extra_body)
    if issubclass(cls, OpenAIChatModel) and thinking is not None:
        if config.thinking_control == "enable_thinking":
            extra["enable_thinking"] = thinking
        elif config.thinking_control == "thinking_type":
            extra["thinking"] = {"type": "enabled" if thinking else "disabled"}
        elif config.thinking_control == "chat_template_kwargs":
            extra["chat_template_kwargs"] = {"enable_thinking": thinking}
    for key in tuple(extra):
        if key in cls.Parameters.model_fields:
            values[key] = extra.pop(key)
    if extra and not issubclass(cls, OpenAIChatModel):
        raise ValueError(f"Unsupported parameters for {cls.__name__}: {sorted(extra)}")
    timeout = httpx.Timeout(runtime.llm_call_timeout or 300, connect=15, pool=15)
    async with httpx.AsyncClient(transport=transport or GovernedTransport(control, lease_factory=lambda c: Admission(c, model_role)), timeout=timeout) as client:
        kwargs = dict(credential=credential_cls(api_key=config.api_key or "no-auth", base_url=config.base_url or None),
                      model=config.model_name, parameters=cls.Parameters(**values), stream=True, max_retries=0,
                      context_size=runtime.max_context_tokens, client_kwargs={"http_client": client, "max_retries": 2,
                                                                            "default_headers": config.headers})
        if issubclass(cls, OpenAIChatModel):
            kwargs["extra_body"] = extra
        yield cls(**kwargs)
