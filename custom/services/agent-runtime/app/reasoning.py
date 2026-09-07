"""Decode explicitly configured inline reasoning before SDK message assembly.

Some OpenAI-compatible services put reasoning in content instead of the native
reasoning field. Their chat templates may prefill <think>, so the opening token
can be absent on the wire. Until the delimiter arrives that prefix is ambiguous
and must not be published as answer text. Native providers keep normal streaming.
"""
from __future__ import annotations

import copy
from contextlib import aclosing
import re

from agentscope.message import TextBlock, ToolCallBlock
from agentscope.model import OpenAIChatModel


class ThinkContent:
    def __init__(self):
        self.pending = ""
        self.answer_started = False
        self.scan = 0
        self.line_start = 0
        self.fence = None

    def boundary(self, *, final=False):
        text = self.pending
        # An explicit opening token identifies the protocol unambiguously.
        leading = len(text) - len(text.lstrip())
        if text[leading:].startswith("<think>"):
            end = text.find("</think>", max(leading + 7, self.scan))
            self.scan = max(leading + 7, len(text) - 7)
            return (leading + 7, end) if end >= 0 else None
        if "<think>".startswith(text[leading:]):
            return None
        # Prefilled opening tokens are absent from the response. Recognize only
        # a standalone closing delimiter outside quoted/fenced document text.
        while self.scan < len(text):
            end = text.find("\n", self.scan)
            if end < 0:
                self.scan = len(text)
                if not final:
                    return None
                end = len(text) - 1
            line = text[self.line_start:end + 1]
            stripped = line.strip()
            marker = re.match(r"^ {0,3}(`{3,}|~{3,})", line)
            if marker:
                token = marker.group(1)
                if self.fence is None:
                    self.fence = token
                elif token[0] == self.fence[0] and len(token) >= len(self.fence) and not line[marker.end():].strip():
                    self.fence = None
            elif self.fence is None and stripped == "</think>" and not line.startswith(("    ", "\t")):
                return 0, self.line_start + line.index("</think>")
            self.line_start = self.scan = end + 1
        return None

    def feed(self, text):
        if self.answer_started:
            return "", text
        self.pending += text
        boundary = self.boundary()
        if boundary is None:
            return "", ""
        start, end = boundary
        thought, answer = self.pending[start:end], self.pending[end + 8:]
        self.pending = ""
        self.answer_started = True
        return thought, answer

    def finish(self):
        # Revisit the final unterminated line once, including a closing token
        # immediately followed by the provider's terminal packet.
        self.scan = self.line_start
        boundary = self.boundary(final=True)
        if boundary is not None:
            start, end = boundary
            thought, answer = self.pending[start:end], self.pending[end + 8:]
            self.pending = ""
            self.answer_started = True
            return thought, answer
        text, self.pending = self.pending, ""
        self.answer_started = True
        if text.lstrip().startswith("<think>"):
            # Never promote an unfinished explicit thought to a final answer.
            from .models import ProviderIncomplete
            raise ProviderIncomplete("Provider reasoning block did not terminate")
        return "", text


class TaggedReasoningOpenAIChatModel(OpenAIChatModel):
    """Adapt a wire format, retaining the SDK's tools, usage and state handling."""

    async def _parse_stream_response(self, *args, **kwargs):
        decoder = ThinkContent()
        template = None
        text_id = None

        def decoded_chunk(thought, text):
            chunk = copy.copy(template)
            chunk.content, chunk.usage = [], None
            if thought:
                chunk.append_thinking(block_id=text_id + ":reasoning", thinking=thought)
            if text:
                chunk.append_text(block_id=text_id, text=text)
            return chunk

        async with aclosing(super()._parse_stream_response(*args, **kwargs)) as stream:
            async for chunk in stream:
                template = chunk
                remaining = []
                for block in chunk.content:
                    if isinstance(block, TextBlock):
                        text_id = text_id or block.id
                        thought, text = decoder.feed(block.text)
                        if thought or text:
                            yield decoded_chunk(thought, text)
                    else:
                        # Keep commentary before the tool boundary in its original
                        # order. Tool arguments are never fed into the text decoder.
                        if isinstance(block, ToolCallBlock) and decoder.pending:
                            thought, text = decoder.finish()
                            yield decoded_chunk(thought, text)
                        remaining.append(block)
                if remaining or chunk.usage:
                    passthrough = copy.copy(chunk)
                    passthrough.content = remaining
                    yield passthrough
        if decoder.pending:
            thought, text = decoder.finish()
            yield decoded_chunk(thought, text)
