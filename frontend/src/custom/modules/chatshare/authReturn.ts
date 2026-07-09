export const CHAT_SHARE_RETURN_KEY = "weknora_chat_share_return_to";

export function normalizeShareReturnPath(raw: unknown): string {
  const value = typeof raw === "string" ? raw.trim() : "";
  if (!value) return "";
  try {
    const parsed = new URL(value, window.location.origin);
    if (parsed.origin !== window.location.origin) return "";
    if (!parsed.pathname.startsWith("/share/chat/")) return "";
    return `${parsed.pathname}${parsed.search}${parsed.hash}`;
  } catch {
    return "";
  }
}

export function rememberShareReturnPath(raw: unknown): string {
  const path = normalizeShareReturnPath(raw);
  if (!path) return "";
  try {
    sessionStorage.setItem(CHAT_SHARE_RETURN_KEY, path);
  } catch {
    // ignore storage failures
  }
  return path;
}

export function consumeShareReturnPath(raw?: unknown): string {
  const fromArg = normalizeShareReturnPath(raw);
  if (fromArg) {
    try {
      sessionStorage.removeItem(CHAT_SHARE_RETURN_KEY);
    } catch {
      // ignore storage failures
    }
    return fromArg;
  }
  try {
    const stored = normalizeShareReturnPath(sessionStorage.getItem(CHAT_SHARE_RETURN_KEY) || "");
    sessionStorage.removeItem(CHAT_SHARE_RETURN_KEY);
    return stored;
  } catch {
    return "";
  }
}
