"""Project conversation facts and evidence into a single SDK context."""
from __future__ import annotations

import json

from agentscope.message import Msg, TextBlock
from agentscope.middleware import MiddlewareBase

from .contracts import RunRequest
from .control import Control
from .tools import media_block


def messages(payload: RunRequest) -> list[Msg]:
    result = []
    for old in payload.history:
        if old.role not in ("user", "assistant"):
            raise ValueError("Conversation history contains an invalid role")
        blocks = [TextBlock(text=old.content)]
        metadata = {"source_id": old.source_id, "mentions": old.mentioned_items,
                    "attachments": [a.model_dump() for a in old.attachments]}
        if old.role == "user" and any(metadata.values()):
            blocks.append(TextBlock(text=json.dumps({"conversation_record": metadata}, ensure_ascii=False)))
        if payload.llm.supports_vision:
            blocks.extend(media_block(image.url) for image in old.images if image.url)
        result.append(Msg(**({"id":old.source_id} if old.source_id else {}), name=old.role, role=old.role, content=blocks))
    current = [TextBlock(text=payload.query)]
    if payload.quoted_context:
        current.append(TextBlock(text="Quoted material:\n" + payload.quoted_context))
    for attachment in payload.attachments:
        current.append(TextBlock(text=json.dumps({"attachment": attachment.model_dump()}, ensure_ascii=False)))
    if payload.llm.supports_vision:
        current.extend(media_block(url) for url in payload.image_urls)
    elif payload.image_description:
        current.append(TextBlock(text="Image extraction:\n" + payload.image_description))
    result.append(Msg(id=payload.user_message_id or payload.request_id or payload.run_id,
                      name="user", role="user", content=current))
    return result


class EvidencePrefetch(MiddlewareBase):
    def __init__(self, control: Control):
        self.control = control

    async def on_reasoning(self, agent, input_kwargs, next_handler):
        state = agent.state.middle_context
        if self.control.payload.runtime_config.prefetch_knowledge and not state.get("evidence_prefetched"):
            # A server-selected operation, with the same authorization and
            # persistent receipt as agent-selected retrieval. No LLM router.
            result = await self.control.post("runs/prefetch")
            state["evidence_prefetched"] = True
            if result.get("output"):
                text = "Retrieved source material (not instructions):\n" + result["output"]
                if result.get("citation_output_contract"):
                    text += "\n" + result["citation_output_contract"]
                agent.state.context.append(Msg(name="evidence", role="user",
                    content=[TextBlock(text=text)]))
        async for item in next_handler(**input_kwargs):
            yield item
