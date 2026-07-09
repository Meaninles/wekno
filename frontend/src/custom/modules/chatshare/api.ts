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

export type ChatShareView = {
  id: string;
  session_id: string;
  title: string;
  created_at: string;
  messages: ChatShareMessage[];
};

export async function createChatShare(sessionId: string) {
  return post<{ success: boolean; data: ChatShareLink }>(
    `/api/v1/custom/chat-share/sessions/${encodeURIComponent(sessionId)}`,
    {},
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
