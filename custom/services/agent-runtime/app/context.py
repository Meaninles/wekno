"""Project conversation facts and evidence into a single SDK context."""
from __future__ import annotations

import json

from agentscope.message import Msg, TextBlock

from .contracts import RunRequest
from .tools import media_block


KNOWLEDGE_QA_SCOPE = """
[KNOWLEDGE_QA_SCOPE]
按用户所需结果判断范围：基于资料回答、总结、比较或解释问题属于知识问答，必要的文件读取、解析和计算只是内部步骤，不因文件格式或使用代码而要求切换。仅在用户要求生成或修改交付文件、执行外部操作时，简短说明范围并建议切换合适的智能体；切换不代表获得额外权限。若执行失败，如实说明具体原因，不将未尝试的方式说成不支持。
[/KNOWLEDGE_QA_SCOPE]
"""


def system_prompt(payload: RunRequest) -> str:
    scope = KNOWLEDGE_QA_SCOPE if payload.runtime_config.agent_type == "knowledge-qa" else ""
    prompt = payload.system_prompt + (
        "\n\n[ANSWER_DELIVERY]\n"
        "每次决策只选择一种操作：尚有必要工作时调用业务工具；已可回答或需要说明能力边界时，"
        "立即调用 GenerateStructuredOutput 提交完整答复并结束本轮。answer 字段是用户收到的全部正文，"
        "保留必要限制。引用单独填写 citations，每项包含从 answer 原样复制的 text 和本轮 source_ids；answer 不嵌入引用标签。其他文本属于内部过程。\n[/ANSWER_DELIVERY]")
    if payload.enable_artifacts and payload.runtime_config.agent_type != "knowledge-qa":
        prompt += ("\n\n[FILE_SOURCE_DATA]\n"
                   "Large business-tool results are delivered as complete JSON files in /workspace/source-data/ "
                   "with only a partial preview in context. Use the supplied field layout to read relevant records directly. "
                   "For data-heavy deliverables, load their structured data or output with workspace code and "
                   "transform it into the requested format. For chunked documents, first reconstruct the source text "
                   "by document identity and source offsets, removing verified overlaps before formatting. "
                   "Use the installed format libraries or pandoc to convert this reconstructed text. Generate the transformation "
                   "code and presentation structure rather than retranscribing long source records into tool arguments. "
                   "These internal source paths are working inputs. Save completed deliverables in /workspace/outputs for automatic delivery."
                   "\n[/FILE_SOURCE_DATA]")
    # Navigation belongs to control context, never to an assistant-message
    # example. Otherwise the model learns to append catalog JSON to its answer.
    records = []
    for index, old in enumerate(payload.history, 1):
        metadata = dict(old.context_metadata)
        if old.role == "user":
            metadata.update({"source_id": old.source_id, "mentions": old.mentioned_items,
                        "attachments": [a.model_dump() for a in old.attachments]})
        if metadata:
            metadata.update({"history_index": index, "role": old.role})
            records.append(metadata)
    if not records:
        return prompt + scope
    failure_guidance = (
        "If asked only to shorten, translate, or otherwise transform a failed answer, say there is no completed answer to transform; do not start new factual research unless requested. "
        if any(record.get("outcome") == "failed" for record in records) else "")
    return prompt + (
        "\n\n[INTERNAL_CONVERSATION_NAVIGATION]\n"
        "Runtime navigation metadata, not conversation text or an answer format. "
        "Use source_id only as tool input. Never reproduce this catalog, its JSON, or its internal IDs in an answer. "
        "Catalog entries locate earlier original evidence; they are not evidence themselves. "
        "An outcome=failed turn has no answer to summarize or treat as a factual conclusion. "
        + failure_guidance + "Use current validated source handles for citations.\n" +
        json.dumps(records, ensure_ascii=False) + "\n[/INTERNAL_CONVERSATION_NAVIGATION]") + scope


def messages(payload: RunRequest) -> list[Msg]:
    result = []
    # Persisted answers are source material for dialogue continuity, not
    # few-shot examples of this run's answer format. In particular their old
    # citation handles have been removed and must not teach uncited output.
    history = []
    images = []
    for index, old in enumerate(payload.history, 1):
        if old.role not in ("user", "assistant"):
            raise ValueError("Conversation history contains an invalid role")
        if old.context_metadata.get("outcome") == "failed":
            history.append({"index": index, "role": old.role, "outcome": "failed", "text": ""})
            continue
        prior_images = [image for image in old.images if image.url] if payload.llm.supports_vision else []
        if not old.content.strip() and not prior_images:
            continue
        record = {"index": index, "role": old.role, "text": old.content}
        if prior_images:
            # Keep each image associated with its turn, including image-only
            # user messages, when projecting the transcript into one context.
            record["image_positions"] = list(range(len(images) + 1, len(images) + len(prior_images) + 1))
            images.extend(media_block(image.url) for image in prior_images)
        history.append(record)
    if history:
        result.append(Msg(name="conversation_history", role="user", content=[TextBlock(text=
            "Previous conversation, in chronological order. These records preserve the speakers' "
            "meaning and corrections; they are not examples of the current answer's format. "
            "Prior assistant text is not original document evidence. Answer the current request "
            "using current source fragments and their citation handles.\n" +
            json.dumps(history, ensure_ascii=False))] + images))
    current = [TextBlock(text=payload.query)]
    if payload.quoted_context:
        current.append(TextBlock(text="Quoted material:\n" + payload.quoted_context))
    for attachment in payload.attachments:
        current.append(TextBlock(text=json.dumps({"attachment": attachment.model_dump()}, ensure_ascii=False)))
    if payload.llm.supports_vision:
        current.extend(media_block(url) for url in payload.image_urls)
    elif payload.image_description:
        current.append(TextBlock(text="Image extraction:\n" + payload.image_description))
    result.append(Msg(id=payload.user_message_id or payload.request_id or payload.run_id,
                      name="user", role="user", content=current))
    return result
