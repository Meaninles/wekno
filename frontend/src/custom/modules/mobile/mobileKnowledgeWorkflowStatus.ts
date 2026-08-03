import {
  resolveKnowledgeWorkflowStatus,
  type KnowledgePollStatus,
  type KnowledgeWorkflowStatus,
} from "../knowledgeWorkflowStatus/status.ts";

export interface MobileKnowledgeWorkflowPresentation {
  status: KnowledgeWorkflowStatus;
  label: string;
  className: "is-completed" | "is-running" | "is-failed" | "is-warning" | "is-muted";
}

const LABELS: Record<KnowledgeWorkflowStatus, string> = {
  pending: "等待执行",
  processing: "处理中",
  cancelling: "取消中",
  deleting: "删除中",
  completed: "已完成",
  degraded: "已完成（部分增强降级）",
  failed: "执行失败",
  cancelled: "已取消",
  draft: "草稿",
  unknown: "状态未知",
};

const CLASSES: Record<KnowledgeWorkflowStatus, MobileKnowledgeWorkflowPresentation["className"]> = {
  pending: "is-running",
  processing: "is-running",
  cancelling: "is-running",
  deleting: "is-running",
  completed: "is-completed",
  degraded: "is-warning",
  failed: "is-failed",
  cancelled: "is-muted",
  draft: "is-muted",
  unknown: "is-muted",
};

export function resolveMobileKnowledgeWorkflowPresentation(
  item: KnowledgePollStatus,
): MobileKnowledgeWorkflowPresentation {
  const status = resolveKnowledgeWorkflowStatus(item);
  const coreStatus = String(item.core_status || "").trim().toLowerCase();
  return {
    status,
    label: status === "processing" && coreStatus === "ready"
      ? "核心可检索，增强处理中"
      : LABELS[status],
    className: CLASSES[status],
  };
}
