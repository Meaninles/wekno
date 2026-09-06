"""Read-only parse capture; run in the existing eval DocReader container.

Usage: python remaining_docreader_probe.py INPUT.docx OUTPUT.json
No indexing, database writes, model calls or parser changes are performed.
"""
from __future__ import annotations

import hashlib
import json
from pathlib import Path
import sys
import time

sys.path.insert(0, "/app")
from docreader.parser.docx2_parser import Docx2Parser


def main() -> None:
    source, destination = map(Path, sys.argv[1:3])
    data = source.read_bytes()
    start = time.perf_counter()
    document = Docx2Parser(file_type="docx").parse_into_text(data)
    result = {
        "source_sha256": hashlib.sha256(data).hexdigest(),
        "parser": "builtin/Docx2Parser",
        "elapsed_s": time.perf_counter() - start,
        "content": document.content,
        "image_count": len(document.images or {}),
    }
    destination.write_text(json.dumps(result, ensure_ascii=False, indent=2), encoding="utf-8")
    print(json.dumps({k: v for k, v in result.items() if k != "content"}))


if __name__ == "__main__":
    main()
