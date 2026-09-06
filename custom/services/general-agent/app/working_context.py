"""Model-facing views over exact, run-local records; no semantic summarization."""
import hashlib
import json
from pathlib import Path


TOOL_RESULT_CHAR_LIMIT = 24000
PREVIEW_CHAR_LIMIT = 2000
RECOVERY_MESSAGE = (
    "Runtime status: the previous model response ended without a complete answer or "
    "executable tool call. Its incomplete output was not executed. Previously completed "
    "tools and files remain available. Continue from their results with the next complete "
    "tool call or answer; do not repeat completed operations. File changes can be made "
    "incrementally using the workspace tools."
)


def recovery_reason(finish, text, has_tools=False):
    finish = (finish or "").strip().lower()
    if finish in {"length", "max_tokens", "max_output_tokens"}:
        return "output_limit"
    if finish in {"stop", "end_turn"} and not text.strip() and not has_tools:
        return "empty_response"
    return ""


class ResponseRecovery:
    """One uncommitted response recovery per run, without changing model budgets."""
    def __init__(self):
        self.used = False

    def take(self, finish, text, has_tools=False):
        reason = recovery_reason(finish, text, has_tools)
        if not reason or self.used:
            return ""
        self.used = True
        return reason


def visible_context_view(context, working_directory):
    if not context or not working_directory:
        return context
    root = Path(working_directory).resolve()
    path = root / "runtime-context.json"
    path.write_text(json.dumps(context, ensure_ascii=False, separators=(",", ":")), encoding="utf-8")
    # The complete authorized catalog remains readable. Keep every resource's
    # identity in the index; never select tools/resources using query keywords.
    view = dict(context)
    agent = view.get("agent")
    if isinstance(agent, dict):
        view["agent"] = {k: agent[k] for k in ("id", "name", "agent_type") if k in agent}
    resources = view.get("visible_resources")
    if isinstance(resources, dict):
        view["visible_resources"] = {
            key: [{k: row[k] for k in ("id", "name", "title", "type", "knowledge_base_id") if k in row}
                  if isinstance(row, dict) else row for row in value]
            if isinstance(value, list) else value
            for key, value in resources.items()
        }
    effective = view.get("effective_configuration")
    if isinstance(effective, dict):
        # Execution controls are enforced by the runtime, not model instructions.
        keys = ("knowledge_base_ids", "knowledge_file_ids", "database_data_source_ids",
                "web_search_enabled", "mcp_service_ids", "allowed_professional_skill_names",
                "retrieve_kb_only_when_mentioned", "artifacts_enabled")
        view["effective_configuration"] = {k: effective[k] for k in keys if k in effective}
    view["configuration_details"] = {"read_tool": "read_file", "file_path": path.name,
        "description": "Exact authorized resource metadata and runtime configuration; read details as needed."}
    return view


class ToolResultStore:
    def __init__(self, root):
        self.root = Path(root).resolve()

    def view(self, text, tool_name=""):
        # Paged readers already bound their output. Re-offloading a page would
        # create an endless chain of references rather than reveal the record.
        if len(text) <= TOOL_RESULT_CHAR_LIMIT or tool_name in {"read_file", "read_conversation"}:
            return text
        digest = hashlib.sha256(text.encode("utf-8")).hexdigest()
        path = self.root / "tool-results" / (digest + ".txt")
        path.parent.mkdir(exist_ok=True)
        if not path.exists():
            path.write_text(text, encoding="utf-8")
        return json.dumps({"complete": False, "total_characters": len(text), "sha256": digest,
            "preview": text[:PREVIEW_CHAR_LIMIT], "read_tool": "read_file",
            "file_path": str(path.relative_to(self.root)).replace("\\", "/"), "offset": 0,
            "notice": "Preview only. Read or search the exact saved result for omitted evidence and details."},
            ensure_ascii=False)
