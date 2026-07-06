import type { CustomAgent } from "@/api/agent";
import type { ModelConfig } from "@/api/model";
import type { MCPService } from "@/api/mcp-service";
import type { SkillInfo } from "@/api/skill";

export type MobileResourceType =
  | "agent"
  | "model"
  | "kb"
  | "file"
  | "tag"
  | "skill"
  | "professional"
  | "mcp"
  | "web"
  | "image"
  | "attachment";

export interface MobileResourceChip {
  id: string;
  type: MobileResourceType;
  name: string;
  meta?: string;
  removable?: boolean;
}

export interface MobileMentionItem {
  id: string;
  name: string;
  type: string;
  kb_type?: string;
  kb_id?: string;
  kb_name?: string;
  service_id?: string;
  skill_name?: string;
}

export interface MobileUploadAttachment {
  file: File;
  name: string;
  size: number;
}

export function formatFileSize(bytes?: number) {
  if (!bytes) return "";
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
}

export function fileToDataUrl(file: File): Promise<string> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onload = () => resolve(String(reader.result || ""));
    reader.onerror = reject;
    reader.readAsDataURL(file);
  });
}

export async function fileToBase64(file: File): Promise<string> {
  const dataUrl = await fileToDataUrl(file);
  return dataUrl.includes(",") ? dataUrl.split(",")[1] : dataUrl;
}

export function agentLabel(agent?: CustomAgent | null) {
  return agent?.name || "快速问答";
}

export function modelLabel(model?: ModelConfig | null) {
  return model?.display_name || model?.name || "默认模型";
}

export function skillLabel(skill?: SkillInfo | null) {
  return skill?.display_name || skill?.name || "技能";
}

export function mcpLabel(service?: MCPService | null) {
  return service?.name || "MCP";
}

export function isParseInFlight(status?: string) {
  return ["pending", "processing", "finalizing"].includes(String(status || ""));
}

export function parseStatusText(status?: string, summaryStatus?: string) {
  if (status === "pending") return "待解析";
  if (status === "processing") return "解析中";
  if (status === "finalizing") return "整理中";
  if (status === "failed") return "解析失败";
  if (status === "cancelled") return "已取消";
  if (status === "draft") return "草稿";
  if (status === "completed" && ["pending", "processing"].includes(String(summaryStatus || ""))) {
    return "生成摘要中";
  }
  if (status === "completed") return "已完成";
  return status || "未知";
}

export function parseStatusClass(status?: string) {
  if (status === "completed") return "is-completed";
  if (status === "failed") return "is-failed";
  if (isParseInFlight(status)) return "is-running";
  return "is-muted";
}

export function downloadBlob(blob: Blob, fileName: string) {
  const url = URL.createObjectURL(blob);
  const anchor = document.createElement("a");
  anchor.href = url;
  anchor.download = fileName || "download";
  document.body.appendChild(anchor);
  anchor.click();
  anchor.remove();
  window.setTimeout(() => URL.revokeObjectURL(url), 1000);
}
