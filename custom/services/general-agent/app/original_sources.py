"""Permission-bound original manifests; bytes are fetched only on tool demand."""
from __future__ import annotations

import asyncio
from dataclasses import asdict
import json
import threading


class OriginalSources:
    def __init__(self, payload, root):
        self.root = root
        self.sources = {source.id: (i, source) for i, source in enumerate(payload.original_input_files, 1)}
        self.locks = {}
        self.prepared = {}

    def manifest(self):
        # Storage/download URLs are credentials, never model context.
        return [{"file_id": s.id, "file_name": s.file_name, "file_type": s.file_type,
                 "file_size": s.file_size, "sha256": s.sha256, "source": s.source, "role": s.role,
                 "knowledge_id": s.knowledge_id, "knowledge_base_id": s.knowledge_base_id}
                for _, s in self.sources.values()]

    def prompt(self):
        if not self.sources:
            return ""
        return ("\n<original_file_sources>\nOriginal files available to this run. Use their knowledge IDs with "
                "retrieval/table tools, or materialize_file(file_id) when original bytes are needed in the workspace. "
                "Listed files have not been downloaded or inspected.\n" +
                json.dumps(self.manifest(), ensure_ascii=False) + "\n</original_file_sources>")

    async def materialize(self, args):
        from .runner import download_and_verify_original_input_file
        key = args["file_id"]
        if key not in self.sources:
            raise ValueError("file_id is not in this run's authorized source manifest")
        async with self.locks.setdefault(key, asyncio.Lock()):
            if key not in self.prepared:
                index, item = self.sources[key]
                stopped = threading.Event()
                task = asyncio.create_task(asyncio.to_thread(download_and_verify_original_input_file, item, self.root, index, stopped))
                try:
                    self.prepared[key] = await asyncio.shield(task)
                except asyncio.CancelledError:
                    stopped.set()
                    # The download thread must stop before run cleanup removes
                    # its directory; otherwise it can recreate orphan files.
                    await asyncio.gather(task, return_exceptions=True)
                    raise
            return {"success": True, **asdict(self.prepared[key])}
