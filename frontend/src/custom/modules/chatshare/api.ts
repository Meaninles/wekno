import { get, post } from "@/utils/request";

export type ChatShareLink = {
  id: string;
  session_id: string;
  token: string;
  url: string;
  title: string;
  created_at: string;
};

export type ChatShareMessage = Record<string, any>;

export type ChatShareCandidateMessage = ChatShareMessage & {
  id: string;
  turn_id: string;
  session_id: string;
  request_id?: string;
  role: "user" | "assistant";
  content: string;
  is_completed: boolean;
  selectable: boolean;
  disabled_reason?: string;
};

export type ChatShareCandidates = {
  session_id: string;
  title: string;
  messages: ChatShareCandidateMessage[];
};

export type ChatShareView = {
  id: string;
  session_id: string;
  title: string;
  created_at: string;
  messages: ChatShareMessage[];
};

export async function getChatShareCandidates(sessionId: string) {
  return get<{ success: boolean; data: ChatShareCandidates }>(
    `/api/v1/custom/chat-share/sessions/${encodeURIComponent(sessionId)}/candidates`,
  );
}

export async function createChatShare(sessionId: string, messageIds: string[]) {
  return post<{ success: boolean; data: ChatShareLink }>(
    `/api/v1/custom/chat-share/sessions/${encodeURIComponent(sessionId)}`,
    { message_ids: messageIds },
  );
}

export async function getChatShare(token: string) {
  return get<{ success: boolean; data: ChatShareView }>(
    `/api/v1/custom/chat-share/${encodeURIComponent(token)}`,
  );
}

export function absoluteShareURL(urlOrPath: string, token?: string) {
  const raw = (urlOrPath || "").trim() || (token ? `/share/chat/${token}` : "");
  if (!raw) return "";
  try {
    return new URL(raw, window.location.origin).toString();
  } catch {
    return `${window.location.origin}/share/chat/${encodeURIComponent(token || raw)}`;
  }
}
