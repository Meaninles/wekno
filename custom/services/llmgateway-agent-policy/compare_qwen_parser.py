"""Replay captured reasoning chunks through unmodified official parser classes.

Usage: python compare_qwen_parser.py OLD_SOURCE NEW_SOURCE CAPTURE_JSONL
Sources are pinned upstream reasoning_parser.py snapshots, stored outside Git.
Only their standalone Qwen detector classes are loaded; no inference or tools run.
"""
import argparse
import ast
import hashlib
import json
from pathlib import Path
import re
from typing import Dict, List, Optional, Tuple, Type


def replay(source_path, chunks):
    source = source_path.read_text(encoding="utf-8")
    nodes = [node for node in ast.parse(source).body if isinstance(node, ast.ClassDef)
             and node.name in {"StreamingParseResult", "BaseReasoningFormatDetector", "Qwen3Detector"}]
    assert len(nodes) == 3, "Expected official standalone Qwen parser classes"
    scope = dict(Dict=Dict, List=List, Optional=Optional, Tuple=Tuple, Type=Type, re=re)
    exec(compile(ast.Module(body=nodes, type_ignores=[]), str(source_path), "exec"), scope)
    detector = scope["Qwen3Detector"](force_reasoning=True)
    reasoning, normal = [], []
    for chunk in chunks:
        parsed = detector.parse_streaming_increment(chunk)
        reasoning.append(parsed.reasoning_text)
        normal.append(parsed.normal_text)
    if hasattr(detector, "finish"):
        parsed = detector.finish()
        reasoning.append(parsed.reasoning_text)
        normal.append(parsed.normal_text)
    return {"source_sha256": hashlib.sha256(source.encode()).hexdigest(),
            "reasoning_chars": len("".join(reasoning)), "normal_chars": len("".join(normal)),
            "normal_sha256": hashlib.sha256("".join(normal).encode()).hexdigest()}


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    for name in ("old", "new", "capture"):
        parser.add_argument(name, type=Path)
    args = parser.parse_args()
    rows = [json.loads(line) for line in args.capture.read_text(encoding="utf-8").splitlines()]
    start = max(i for i, row in enumerate(rows) if row["boundary"] == "platform_model_request")
    chunks = [row["event"].get("content", "") for row in rows[start + 1:]
              if row["event"].get("response_type") == "thinking"]
    print(json.dumps({"old": replay(args.old, chunks), "new": replay(args.new, chunks)}, indent=2))
