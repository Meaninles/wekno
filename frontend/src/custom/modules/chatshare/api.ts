import { get, post } from "@/utils/request";
import { getApiBaseUrl } from "@/utils/api-base";

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

export type ArtifactShareLink = {
  id: string;
  artifact_id: string;
  filename: string;
  url: string;
  preview_url: string;
  password_configured: boolean;
  created_at: string;
};

export type ArtifactShareView = {
  id: string;
  artifact_id: string;
  filename: string;
  file_type: string;
  file_size: number;
  content_type: string;
  content_url: string;
  requires_password: boolean;
  created_at: string;
};

export type ArtifactShareAccess = {
  access_token: string;
  expires_at: string;
};

// Keep the creator's plaintext only in this page session so a repeated Share
// click can copy the same bundle without ever persisting it server-side or in
// browser storage. A reload simply asks the creator to enter it again.
const artifactSharePasswordCache = new Map<string, string>();

export function getCachedArtifactSharePassword(artifactId: string) {
  return artifactSharePasswordCache.get(artifactId) || "";
}

export function cacheArtifactSharePassword(artifactId: string, password: string) {
  if (artifactId && password) artifactSharePasswordCache.set(artifactId, password);
}

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

export async function createArtifactShare(artifactId: string) {
  return post<{ success: boolean; data: ArtifactShareLink }>(
    `/api/v1/custom/artifact-share/artifacts/${encodeURIComponent(artifactId)}`,
  );
}

export async function setArtifactSharePassword(artifactId: string, password: string) {
  return post<{ success: boolean; data: ArtifactShareLink }>(
    `/api/v1/custom/artifact-share/artifacts/${encodeURIComponent(artifactId)}/password`,
    { password },
  );
}

export async function getArtifactShare(
  token: string,
  credentials?: { previewToken?: string; accessToken?: string },
) {
  const query = new URLSearchParams();
  if (credentials?.previewToken) query.set("preview", credentials.previewToken);
  if (credentials?.accessToken) query.set("access", credentials.accessToken);
  const suffix = query.toString() ? `?${query.toString()}` : "";
  return get<{ success: boolean; data: ArtifactShareView }>(
    `/api/v1/custom/artifact-share/${encodeURIComponent(token)}${suffix}`,
  );
}

export async function verifyArtifactSharePassword(token: string, password: string) {
  return post<{ success: boolean; data: ArtifactShareAccess }>(
    `/api/v1/custom/artifact-share/${encodeURIComponent(token)}/access`,
    { password },
  );
}

export function artifactShareContentURL(
  token: string,
  contentURL?: string,
  credentials?: { previewToken?: string; accessToken?: string },
) {
  const apiBase = getApiBaseUrl();
  const contentPath = `/api/v1/custom/artifact-share/${encodeURIComponent(token)}/content`;
  const supplied = (contentURL || "").trim();
  const raw = supplied || `${apiBase}${contentPath}`;
  try {
    // The API returns a root-relative path. When the SPA is mounted below a
    // reverse-proxy prefix, route it through that same prefix so the iframe
    // reaches the backend instead of the proxy root.
    const resolved = supplied && supplied.startsWith("/") && apiBase && !supplied.startsWith(`${apiBase}/`)
      ? `${apiBase}${supplied}`
      : raw;
    const absolute = new URL(resolved, window.location.origin);
    if (credentials?.previewToken) absolute.searchParams.set("preview", credentials.previewToken);
    if (credentials?.accessToken) absolute.searchParams.set("access", credentials.accessToken);
    return absolute.toString();
  } catch {
    const fallback = new URL(`${window.location.origin}${apiBase}${contentPath}`);
    if (credentials?.previewToken) fallback.searchParams.set("preview", credentials.previewToken);
    if (credentials?.accessToken) fallback.searchParams.set("access", credentials.accessToken);
    return fallback.toString();
  }
}

export function absoluteArtifactShareURL(urlOrPath: string, token?: string) {
  const raw = (urlOrPath || "").trim() || (token ? `/share/artifact/${encodeURIComponent(token)}` : "");
  if (!raw) return "";
  try {
    return new URL(raw, window.location.origin).toString();
  } catch {
    return `${window.location.origin}/share/artifact/${encodeURIComponent(token || raw)}`;
  }
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
