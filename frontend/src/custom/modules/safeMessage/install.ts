import type { App } from "vue";
import { MessagePlugin } from "tdesign-vue-next";

type NotifyTheme = "error" | "warning";
type NotifyParams = string | { content?: unknown; [key: string]: unknown };
type CloseableMessage = { close: () => void };

const INSTALL_FLAG = "__weknoraSafeMessageInstalled__";
const RECENT_MESSAGE_TTL_MS = 1800;
const DEFAULT_DURATION_MS = 4000;

let container: HTMLDivElement | null = null;
let nextId = 1;
const activeToasts = new Map<number, HTMLDivElement>();
const recentMessages = new Map<string, number>();

const originalCloseAll = MessagePlugin.closeAll?.bind(MessagePlugin);

function getLocale() {
  if (typeof localStorage !== "undefined") {
    const stored = localStorage.getItem("locale");
    if (stored) return stored;
  }
  if (typeof navigator !== "undefined") return navigator.language || "";
  return "";
}

function promptTooLongMessage() {
  return getLocale().toLowerCase().startsWith("zh")
    ? "本次请求内容过长，请减少输入、附件或检索范围后重试。"
    : "The request is too long. Reduce the input, attachments, or search scope and try again.";
}

function toPlainText(value: unknown): string {
  if (typeof value === "string") return value;
  if (typeof value === "number" || typeof value === "boolean") return String(value);
  if (value instanceof Error) return value.message;
  if (value && typeof value === "object" && "message" in value) {
    return toPlainText((value as { message?: unknown }).message);
  }
  return "";
}

export function normalizeUserFacingError(value: unknown): string {
  const message = toPlainText(value).trim();
  if (!message) return "";

  if (
    /prompt exceeds max length/i.test(message) ||
    /maximum context length/i.test(message) ||
    /context length exceeded/i.test(message) ||
    /exceeds? (the )?(maximum )?(token|context)/i.test(message)
  ) {
    return promptTooLongMessage();
  }

  return message;
}

function getContent(params: NotifyParams): string {
  if (typeof params === "string") return params;
  return toPlainText(params?.content);
}

function ensureContainer() {
  if (container && document.body.contains(container)) return container;
  container = document.createElement("div");
  container.className = "weknora-safe-message-container";
  container.setAttribute("aria-live", "polite");
  Object.assign(container.style, {
    position: "fixed",
    top: "16px",
    left: "50%",
    transform: "translateX(-50%)",
    zIndex: "2147483647",
    display: "flex",
    flexDirection: "column",
    alignItems: "center",
    gap: "8px",
    pointerEvents: "none",
    width: "min(92vw, 520px)",
  });
  document.body.appendChild(container);
  return container;
}

function pruneRecentMessages(now: number) {
  for (const [key, timestamp] of recentMessages) {
    if (now - timestamp > RECENT_MESSAGE_TTL_MS) {
      recentMessages.delete(key);
    }
  }
}

function shouldSkipDuplicate(theme: NotifyTheme, text: string) {
  const now = Date.now();
  pruneRecentMessages(now);
  const key = `${theme}:${text}`;
  const last = recentMessages.get(key);
  if (last && now - last < RECENT_MESSAGE_TTL_MS) return true;
  recentMessages.set(key, now);
  return false;
}

function createToast(theme: NotifyTheme, text: string, duration?: number): CloseableMessage {
  const host = ensureContainer();
  const id = nextId++;
  const toast = document.createElement("div");
  const isError = theme === "error";

  toast.className = `weknora-safe-message weknora-safe-message-${theme}`;
  toast.textContent = text;
  toast.setAttribute("role", isError ? "alert" : "status");
  Object.assign(toast.style, {
    boxSizing: "border-box",
    width: "fit-content",
    maxWidth: "100%",
    padding: "10px 14px",
    borderRadius: "6px",
    fontSize: "14px",
    lineHeight: "1.45",
    color: isError ? "#b42318" : "#7a4f01",
    background: isError ? "#fff1f0" : "#fff8e1",
    border: `1px solid ${isError ? "#ffccc7" : "#f3d27a"}`,
    boxShadow: "0 8px 24px rgba(15, 23, 42, 0.14)",
    pointerEvents: "auto",
    opacity: "0",
    transform: "translateY(-6px)",
    transition: "opacity 120ms ease, transform 120ms ease",
    wordBreak: "break-word",
    whiteSpace: "pre-wrap",
  });

  const close = () => {
    if (!activeToasts.has(id)) return;
    activeToasts.delete(id);
    toast.style.opacity = "0";
    toast.style.transform = "translateY(-6px)";
    window.setTimeout(() => {
      toast.remove();
      if (container && container.childElementCount === 0) {
        container.remove();
        container = null;
      }
    }, 140);
  };

  activeToasts.set(id, toast);
  host.appendChild(toast);
  window.requestAnimationFrame(() => {
    toast.style.opacity = "1";
    toast.style.transform = "translateY(0)";
  });

  const ttl = typeof duration === "number" ? duration : DEFAULT_DURATION_MS;
  if (ttl > 0) window.setTimeout(close, ttl);

  return { close };
}

function showSafeMessage(theme: NotifyTheme, params: NotifyParams, duration?: number) {
  if (typeof document === "undefined") {
    return Promise.resolve({ close: () => undefined } as CloseableMessage);
  }

  const rawText = getContent(params);
  const text = theme === "error" ? normalizeUserFacingError(rawText) : rawText.trim();
  if (!text || shouldSkipDuplicate(theme, text)) {
    return Promise.resolve({ close: () => undefined } as CloseableMessage);
  }

  return Promise.resolve(createToast(theme, text, duration));
}

function closeSafeMessages() {
  for (const toast of Array.from(activeToasts.values())) {
    toast.remove();
  }
  activeToasts.clear();
  if (container) {
    container.remove();
    container = null;
  }
}

export function installSafeMessagePlugin(app?: App) {
  const target = typeof window !== "undefined" ? (window as unknown as Record<string, unknown>) : null;
  if (target?.[INSTALL_FLAG]) return;
  if (target) target[INSTALL_FLAG] = true;

  MessagePlugin.error = ((params: NotifyParams, duration?: number) =>
    showSafeMessage("error", params, duration)) as typeof MessagePlugin.error;
  MessagePlugin.warning = ((params: NotifyParams, duration?: number) =>
    showSafeMessage("warning", params, duration)) as typeof MessagePlugin.warning;
  MessagePlugin.closeAll = (() => {
    closeSafeMessages();
    originalCloseAll?.();
  }) as typeof MessagePlugin.closeAll;

  const globalMessage = app?.config.globalProperties.$message;
  if (globalMessage) {
    globalMessage.error = MessagePlugin.error;
    globalMessage.warning = MessagePlugin.warning;
    globalMessage.closeAll = MessagePlugin.closeAll;
  }
}
