"""SDK read-before-edit state validated against the remote workspace.

AgentScope's default ToolContext checks local host mtimes. Workspace files
live in a different container, so cache validity must use that backend.
"""
import time

from agentscope.state import ToolContext
from pydantic import PrivateAttr


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
