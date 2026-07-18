from __future__ import annotations

from typing import Any

from pydantic import BaseModel, Field, field_validator


def empty_list_when_none(value: Any) -> Any:
    if value is None:
        return []
    return value


def empty_dict_when_none(value: Any) -> Any:
    if value is None:
        return {}
    return value


class LLMConfig(BaseModel):
    model_name: str
    base_url: str = ""
    api_key: str = ""
    provider: str = ""
    auth_type: str = ""
    api_key_helper: str = ""


class RuntimeToolSpec(BaseModel):
    name: str
    description: str = ""
    parameters: dict[str, Any] = Field(default_factory=dict)
    source: str = "native"


class RuntimeConfigSpec(BaseModel):
    agent_id: str = ""
    agent_type: str = ""
    max_iterations: int = 10
    temperature: float = 0
    thinking: bool | None = None
    allowed_tools: list[str] = Field(default_factory=list)
    knowledge_bases: list[str] = Field(default_factory=list)
    knowledge_ids: list[str] = Field(default_factory=list)
    db_data_sources: list[str] = Field(default_factory=list)
    web_search_enabled: bool = False
    web_search_provider_id: str = ""
    web_search_max_results: int = 0
    claude_sdk_web_search_enabled: bool = False
    web_fetch_enabled: bool = False
    web_fetch_top_n: int = 0
    multi_turn_enabled: bool = False
    history_turns: int = 0
    mcp_selection_mode: str = ""
    mcp_services: list[str] = Field(default_factory=list)
    skills_enabled: bool = False
    allowed_skills: list[str] = Field(default_factory=list)
    professional_skills_enabled: bool = False
    allowed_professional_skills: list[str] = Field(default_factory=list)
    retrieve_kb_only_when_mentioned: bool = False
    retain_retrieval_history: bool = False
    llm_call_timeout: int = 0
    embedding_top_k: int = 0
    keyword_threshold: float = 0
    vector_threshold: float = 0
    rerank_top_k: int = 0
    rerank_threshold: float = 0
    faq_priority_enabled: bool = False
    faq_direct_answer_threshold: float = 0
    faq_score_boost: float = 0
    knowledge_management: dict[str, Any] = Field(default_factory=dict)

    @field_validator("knowledge_management", mode="before")
    @classmethod
    def coerce_optional_dicts(cls, value: Any) -> Any:
        return empty_dict_when_none(value)

    @field_validator(
        "allowed_tools",
        "knowledge_bases",
        "knowledge_ids",
        "db_data_sources",
        "mcp_services",
        "allowed_skills",
        "allowed_professional_skills",
        mode="before",
    )
    @classmethod
    def coerce_optional_lists(cls, value: Any) -> Any:
        return empty_list_when_none(value)


class AttachmentSpec(BaseModel):
    file_name: str = ""
    file_type: str = ""
    file_size: int = 0
    content: str = ""
    is_truncated: bool = False


class OriginalInputFileSpec(BaseModel):
    id: str = ""
    source: str = ""
    role: str = ""
    file_name: str = ""
    file_type: str = ""
    file_size: int = 0
    sha256: str = ""
    download_url: str = ""
    storage_url: str = ""
    knowledge_id: str = ""
    knowledge_base_id: str = ""


class DocumentTemplateFileSpec(BaseModel):
    role: str = ""
    format: str = ""
    variable: str = ""
    source: str = ""
    builtin_id: str = ""
    file_name: str = ""
    file_type: str = ""
    file_size: int = 0
    content_base64: str = ""


class DocumentTemplateContextSpec(BaseModel):
    files: list[DocumentTemplateFileSpec] = Field(default_factory=list)

    @field_validator("files", mode="before")
    @classmethod
    def coerce_optional_lists(cls, value: Any) -> Any:
        return empty_list_when_none(value)


class ImageSpec(BaseModel):
    url: str = ""
    caption: str = ""


class ChatHistoryMessage(BaseModel):
    role: str
    content: str
    mentioned_items: list[dict[str, Any]] = Field(default_factory=list)
    images: list[ImageSpec] = Field(default_factory=list)
    attachments: list[AttachmentSpec] = Field(default_factory=list)


class ProfessionalSkillFileSpec(BaseModel):
    path: str = ""
    content_base64: str = ""


class ProfessionalSkillSpec(BaseModel):
    name: str
    display_name: str = ""
    description: str = ""
    files: list[ProfessionalSkillFileSpec] = Field(default_factory=list)


class ChatPayload(BaseModel):
    run_id: str
    tenant_id: int = 0
    user_id: str = ""
    session_id: str
    request_id: str = ""
    assistant_message_id: str
    query: str
    system_prompt: str = ""
    history: list[ChatHistoryMessage] = Field(default_factory=list)
    image_urls: list[str] = Field(default_factory=list)
    image_description: str = ""
    quoted_context: str = ""
    selected_skill_context: str = ""
    attachments: list[AttachmentSpec] = Field(default_factory=list)
    original_input_files: list[OriginalInputFileSpec] = Field(default_factory=list)
    document_template_context: DocumentTemplateContextSpec = Field(default_factory=DocumentTemplateContextSpec)
    visible_context: dict[str, Any] = Field(default_factory=dict)
    professional_skills: list[ProfessionalSkillSpec] = Field(default_factory=list)
    tools: list[RuntimeToolSpec] = Field(default_factory=list)
    runtime_config: RuntimeConfigSpec = Field(default_factory=RuntimeConfigSpec)
    llm: LLMConfig
    tool_callback_url: str
    tool_callback_api_key: str = ""
    enable_artifacts: bool = False

    @field_validator(
        "history",
        "image_urls",
        "attachments",
        "original_input_files",
        "professional_skills",
        "tools",
        mode="before",
    )
    @classmethod
    def coerce_optional_lists(cls, value: Any) -> Any:
        return empty_list_when_none(value)

    @field_validator("visible_context", mode="before")
    @classmethod
    def coerce_optional_dict(cls, value: Any) -> Any:
        return empty_dict_when_none(value)


class SidecarArtifact(BaseModel):
    file_token: str
    filename: str
    file_type: str
    file_size: int
    sha256: str
    content_type: str = "application/octet-stream"


class ChatResult(BaseModel):
    run_id: str
    answer: str
    artifacts: list[SidecarArtifact] = Field(default_factory=list)
    artifact_notice: str = ""
    artifact_original_count: int = 0
    artifact_returned_count: int = 0
    artifact_dropped_count: int = 0
    artifact_returned_size: int = 0
    artifact_limit_bytes: int = 128 * 1024 * 1024


class RunEvent(BaseModel):
    id: str = ""
    type: str
    content: str = ""
    message: str = ""
    data: Any = None
    done: bool = False
    iteration: int = 0
