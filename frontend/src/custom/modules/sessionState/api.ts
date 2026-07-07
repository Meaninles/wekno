import { post } from "@/utils/request";

export interface SessionStatus {
  session_id: string;
  last_assistant_message_id?: string;
  last_assistant_at?: string;
  is_running: boolean;
  unread: boolean;
}

export type SessionStatusMap = Record<string, SessionStatus>;

export async function listSessionStatuses(sessionIds: string[]): Promise<SessionStatusMap> {
  const ids = Array.from(new Set(sessionIds.map((id) => String(id || "").trim()).filter(Boolean)));
  if (!ids.length) return {};
  const res: any = await post("/api/v1/custom/session-state/status", { session_ids: ids });
  return res?.data || {};
}

export async function markSessionRead(sessionId: string): Promise<SessionStatus | null> {
  const id = String(sessionId || "").trim();
  if (!id) return null;
  const res: any = await post(`/api/v1/custom/session-state/sessions/${encodeURIComponent(id)}/read`, {});
  return res?.data || null;
}
