export const AUTH_RETURN_KEY = "weknora_auth_return_to";

function browserOrigin(): string {
  return typeof window === "undefined" ? "" : window.location.origin;
}

/**
 * Keep post-login navigation same-origin and limited to authenticated product
 * surfaces. Query parameters are retained because citation deep links carry
 * exact knowledge_id/chunk_id or tab/slug coordinates.
 */
export function normalizeAuthReturnPath(raw: unknown, origin = browserOrigin()): string {
  const value = typeof raw === "string" ? raw.trim() : "";
  if (!value || !origin) return "";
  try {
    const parsed = new URL(value, origin);
    if (parsed.origin !== origin) return "";
    const allowed = parsed.pathname === "/platform"
      || parsed.pathname.startsWith("/platform/")
      || parsed.pathname.startsWith("/share/chat/");
    if (!allowed) return "";
    return `${parsed.pathname}${parsed.search}${parsed.hash}`;
  } catch {
    return "";
  }
}

export function rememberAuthReturnPath(raw: unknown): string {
  const path = normalizeAuthReturnPath(raw);
  if (!path) return "";
  try {
    sessionStorage.setItem(AUTH_RETURN_KEY, path);
  } catch {
    // Storage may be unavailable in hardened embedded browsers. The caller
    // still carries return_to through the signed IAM state in that case.
  }
  return path;
}

export function consumeAuthReturnPath(raw?: unknown): string {
  const fromArg = normalizeAuthReturnPath(raw);
  if (fromArg) {
    try {
      sessionStorage.removeItem(AUTH_RETURN_KEY);
    } catch {
      // ignore storage failures
    }
    return fromArg;
  }
  try {
    const stored = normalizeAuthReturnPath(sessionStorage.getItem(AUTH_RETURN_KEY) || "");
    sessionStorage.removeItem(AUTH_RETURN_KEY);
    return stored;
  } catch {
    return "";
  }
}

export function isWeComUserAgent(userAgent?: string): boolean {
  const value = userAgent ?? (typeof navigator === "undefined" ? "" : navigator.userAgent);
  return /\bwxwork\//i.test(value);
}

export function iamSSOEntryForReturnPath(raw: unknown, origin = browserOrigin()): string {
  const path = normalizeAuthReturnPath(raw, origin);
  if (!path) return "/api/v1/custom/iam/sso/entry";
  return `/api/v1/custom/iam/sso/entry?${new URLSearchParams({ return_to: path }).toString()}`;
}
