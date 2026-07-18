import type { ChatShareMessage } from "./api";
import {
  normalizeMessageArtifacts,
  normalizeMessageAttachments,
  normalizeMessageImages,
  normalizeMessageToolResults,
} from "./media.ts";

export function normalizeChatShareMessage(message: ChatShareMessage): ChatShareMessage {
  const artifacts = normalizeMessageArtifacts(message.artifacts);
  const toolResults = normalizeMessageToolResults(message.tool_results);
  const hasStructuredCharts = toolResults.some((item) =>
    item.display_type === "structured_analysis_result" &&
    item.tool_data?.chart_requested === true &&
    item.tool_data?.chart?.eligible === true,
  );
  const content = typeof message.content === "string" ? message.content : "";
  return {
    ...message,
    content,
    knowledge_references: [],
    agent_steps: [],
    tool_results: toolResults,
    agentEventStream: hasStructuredCharts
      ? [
        {
          type: "answer",
          event_id: `${message.id || message.original_message_id || "share"}-answer`,
          content,
          done: true,
          final_answer: true,
        },
        {
          type: "agent_complete",
          event_id: `${message.id || message.original_message_id || "share"}-complete`,
        },
      ]
      : [],
    artifacts,
    isAgentMode: hasStructuredCharts,
    isRagMode: false,
    showThink: false,
    hideContent: false,
    is_completed: message.is_completed !== false,
    mentioned_items: Array.isArray(message.mentioned_items) ? message.mentioned_items : [],
    images: normalizeMessageImages(message.images),
    attachments: normalizeMessageAttachments(message.attachments),
  };
}

export function artifactDataFor(message: ChatShareMessage) {
  const artifacts = normalizeMessageArtifacts(message.artifacts);
  return {
    display_type: "general_agent_artifacts" as const,
    artifacts,
    artifact_original_count: artifacts.length,
  };
}

export function userQueryFor(messages: ChatShareMessage[], index: number): string {
  const message = messages[index];
  const requestID = String(message?.request_id || "").trim();
  if (!requestID) return "";
  const paired = messages.find((item) =>
    item?.role === "user" && String(item?.request_id || "").trim() === requestID,
  );
  return String(paired?.content || "");
}
