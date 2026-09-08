"""The sole Agent construction and execution entry point."""
from __future__ import annotations

import asyncio
import contextlib
import time
from collections.abc import Sequence

from agentscope.agent import Agent, ContextConfig, ModelConfig, ReActConfig
from agentscope.model import ChatModelBase
from agentscope.permission import PermissionMode
from agentscope.tool import ToolBase, Toolkit

from .context import messages, system_prompt
from .contracts import RunRequest, RunResult
from .control import Control
from .delivery import Delivery, DeliveryError
from .events import Events
from .state import Lifecycle, restore
from .tools import BusinessTool
from .vision import Vision
from .budget import IterationBudget
from .artifact_delivery import OutputPresentation
from .context_projection import ContextProjection
from .answer import Answer, AnswerProtocol


async def execute(payload: RunRequest, control: Control, model: ChatModelBase,
                  *, extra_tools: Sequence[ToolBase] = (), skills=(), offloader=None) -> RunResult:
    """Run once through the SDK; recovery enters here with the same durable state."""
    lifecycle = Lifecycle(control)
    lifecycle.workspace = offloader if hasattr(offloader, "prepare_tool") else None
    delivery = Delivery(control, lifecycle)
    events = Events(control)
    lifecycle.events = events
    delivery.events = events
    state = restore(payload.checkpoint, payload.run_id)
    if offloader is not None and hasattr(offloader, "get_backend"):
        from .file_context import WorkspaceToolContext
        state.tool_context = WorkspaceToolContext.bind(state.tool_context, offloader.get_backend())
    # Workspace commands run in a dedicated restricted container. Business
    # mutations still pass through Go's live approval and authorization gates.
    state.permission_context.mode = PermissionMode.BYPASS
    data_backend = (offloader.get_backend() if payload.enable_artifacts
                    and payload.runtime_config.agent_type != "knowledge-qa"
                    and offloader is not None and hasattr(offloader, "get_backend") else None)
    concurrency = asyncio.Semaphore(4)
    toolkit = Toolkit(tools=[BusinessTool(spec, control, data_backend, concurrency) for spec in payload.tools] + list(extra_tools),
                      skills_or_loaders=skills or None)
    agent = Agent(
        name="weknora", system_prompt=system_prompt(payload), model=model,
        toolkit=toolkit, state=state, offloader=offloader,
        middlewares=[ContextProjection(data_backend), OutputPresentation(payload, offloader), Vision(control), AnswerProtocol(), IterationBudget(payload.runtime_config.max_iterations, control), lifecycle, delivery],
        model_config=ModelConfig(max_retries=0, fallback_model=None),
        context_config=ContextConfig(tool_result_limit=4096),
        # SDK's one structured-output grace decision is inside our public limit.
        react_config=ReActConfig(max_iters=payload.runtime_config.max_iterations - 1,
                                structured_output_grace_iters=1,
                                interruption_raise_cancelled_error=True),
    )
    inputs = None if payload.checkpoint else messages(payload)
    remaining = payload.deadline_unix - time.time()
    if remaining <= 0:
        raise TimeoutError("Run deadline has expired")
    owner = asyncio.current_task()
    pump = asyncio.create_task(events.pump())

    def pump_finished(task):
        if not task.cancelled() and task.exception() is not None and owner is not None:
            owner.cancel("Event persistence failed")

    pump.add_done_callback(pump_finished)
    try:
        async with asyncio.timeout(remaining):
            stream = agent.reply_stream(inputs=inputs, structured_schema=Answer, yield_final_msg=True)
            try:
                async for item in stream:
                    # Completion is the committed Go result, never an SDK
                    # end notification with a still-uncommitted message.
                    if getattr(item, "type", "") != "REPLY_END":
                        await events.sdk(item)
            finally:
                await stream.aclose()
            await events.flush()
        if delivery.result is None:
            raise DeliveryError("SDK ended without a final candidate")
        return delivery.result
    finally:
        pump.cancel()
        with contextlib.suppress(asyncio.CancelledError):
            await pump
