"""SDK read-before-edit state validated against the remote workspace.

AgentScope's default ToolContext checks local host mtimes. Workspace files
live in a different container, so cache validity must use that backend.
"""
import time

from agentscope.state import ToolContext
from pydantic import PrivateAttr
from agentscope.tool import Edit, Write


class WorkspaceWrite(Write):
    async def call(self, file_path: str, content: str, _agent_state=None):
        result = await super().call(file_path=file_path, content=content, _agent_state=_agent_state)
        if result.is_last and str(result.state) != "error" and _agent_state is not None:
            text = content.replace("\r\n", "\n").replace("\r", "\n")
            await _agent_state.tool_context.cache_file(file_path,text.splitlines(keepends=True))
        return result


class WorkspaceEdit(Edit):
    async def call(self, file_path: str, old_string: str, new_string: str,
                   replace_all: bool = False, _agent_state=None):
        # Keep the expected content, not a post-write read that could silently
        # adopt an external change. The SDK still validates this snapshot
        # against the backend before it writes.
        entry = next((item for item in _agent_state.tool_context.read_file_cache
                      if item.file_path == file_path), None) if _agent_state is not None else None
        previous = "".join(entry.lines) if entry is not None else None
        result = await super().call(file_path=file_path, old_string=old_string,
                                    new_string=new_string, replace_all=replace_all,
                                    _agent_state=_agent_state)
        if result.is_last and str(result.state) != "error" and previous is not None:
            updated = previous.replace(old_string, new_string, -1 if replace_all else 1)
            updated = updated.replace("\r\n", "\n").replace("\r", "\n")
            await _agent_state.tool_context.cache_file(file_path, updated.splitlines(keepends=True))
        return result


class WorkspaceToolContext(ToolContext):
    _backend = PrivateAttr()

    @classmethod
    def bind(cls, current, backend):
        context = cls.model_validate(current.model_dump())
        context._backend = backend
        return context

    async def get_cache(self, file_path):
        entry = next((item for item in self.read_file_cache if item.file_path == file_path), None)
        if entry is None:
            return None
        try:
            raw = await self._backend.read_file(file_path)
            actual = raw.decode("utf-8", errors="replace").replace("\r\n", "\n").replace("\r", "\n")
        except (OSError, ValueError):
            actual = None
        self.read_file_cache.remove(entry)
        if actual != "".join(entry.lines):
            return None
        self.read_file_cache.append(entry)
        return entry

    async def cache_file(self, file_path, lines):
        if any("\x00" in line for line in lines):
            raise ValueError("This is a binary file. Use the file-format tools or a workspace library to inspect it, not a text read.")
        size = sum(len(line.encode("utf-8")) for line in lines) / 1024
        self.read_file_cache = [item for item in self.read_file_cache if item.file_path != file_path]
        if size > self.max_cache_bytes:
            return
        while self.read_file_cache and (len(self.read_file_cache) >= self.max_cache_files or
                sum(item.bytes for item in self.read_file_cache) + size > self.max_cache_bytes):
            self.read_file_cache.pop(0)
        # Use the SDK's public model validation to retain its serializable entry
        # schema without importing internal implementation classes.
        value = ToolContext.model_validate({"read_file_cache": [{"file_path": file_path,
            "lines": lines, "bytes": size, "updated_at": time.time()}]})
        self.read_file_cache.extend(value.read_file_cache)
