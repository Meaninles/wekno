"""The SDK's single final-answer contract, shared by delivery and streaming."""
from agentscope.message import ToolCallBlock, ToolResultBlock
from agentscope.middleware import MiddlewareBase
from typing import Any
from pydantic import BaseModel, ConfigDict, Field, field_validator
from pydantic_core import from_json

# Built into the pinned AgentScope SDK; this is not a business/workspace tool.
ANSWER_TOOL = "GenerateStructuredOutput"
CITATIONS_SCHEMA = {"type": "array", "items": {"type": "object", "properties": {"text": {"type": "string", "description": "Exact contiguous text copied from answer; insert after its first occurrence."}, "source_ids": {"type": "array", "items": {"type": "string"}, "description": "Supporting IDs from CURRENT_RUN_SOURCES for this run."}}, "required": ["text", "source_ids"], "additionalProperties": False}}


class AnswerProtocol(MiddlewareBase):
    async def on_model_call(self, agent, input_kwargs, next_handler):
        # Advertise the intended citation schema only to the provider. The SDK
        # accepts raw citation data so its JSON-schema validator cannot initiate
        # a repair decision for entries that the backend must simply discard.
        tools = [{**tool, "function": {**tool["function"], "description":
            "Submit the final answer and end the run. Call immediately when the user request has been answered "
            "or cannot be fulfilled. Only take further business actions if they are necessary to answer the request."}}
            if tool["function"]["name"] == ANSWER_TOOL else tool for tool in input_kwargs["tools"]]
        for tool in tools:
            function = tool["function"]
            if function["name"] == ANSWER_TOOL:
                parameters = function["parameters"]
                function["parameters"] = {**parameters, "properties": {**parameters["properties"], "citations": CITATIONS_SCHEMA}}
        return await next_handler(**{**input_kwargs, "tools": tools})


class DeliveryError(RuntimeError):
    pass


class Answer(BaseModel):
    model_config = ConfigDict(extra="forbid", strict=True)
    answer: str = Field(description="Complete coherent Markdown answer in the requested language, without inline citation tags or numeric citation markers. Include necessary limitations.")
    # Describe the intended schema, but leave malformed citation data to the
    # deterministic backend filter instead of triggering SDK/model repair.
    citations: Any = Field(default_factory=list)

    @field_validator("answer")
    @classmethod
    def nonempty(cls, value):
        if not value.strip():
            raise ValueError("Empty response")
        return value


def decision_calls(message):
    """SDK aggregates tool rounds in one assistant message."""
    if message is None or message.role != "assistant":
        return []
    start = max((i + 1 for i, b in enumerate(message.content) if isinstance(b, ToolResultBlock)), default=0)
    return [b for b in message.content[start:] if isinstance(b, ToolCallBlock)]


def validate_decision(message):
    calls = decision_calls(message)
    if not calls:
        raise DeliveryError("Final answer was not submitted through the answer protocol")
    final = [c for c in calls if c.name == ANSWER_TOOL]
    if final:
        if len(calls) != 1:
            raise DeliveryError("Final answer must be submitted separately from business actions")
        try:
            Answer.model_validate_json(final[0].input)
        except ValueError as exc:
            raise DeliveryError("Invalid final answer structure or empty response") from exc


class AnswerStream:
    """Decode only the final answer field; never interpret natural-language text."""
    def __init__(self, max_bytes):
        self.raw = bytearray()
        self.text = ""
        self.max_bytes = max_bytes

    def feed(self, delta):
        self.raw.extend(delta.encode())
        if len(self.raw) > self.max_bytes:
            raise DeliveryError("Final answer exceeds the response limit")
        try:
            value = from_json(self.raw, allow_partial="trailing-strings")
        except ValueError:
            return ""
        text = value.get("answer") if isinstance(value, dict) else None
        if not isinstance(text, str):
            return ""
        if not text.startswith(self.text):
            raise DeliveryError("Final answer field changed within one response")
        delta, self.text = text[len(self.text):], text
        return delta
