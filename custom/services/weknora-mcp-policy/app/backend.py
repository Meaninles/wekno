"""Small authenticated HTTP client for the WeKnora API."""

from __future__ import annotations

import base64
import binascii
import io
import json
import logging
import os
import re
from pathlib import Path
from typing import Any, Iterable
from urllib.parse import quote

import requests
from requests import Response
from requests.exceptions import RequestException


logger = logging.getLogger(__name__)


class BackendError(RuntimeError):
    """An upstream error safe to return to an MCP caller."""

    def __init__(self, message: str, status_code: int = 0):
        super().__init__(message)
        self.status_code = status_code


def _safe_error_message(response: Response) -> str:
    """Extract a short, non-sensitive upstream error description."""

    try:
        payload = response.json()
    except ValueError:
        return f"WeKnora API returned HTTP {response.status_code}"

    message: Any = None
    if isinstance(payload, dict):
        message = payload.get("message") or payload.get("error") or payload.get("code")
    if isinstance(message, dict):
        message = message.get("message") or message.get("code")
    if not isinstance(message, str) or not message.strip():
        return f"WeKnora API returned HTTP {response.status_code}"
    # Do not reflect a large body or credentials in a tool error.
    compact = " ".join(message.split())
    compact = re.sub(r"(?i)bearer\s+\S+", "Bearer [REDACTED]", compact)
    return compact[:512]


def _encoded(value: str) -> str:
    return quote(str(value), safe="")


def _path_with_slug(kb_id: str, slug: str) -> str:
    # Wiki slugs can contain hierarchical slash separators.
    return f"/knowledgebase/{_encoded(kb_id)}/wiki/pages/{quote(str(slug), safe='/')}"


def _extract_list(payload: Any) -> list[dict[str, Any]]:
    if isinstance(payload, list):
        return [item for item in payload if isinstance(item, dict)]
    if isinstance(payload, dict):
        for key in ("data", "list", "items", "results"):
            value = payload.get(key)
            if isinstance(value, list):
                return [item for item in value if isinstance(item, dict)]
            if isinstance(value, dict):
                nested = _extract_list(value)
                if nested:
                    return nested
    return []


class BackendClient:
    """Authenticated facade over the public /api/v1 routes."""

    _UUID_RE = re.compile(
        r"^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$",
        re.IGNORECASE,
    )

    def __init__(self, base_url: str, api_key: str):
        self.base_url = base_url.rstrip("/")
        self.api_key = api_key
        self.connect_timeout = float(os.getenv("WEKNORA_MCP_CONNECT_TIMEOUT", "10"))
        self.read_timeout = float(os.getenv("WEKNORA_MCP_READ_TIMEOUT", "300"))
        self.max_upload_bytes = int(os.getenv("MCP_MAX_UPLOAD_BYTES", str(32 * 1024 * 1024)))
        self.max_download_bytes = int(os.getenv("MCP_MAX_DOWNLOAD_BYTES", str(8 * 1024 * 1024)))
        self.allow_server_file_path = os.getenv("MCP_ALLOW_SERVER_FILE_PATH", "false").lower() == "true"
        self.session = requests.Session()
        # The tenant key is deliberately request-derived.  Never log this
        # session or its headers.
        self.session.headers.update({"X-API-Key": api_key, "Accept": "application/json"})

    def _request(self, method: str, endpoint: str, **kwargs) -> Any:
        url = f"{self.base_url}{endpoint}"
        kwargs.setdefault("timeout", (self.connect_timeout, self.read_timeout))
        try:
            response = self.session.request(method, url, **kwargs)
        except RequestException as exc:
            logger.warning("WeKnora API request failed: method=%s endpoint=%s type=%s", method, endpoint, type(exc).__name__)
            raise BackendError("WeKnora API is unavailable") from exc

        if not response.ok:
            raise BackendError(_safe_error_message(response), response.status_code)
        if response.status_code == 204 or not response.content:
            return {"success": True}
        try:
            return response.json()
        except ValueError as exc:
            raise BackendError("WeKnora API returned a non-JSON response", response.status_code) from exc

    def _get(self, endpoint: str, params: dict[str, Any] | None = None) -> Any:
        return self._request("GET", endpoint, params=params or {})

    def _post(self, endpoint: str, body: dict[str, Any] | None = None) -> Any:
        return self._request("POST", endpoint, json=body or {})

    def _put(self, endpoint: str, body: dict[str, Any]) -> Any:
        return self._request("PUT", endpoint, json=body)

    def _delete(self, endpoint: str) -> Any:
        return self._request("DELETE", endpoint)

    def resolve_kb_id(self, value: str) -> str:
        if self._UUID_RE.match(value):
            return value
        records = _extract_list(self.list_knowledge_bases())
        wanted = value.casefold()
        for record in records:
            if str(record.get("name", "")).casefold() == wanted:
                return str(record["id"])
        raise BackendError(f"knowledge base not found: {value}", 404)

    def resolve_agent_id(self, value: str) -> str:
        if self._UUID_RE.match(value):
            return value
        records = _extract_list(self.list_agents())
        wanted = value.casefold()
        for record in records:
            if str(record.get("name", "")).casefold() == wanted:
                return str(record["id"])
        raise BackendError(f"agent not found: {value}", 404)

    # Tenant and knowledge-base management.
    def create_tenant(self, args: dict[str, Any]) -> Any:
        body = {
            "name": args["name"],
            "description": args.get("description", ""),
            "business": args.get("business", ""),
            "retriever_engines": args.get(
                "retriever_engines",
                {
                    "engines": [
                        {"retriever_type": "keywords", "retriever_engine_type": "postgres"},
                        {"retriever_type": "vector", "retriever_engine_type": "postgres"},
                    ]
                },
            ),
        }
        return self._post("/tenants", body)

    def list_tenants(self) -> Any:
        return self._get("/tenants")

    def create_knowledge_base(self, args: dict[str, Any]) -> Any:
        body = dict(args.get("config") or {})
        # Keep the common legacy MCP arguments usable while allowing the
        # modern nested config object for the full API surface.
        for key in (
            "type",
            "embedding_model_id",
            "summary_model_id",
            "derivative_model_id",
            "chunking_config",
            "image_processing_config",
            "vlm_config",
            "asr_config",
            "extract_config",
            "faq_config",
            "wiki_config",
            "indexing_strategy",
        ):
            if args.get(key) is not None:
                body[key] = args[key]
        body.update({"name": args["name"], "description": args.get("description", "")})
        return self._post("/knowledge-bases", body)

    def list_knowledge_bases(self) -> Any:
        return self._get("/knowledge-bases")

    def get_knowledge_base(self, kb_id: str) -> Any:
        return self._get(f"/knowledge-bases/{_encoded(self.resolve_kb_id(kb_id))}")

    def update_knowledge_base(self, args: dict[str, Any]) -> Any:
        kb_id = self.resolve_kb_id(args["kb_id"])
        body: dict[str, Any] = {
            "name": args["name"],
            "description": args.get("description", ""),
        }
        if args.get("config") is not None:
            body["config"] = args["config"]
        return self._put(f"/knowledge-bases/{_encoded(kb_id)}", body)

    def delete_knowledge_base(self, kb_id: str) -> Any:
        return self._delete(f"/knowledge-bases/{_encoded(self.resolve_kb_id(kb_id))}")

    def hybrid_search(self, args: dict[str, Any]) -> Any:
        kb_id = self.resolve_kb_id(args["kb_id"])
        body: dict[str, Any] = {"query_text": args["query"]}
        for key in ("vector_threshold", "keyword_threshold", "match_count"):
            if key in args and args[key] is not None:
                body[key] = args[key]
        return self._post(f"/knowledge-bases/{_encoded(kb_id)}/hybrid-search", body)

    # Knowledge ingestion and lifecycle.
    def create_knowledge_from_content(self, args: dict[str, Any]) -> Any:
        kb_id = self.resolve_kb_id(args["kb_id"])
        body: dict[str, Any] = {
            "title": args["title"],
            "content": args["content"],
            "status": args.get("status", "publish"),
            "tag_ids": args.get("tag_ids") or [],
            "channel": args.get("channel", "api"),
        }
        if args.get("process_config") is not None:
            body["process_config"] = args["process_config"]
        return self._post(f"/knowledge-bases/{_encoded(kb_id)}/knowledge/manual", body)

    @staticmethod
    def _decode_base64_content(raw: str) -> bytes:
        value = raw.strip()
        if ";base64," in value and value.lower().startswith("data:"):
            value = value.split(",", 1)[1]
        try:
            return base64.b64decode(value, validate=True)
        except (ValueError, binascii.Error) as exc:
            raise BackendError("content_base64 is not valid base64", 400) from exc

    def _file_bytes(self, args: dict[str, Any]) -> bytes:
        content = args.get("content_base64")
        if isinstance(content, str) and content.strip():
            payload = self._decode_base64_content(content)
        else:
            file_path = args.get("file_path")
            if not isinstance(file_path, str) or not file_path.strip():
                raise BackendError("provide content_base64; server file_path is disabled", 400)
            if not self.allow_server_file_path:
                raise BackendError("server file_path is disabled for this service", 403)
            try:
                payload = Path(file_path).read_bytes()
            except OSError as exc:
                raise BackendError("server file_path cannot be read", 400) from exc
        if len(payload) > self.max_upload_bytes:
            raise BackendError(f"uploaded file exceeds {self.max_upload_bytes} bytes", 413)
        return payload

    def create_knowledge_from_file(self, args: dict[str, Any]) -> Any:
        kb_id = self.resolve_kb_id(args["kb_id"])
        filename = str(args["filename"]).strip()
        if not filename:
            raise BackendError("filename is required", 400)
        payload = self._file_bytes(args)
        fields: dict[str, str] = {
            "enable_multimodel": str(bool(args.get("enable_multimodel", True))).lower(),
            "channel": str(args.get("channel", "api")),
        }
        if args.get("metadata") is not None:
            fields["metadata"] = json.dumps(args["metadata"], ensure_ascii=False)
        if args.get("tag_ids"):
            fields["tag_ids"] = ",".join(str(item) for item in args["tag_ids"])
        if args.get("process_config") is not None:
            fields["process_config"] = json.dumps(args["process_config"], ensure_ascii=False)

        content_type = str(args.get("content_type") or "application/octet-stream")
        files = {"file": (filename, io.BytesIO(payload), content_type)}
        return self._request(
            "POST",
            f"/knowledge-bases/{_encoded(kb_id)}/knowledge/file",
            files=files,
            data=fields,
        )

    def create_knowledge_from_url(self, args: dict[str, Any]) -> Any:
        kb_id = self.resolve_kb_id(args["kb_id"])
        body: dict[str, Any] = {"url": args["url"]}
        mapping = {
            "file_name": "file_name",
            "file_type": "file_type",
            "enable_multimodel": "enable_multimodel",
            "title": "title",
            "tag_ids": "tag_ids",
            "channel": "channel",
            "process_config": "process_config",
        }
        for source, target in mapping.items():
            if args.get(source) is not None:
                body[target] = args[source]
        return self._post(f"/knowledge-bases/{_encoded(kb_id)}/knowledge/url", body)

    def list_knowledge(self, args: dict[str, Any]) -> Any:
        kb_id = self.resolve_kb_id(args["kb_id"])
        params: dict[str, Any] = {
            "page": args.get("page", 1),
            "page_size": args.get("page_size", 20),
        }
        for key in ("keyword", "file_type", "parse_status", "workflow_status", "source"):
            if args.get(key) not in (None, ""):
                params[key] = args[key]
        return self._get(f"/knowledge-bases/{_encoded(kb_id)}/knowledge", params)

    def get_knowledge(self, knowledge_id: str) -> Any:
        return self._get(f"/knowledge/{_encoded(knowledge_id)}")

    def ingest_status(self, knowledge_id: str) -> Any:
        payload = self.get_knowledge(knowledge_id)
        data = payload.get("data", payload) if isinstance(payload, dict) else payload
        if not isinstance(data, dict):
            return data
        fields = (
            "id", "knowledge_base_id", "title", "file_name", "type", "source",
            "parse_status", "core_status", "summary_status", "enrichment_status",
            "wiki_status", "error_message", "updated_at", "processed_at",
        )
        return {key: data[key] for key in fields if key in data}

    def download_knowledge(self, knowledge_id: str) -> dict[str, Any]:
        endpoint = f"/knowledge/{_encoded(knowledge_id)}/download"
        try:
            response = self.session.get(
                f"{self.base_url}{endpoint}",
                timeout=(self.connect_timeout, self.read_timeout),
                stream=True,
            )
        except RequestException as exc:
            logger.warning("WeKnora download failed: endpoint=%s type=%s", endpoint, type(exc).__name__)
            raise BackendError("WeKnora API is unavailable") from exc
        if not response.ok:
            raise BackendError(_safe_error_message(response), response.status_code)

        chunks: list[bytes] = []
        total = 0
        for chunk in response.iter_content(chunk_size=64 * 1024):
            if not chunk:
                continue
            total += len(chunk)
            if total > self.max_download_bytes:
                raise BackendError(f"download exceeds {self.max_download_bytes} bytes", 413)
            chunks.append(chunk)
        filename = "download.bin"
        disposition = response.headers.get("Content-Disposition", "")
        match = re.search(r"filename\*?=(?:UTF-8''|\")?([^;\"]+)", disposition, re.IGNORECASE)
        if match:
            filename = match.group(1).strip().strip('"')
        return {
            "knowledge_id": knowledge_id,
            "filename": filename,
            "content_type": response.headers.get("Content-Type", "application/octet-stream"),
            "size_bytes": total,
            "content_base64": base64.b64encode(b"".join(chunks)).decode("ascii"),
        }

    def delete_knowledge(self, knowledge_id: str) -> Any:
        return self._delete(f"/knowledge/{_encoded(knowledge_id)}")

    # Models, sessions, agents and chunks.
    def create_model(self, args: dict[str, Any]) -> Any:
        model_type = args.get("model_type", args.get("type"))
        if not model_type:
            raise BackendError("model_type is required", 400)
        parameters = dict(args.get("parameters") or {})
        for key in ("base_url", "api_key"):
            if args.get(key) is not None and key not in parameters:
                parameters[key] = args[key]
        body: dict[str, Any] = {
            "name": args["name"],
            "display_name": args.get("display_name", ""),
            "type": model_type,
            "source": args.get("source", "local"),
            "description": args.get("description", ""),
            "parameters": parameters,
        }
        if args.get("workload_scope") is not None:
            body["workload_scope"] = args["workload_scope"]
        # Older clients send is_default.  The current API ignores this
        # compatibility field, but retaining it is harmless for deployments
        # that still implement the older model-selection behavior.
        if args.get("is_default") is not None:
            body["is_default"] = args["is_default"]
        return self._post("/models", body)

    def list_models(self) -> Any:
        return self._get("/models")

    def get_model(self, model_id: str) -> Any:
        return self._get(f"/models/{_encoded(model_id)}")

    def create_session(self, args: dict[str, Any]) -> Any:
        body = {
            "title": args.get("title", ""),
            "description": args.get("description", ""),
        }
        if args.get("last_request_state") is not None:
            body["last_request_state"] = args["last_request_state"]
        return self._post("/sessions", body)

    def get_session(self, session_id: str) -> Any:
        return self._get(f"/sessions/{_encoded(session_id)}")

    def list_sessions(self, args: dict[str, Any]) -> Any:
        return self._get(
            "/sessions",
            {"page": args.get("page", 1), "page_size": args.get("page_size", 20)},
        )

    def delete_session(self, session_id: str) -> Any:
        return self._delete(f"/sessions/{_encoded(session_id)}")

    def _chat(self, endpoint: str, session_id: str, body: dict[str, Any]) -> Any:
        try:
            response = self.session.post(
                f"{self.base_url}/{endpoint}/{_encoded(session_id)}",
                json=body,
                stream=True,
                timeout=(self.connect_timeout, self.read_timeout),
            )
        except RequestException as exc:
            logger.warning("WeKnora chat request failed: endpoint=%s type=%s", endpoint, type(exc).__name__)
            raise BackendError("WeKnora API is unavailable") from exc
        if not response.ok:
            raise BackendError(_safe_error_message(response), response.status_code)

        answers: list[str] = []
        references: list[Any] = []
        event_types: list[str] = []
        try:
            for raw_line in response.iter_lines():
                if not raw_line:
                    continue
                line = raw_line.decode("utf-8", errors="replace") if isinstance(raw_line, bytes) else raw_line
                if not line.startswith("data:"):
                    continue
                try:
                    event = json.loads(line[5:].lstrip())
                except json.JSONDecodeError:
                    continue
                response_type = str(event.get("response_type", ""))
                if response_type:
                    event_types.append(response_type)
                if response_type == "answer" and event.get("content"):
                    answers.append(str(event["content"]))
                elif response_type == "references":
                    references = event.get("knowledge_references") or []
                elif response_type == "error":
                    raise BackendError("WeKnora chat failed")
                elif response_type == "complete":
                    break
        finally:
            response.close()
        return {"answer": "".join(answers), "references": references, "event_types": event_types}

    def chat(self, args: dict[str, Any]) -> Any:
        body: dict[str, Any] = {"query": args["query"], "channel": "api"}
        if args.get("knowledge_base_ids"):
            body["knowledge_base_ids"] = [self.resolve_kb_id(value) for value in args["knowledge_base_ids"]]
        if args.get("web_search_enabled"):
            body["web_search_enabled"] = True
        if args.get("enable_memory"):
            body["enable_memory"] = True
        result = self._chat("knowledge-chat", args["session_id"], body)
        result["session_id"] = args["session_id"]
        return result

    def agent_chat(self, args: dict[str, Any]) -> Any:
        body: dict[str, Any] = {
            "query": args["query"],
            "agent_id": self.resolve_agent_id(args["agent_id"]),
            "channel": "api",
        }
        if args.get("knowledge_base_ids"):
            body["knowledge_base_ids"] = [self.resolve_kb_id(value) for value in args["knowledge_base_ids"]]
        if args.get("web_search_enabled"):
            body["web_search_enabled"] = True
        if args.get("enable_memory"):
            body["enable_memory"] = True
        result = self._chat("agent-chat", args["session_id"], body)
        result["session_id"] = args["session_id"]
        result["agent_id"] = body["agent_id"]
        return result

    def list_agents(self, args: dict[str, Any] | None = None) -> Any:
        args = args or {}
        return self._get("/agents", {"page": args.get("page", 1), "page_size": args.get("page_size", 50)})

    def get_agent(self, agent_id: str) -> Any:
        return self._get(f"/agents/{_encoded(self.resolve_agent_id(agent_id))}")

    def list_chunks(self, args: dict[str, Any]) -> Any:
        return self._get(
            f"/chunks/{_encoded(args['knowledge_id'])}",
            {"page": args.get("page", 1), "page_size": args.get("page_size", 20)},
        )

    def delete_chunk(self, args: dict[str, Any]) -> Any:
        return self._delete(
            f"/chunks/{_encoded(args['knowledge_id'])}/{_encoded(args['chunk_id'])}"
        )

    # Wiki read-only routes.
    def wiki_search(self, args: dict[str, Any]) -> Any:
        kb_id = self.resolve_kb_id(args["kb_id"])
        return self._get(
            f"/knowledgebase/{_encoded(kb_id)}/wiki/search",
            {"q": args["query"], "limit": args.get("limit", 10)},
        )

    def wiki_read_page(self, args: dict[str, Any]) -> Any:
        kb_id = self.resolve_kb_id(args["kb_id"])
        return self._get(_path_with_slug(kb_id, args["slug"]))

    def wiki_index_view(self, args: dict[str, Any]) -> Any:
        kb_id = self.resolve_kb_id(args["kb_id"])
        return self._get(
            f"/knowledgebase/{_encoded(kb_id)}/wiki/index",
            {"limit": args.get("limit", 50)},
        )
