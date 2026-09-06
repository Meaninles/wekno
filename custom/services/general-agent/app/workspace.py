"""Run-scoped file execution and byte-bound inspection, shared by both adapters."""
from __future__ import annotations

import asyncio
import hashlib
import json
import os
from pathlib import Path
import signal
import sys
import uuid


def schema(properties, required=()):
    return {"type": "object", "properties": properties, "required": list(required), "additionalProperties": False}


STR = {"type": "string", "minLength": 1}
PATH = {"file_path": STR}


class Workspace:
    def __init__(self, artifacts, callback):
        self.artifacts = artifacts
        self.root = artifacts.run_dir.resolve()
        self.callback = callback
        self.page_inspections = {}
        self.inspection_calls = {}
        self.renderings = {}
        self.file_lock = asyncio.Lock()

    def path(self, value, *, exists=True):
        p = Path(value)
        p = (p if p.is_absolute() else self.root / p).resolve()
        if p != self.root and self.root not in p.parents:
            raise ValueError("file_path must be inside this run's working directory")
        if exists and not p.exists():
            raise ValueError(f"path does not exist: {p.relative_to(self.root)}")
        return p

    def metadata(self, path):
        p = self.path(path)
        if not p.is_file():
            raise ValueError("expected a file")
        return {"file_path": str(p.relative_to(self.root)), "filename": p.name,
                "sha256": self.file_digest(p), "file_size": p.stat().st_size,
                "file_type": p.suffix.lstrip(".").lower()}

    @staticmethod
    def file_digest(path):
        with path.open("rb") as stream:
            return hashlib.file_digest(stream, "sha256").hexdigest()

    async def process(self, argv, cwd, timeout=120):
        log = self.root / "execution-logs" / (str(uuid.uuid4()) + ".json")
        log.parent.mkdir(exist_ok=True)
        stdout_path, stderr_path = log.with_suffix(".stdout.txt"), log.with_suffix(".stderr.txt")
        # Stream to files instead of accumulating unbounded PIPE buffers. A
        # foreground call owns its process group even if its shell exits first.
        with stdout_path.open("wb") as stdout_file, stderr_path.open("wb") as stderr_file:
            proc = await asyncio.create_subprocess_exec(*argv, cwd=str(cwd),
                stdout=stdout_file, stderr=stderr_file, start_new_session=True)
            try:
                await asyncio.wait_for(proc.wait(), timeout)
            finally:
                try:
                    os.killpg(proc.pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
                await proc.wait()
                log.write_text(json.dumps({"argv": argv, "cwd": str(cwd), "exit_code": proc.returncode,
                    "stdout_file": str(stdout_path.relative_to(self.root)),
                    "stderr_file": str(stderr_path.relative_to(self.root))}, ensure_ascii=False),encoding="utf-8")
        with stdout_path.open("rb") as f:
            stdout=f.read(16000)
        with stderr_path.open("rb") as f:
            stderr=f.read(8000)
        return {"cwd": str(cwd), "exit_code": proc.returncode,
            "stdout": stdout.decode("utf-8", "replace"), "stderr": stderr.decode("utf-8", "replace"),
            "truncated": stdout_path.stat().st_size>16000 or stderr_path.stat().st_size>8000,
            "execution_log": str(log.relative_to(self.root))}

    async def execute_code(self, args):
        cwd = self.path(args.get("cwd", "."))
        if not cwd.is_dir():
            raise ValueError("cwd must be a directory")
        language = args.get("language", "python")
        suffix, program = {"python": (".py", sys.executable), "shell": (".sh", "/bin/bash")}[language]
        script = self.root / "execution-scripts" / (str(uuid.uuid4()) + suffix)
        script.parent.mkdir(exist_ok=True)
        script.write_text(args["code"], encoding="utf-8")
        return await self.process([program, str(script)], cwd, args.get("timeout_seconds", 120))

    async def list_files(self, args):
        root = self.path(args.get("directory", "."))
        entries = sorted(p for p in root.iterdir() if p.name != "claude-config")
        offset = args.get("offset", 0)
        return {"directory": str(root), "total": len(entries), "offset": offset,
            "entries": [{"path": str(p.relative_to(self.root)), "kind": "directory" if p.is_dir() else "file",
                         "size": p.stat().st_size if p.is_file() else None} for p in entries[offset:offset+100]],
            "next_offset": offset+100 if len(entries)>offset+100 else None}

    async def read_file(self, args):
        p = self.path(args["file_path"])
        offset = args.get("offset", 0)
        # Bound memory even for large archived results. Preserve exact newlines
        # so a subsequent hash-checked edit matches what the reader observed.
        skipped = 0
        with p.open("r", encoding="utf-8", newline="") as stream:
            while skipped < offset:
                chunk = stream.read(min(65536, offset-skipped))
                if not chunk:
                    break
                skipped += len(chunk)
            text = stream.read(16001)
        more = len(text) > 16000
        return {**self.metadata(p), "text": text[:16000], "offset": offset,
            "total_characters": None if more else skipped+len(text),
            "next_offset": offset+16000 if more else None}

    async def write_file(self, args):
        async with self.file_lock:
            p = self.path(args["file_path"], exists=False)
            if p.exists():
                raise ValueError("file already exists; use edit_file with the current SHA256")
            p.parent.mkdir(parents=True, exist_ok=True)
            with p.open("x", encoding="utf-8", newline="") as stream:
                stream.write(args["content"])
            return {"success": True, **self.metadata(p)}

    async def edit_file(self, args):
        async with self.file_lock:
            p = self.path(args["file_path"])
            if self.file_digest(p) != args["expected_sha256"]:
                raise ValueError("file changed; read its current content and SHA256 before editing")
            original = p.read_bytes().decode("utf-8")
            old, new = args["old_text"], args["new_text"]
            if old:
                if original.count(old) != 1:
                    raise ValueError("old_text must match exactly once; include enough surrounding text")
                updated = original.replace(old, new, 1)
            else:
                updated = original + new
            # Replace atomically; conversion or readers never observe half a write.
            temporary = p.with_name(p.name + "." + str(uuid.uuid4()) + ".tmp")
            try:
                temporary.write_bytes(updated.encode("utf-8"))
                temporary.replace(p)
            finally:
                temporary.unlink(missing_ok=True)
            return {"success": True, **self.metadata(p)}

    async def convert_file(self, args):
        src = self.path(args["file_path"])
        fmt = args["format"]
        meta = self.metadata(src)
        out = self.root / "converted" / meta["sha256"] / fmt
        out.mkdir(parents=True, exist_ok=True)
        target = out / (src.stem + "." + fmt)
        if not target.exists():
            profile = out / ("lo-profile-" + str(uuid.uuid4()))
            result = await self.process(["libreoffice", "-env:UserInstallation="+profile.as_uri(),
                "--headless", "--convert-to", fmt, "--outdir", str(out), str(src)], self.root, 180)
            if result["exit_code"] != 0 or not target.is_file():
                raise ValueError("conversion failed: " + json.dumps(result, ensure_ascii=False))
        return {"source": meta, "output": self.metadata(target), "converter": "LibreOffice"}

    async def render_file(self, args):
        import fitz
        p = self.path(args["file_path"])
        meta = self.metadata(p)
        cached = self.renderings.get(meta["sha256"])
        if cached and all(self.path(page["file_path"],exists=False).is_file() and self.metadata(page["file_path"])["sha256"]==page["sha256"] for page in cached["pages"]):
            return {**cached, "cached": True}
        if p.suffix.lower() in {".png", ".jpg", ".jpeg", ".webp"}:
            result = {"source": meta, "pages": [{"page": 1, **meta}], "page_count": 1, "renderer": "original image"}
        else:
            pdf = p
            converter = "original PDF"
            if p.suffix.lower() != ".pdf":
                converted = await self.convert_file({"file_path": str(p), "format": "pdf"})
                pdf = self.path(converted["output"]["file_path"])
                converter = "LibreOffice"
            out = self.root / "rendered" / meta["sha256"]
            out.mkdir(parents=True, exist_ok=True)
            pages = []
            with fitz.open(pdf) as doc:
                for i, page in enumerate(doc):
                    dest = out / f"page-{i+1}.png"
                    page.get_pixmap(matrix=fitz.Matrix(1.3, 1.3), alpha=False).save(dest)
                    text_path = out / f"page-{i+1}.txt"
                    text_path.write_text(page.get_text(), encoding="utf-8")
                    pages.append({"page": i+1, **self.metadata(dest), "text_file": str(text_path.relative_to(self.root)),
                        "text_characters": len(page.get_text()), "width_points": page.rect.width, "height_points": page.rect.height})
            result = {"source": meta, "pages": pages, "page_count": len(pages), "renderer": "MuPDF", "converter": converter,
                "layout_reference": "rendered by the named converter; not an independent layout truth"}
        self.renderings[meta["sha256"]] = result
        return result

    async def inspect_image(self, args):
        from .image_inspection import prepare_image
        p = self.path(args["file_path"])
        meta = self.metadata(p)
        transport, view = await asyncio.to_thread(prepare_image, {**args, "file_path":str(p)}, self.root)
        key = (meta["sha256"], args["prompt"], tuple(view["region"]), view["frame"])
        # Direct inspection and document review share both execution and
        # evidence. Concurrent identical pages do not consume extra VLM calls.
        if key not in self.inspection_calls:
            self.inspection_calls[key] = asyncio.create_task(self.callback("inspect_image", {**transport, "image_view": view}))
        try:
            response = await self.inspection_calls[key]
            if not response.get("success"):
                raise ValueError(response.get("error") or "visual inspection failed")
        except BaseException:
            self.inspection_calls.pop(key, None)
            raise
        if view["region"] == [0, 0, view["source_width"], view["source_height"]] and view["frame_count"] == 1:
            self.page_inspections[(meta["sha256"], args["prompt"])] = {
                "image_sha256": meta["sha256"], "focus": args["prompt"], "image_view": view,
                "observation": response.get("output", ""), "details": response.get("data")}
        return response

    async def review_artifacts(self, args):
        p = self.path(args["file_path"])
        rendering = await self.render_file({"file_path":str(p)})
        focus = args.get("focus", "Describe visible layout and content, including any clipping, overlap, unreadable text or missing material. Report observations; do not infer unseen pages.")
        semaphore = asyncio.Semaphore(3)
        async def inspect(page):
            finding = self.page_inspection(page, args.get("focus"))
            if finding is None:
                async with semaphore:
                    await self.inspect_image({"file_path":page["file_path"], "prompt":focus})
                finding = self.page_inspection(page, focus)
            return finding
        findings = await asyncio.gather(*(inspect(page) for page in rendering["pages"]))
        return self.record_inspection(p, rendering, findings)

    def page_inspection(self, page, focus=None):
        for (digest, inspected_focus), record in reversed(list(self.page_inspections.items())):
            if digest == page["sha256"] and (focus is None or focus == inspected_focus):
                return {**record, "page": page["page"]}
        return None

    def record_inspection(self, p, rendering, findings):
        key = rendering["source"]["sha256"]
        if not findings or any(finding is None for finding in findings):
            missing = [page["page"] for page, finding in zip(rendering["pages"], findings) if finding is None]
            raise ValueError(f"Current file has uninspected pages: {missing}. Use review_artifacts or inspect the rendered pages before registering it.")
        record = {"source":rendering["source"], "page_count":rendering["page_count"], "inspected_pages":len(findings),
            "observations":findings, "status":"inspected", "semantic_approval":None,
            "layout_reference":rendering.get("layout_reference", "original image")}
        log = self.root / "inspections" / (key + ".json")
        log.parent.mkdir(exist_ok=True)
        log.write_text(json.dumps(record, ensure_ascii=False), encoding="utf-8")
        self.artifacts.reviewed_fingerprints.add(f"{p.resolve()}::{key}")
        return record

    async def create_artifact(self, args):
        p = self.path(args["file_path"])
        filename = args.get("filename") or p.name
        if Path(filename).suffix.lower()!=p.suffix.lower():
            raise ValueError("output filename must retain the file's actual format; convert the file before changing its extension")
        visual = p.suffix.lower() in {".docx", ".pptx", ".pdf", ".xlsx", ".png", ".jpg", ".jpeg", ".webp"}
        if visual:
            rendering = await self.render_file({"file_path":str(p)})
            self.record_inspection(p, rendering, [self.page_inspection(page) for page in rendering["pages"]])
        result = await asyncio.to_thread(self.artifacts.store_file, filename, p, require_review=visual)
        result["inspection_status"] = "inspected" if visual else "format_and_bytes_only"
        return result

    def tools(self):
        return [
            ("execute_code", "Execute Python code by default, or shell code when language=shell, in the given cwd (default: this run). Returns exit code, actual cwd, bounded output and full log. Cancellation kills the process group.", schema({"language":{"type":"string","enum":["python","shell"],"default":"python"}, "code":STR, "cwd":STR, "timeout_seconds":{"type":"integer","minimum":1,"maximum":300}}, ["code"]), self.execute_code),
            ("list_files", "List files in this run with pagination.", schema({"directory":STR,"offset":{"type":"integer","minimum":0}}), self.list_files),
            ("read_file", "Read UTF-8 text or a skill file in this run. Returns exact text with a continuation offset.", schema({**PATH,"offset":{"type":"integer","minimum":0}},["file_path"]), self.read_file),
            ("write_file", "Create a new UTF-8 file. Returns its path and SHA256. Use edit_file to make later incremental changes; existing files are never overwritten.", schema({**PATH,"content":{"type":"string"}},["file_path","content"]), self.write_file),
            ("edit_file", "Edit a UTF-8 file at its expected SHA256. Replace exactly one old_text occurrence, or append new_text when old_text is empty. Returns the new SHA256 for the next edit.", schema({**PATH,"expected_sha256":{"type":"string","pattern":"^[a-f0-9]{64}$"},"old_text":{"type":"string"},"new_text":{"type":"string"}},["file_path","expected_sha256","old_text","new_text"]), self.edit_file),
            ("convert_file", "Convert an original file with the installed LibreOffice. Returns output identity; leaves original bytes unchanged.", schema({**PATH,"format":{"type":"string","enum":["pdf","docx","xlsx","pptx","txt","csv"]}},["file_path","format"]), self.convert_file),
            ("render_file", "Render all pages and extract text; returns image and text paths bound to source hash. Does not claim visual inspection.", schema(PATH,["file_path"]), self.render_file),
            ("review_artifacts", "Render and inspect all pages with the configured vision model. Returns real page observations and their focus bound to current bytes; no automatic semantic approval. Reuses direct or batch inspections of unchanged pages; an explicit focus requests observations for that focus.", schema({**PATH,"focus":STR},["file_path"]), self.review_artifacts),
            ("create_artifact", "Save a requested output file and return its durable download identity. Computes filename/type/hash/size and binds rendered pages to the current bytes. Visual files require actual full-page inspections from review_artifacts or inspect_image; page crops alone do not cover a complete page. Returns missing pages or storage errors to this call. All registered filenames are delivered; their combined size must not exceed 128 MiB, checked at registration.", schema({**PATH,"filename":STR},["file_path"]), self.create_artifact),
        ]
