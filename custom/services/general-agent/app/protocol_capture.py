"""Opt-in local eval capture. Raw events never enter application observations."""
from dataclasses import asdict, is_dataclass
import json
import os
from pathlib import Path
import re
import time

SECRET_FIELDS = {"authorization", "api_key", "api_key_helper", "tool_callback_api_key", "password"}

def _plain(value):
    if is_dataclass(value):
        value = asdict(value)
    elif hasattr(value, "model_dump"):
        value = value.model_dump()
    if isinstance(value, dict):
        return {str(k): "[redacted]" if str(k).lower() in SECRET_FIELDS else _plain(v) for k,v in value.items()}
    if isinstance(value, (list, tuple)):
        return [_plain(v) for v in value]
    if value is None or isinstance(value, (str,int,float,bool)):
        return value
    return str(value)

def capture(run_id, boundary, event):
    directory = os.environ.get("WEKNORA_AGENT_PROTOCOL_CAPTURE_DIR", "")
    if not directory:
        return
    if not re.fullmatch(r"[A-Za-z0-9_-]+", run_id):
        raise ValueError("invalid protocol capture run identity")
    root = Path(directory).resolve()
    root.mkdir(parents=True, exist_ok=True)
    with (root / (run_id + ".jsonl")).open("a",encoding="utf-8") as stream:
        stream.write(json.dumps({"time_ns":time.time_ns(),"boundary":boundary,
            "event_type":type(event).__name__,"event":_plain(event)},ensure_ascii=False)+"\n")
