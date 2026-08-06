import { getApiBaseUrl } from "../../../utils/api-base.ts";

export type PublicReferenceDocument = {
  title: string;
  file_name: string;
  file_type: string;
  file_size: number;
  created_at: string;
  updated_at: string;
};

export type PublicReferenceFragment = {
  content: string;
  chunk_index: number;
  start_at: number;
  end_at: number;
  chunk_type: string;
  source_locator?: Record<string, unknown> | null;
};

export type PublicReferenceWikiPage = {
  slug: string;
  title: string;
  page_type: string;
  summary: string;
  content: string;
  version: number;
  updated_at: string;
};

export type PublicReferenceView = {
  type: "knowledge" | "wiki";
  title: string;
  document?: PublicReferenceDocument;
  fragment?: PublicReferenceFragment;
  wiki?: PublicReferenceWikiPage;
};

type RouteQueryValue = string | null | Array<string | null> | undefined;

const REFERENCE_DATA_PATH = "/api/v1/custom/im-output/reference/data";
const REFERENCE_ORIGINAL_PATH = "/api/v1/custom/im-output/reference/original";

export function publicReferenceToken(query: Record<string, RouteQueryValue>): string {
  const raw = query.token;
  return String(Array.isArray(raw) ? raw[0] || "" : raw || "").trim();
}

export async function loadPublicReference(token: string, signal?: AbortSignal): Promise<PublicReferenceView> {
  if (!token) throw new Error("引用地址缺少访问凭证");
  const url = publicReferenceURL(REFERENCE_DATA_PATH, token);
  let status = 0;
  let payload: any = null;
  try {
    const response = await fetch(url, {
      method: "GET",
      headers: { Accept: "application/json" },
      credentials: "omit",
      cache: "no-store",
      referrerPolicy: "no-referrer",
      signal,
    });
    status = response.status;
    payload = await response.json().catch(() => null);
  } catch (error) {
    if (signal?.aborted || isAbortError(error)) throw error;
    ({ status, payload } = await loadPublicReferenceWithXHR(url, signal));
  }
  if (status < 200 || status >= 300 || !payload?.success || !isPublicReferenceView(payload.data)) {
    throw new Error(String(payload?.message || "引用链接无效或内容已不可用"));
  }
  return payload.data;
}

export async function loadPublicReferenceOriginal(token: string, signal?: AbortSignal): Promise<Blob> {
  if (!token) throw new Error("引用地址缺少访问凭证");
  const url = publicReferenceOriginalURL(token);
  let response: Response;
  try {
    response = await fetch(url, {
      method: "GET",
      credentials: "omit",
      cache: "no-store",
      referrerPolicy: "no-referrer",
      signal,
    });
  } catch (error) {
    if (signal?.aborted || isAbortError(error)) throw error;
    return loadPublicReferenceOriginalWithXHR(url, signal);
  }
  if (!response.ok) {
    const payload = await response.json().catch(() => null);
    throw new Error(String(payload?.message || "原文档暂时无法查看"));
  }
  return response.blob();
}

export function publicReferenceOriginalURL(token: string): string {
  return publicReferenceURL(REFERENCE_ORIGINAL_PATH, token);
}

export function publicReferenceDownloadURL(token: string): string {
  const url = new URL(publicReferenceOriginalURL(token), window.location.href);
  url.searchParams.set("download", "1");
  return url.toString();
}

export function downloadPublicReferenceOriginalNatively(
  token: string,
  navigate: (url: string) => void = (url) => window.location.assign(url),
): void {
  if (!token) throw new Error("引用地址缺少访问凭证");

  // Keep this as a top-level navigation to a signed server response. WeCom's
  // desktop and mobile WebViews cannot hand page-local blob: URLs to their
  // system download process, while Content-Disposition: attachment works in
  // both WeCom and ordinary external browsers without opening another page.
  navigate(publicReferenceDownloadURL(token));
}

export function formatReferenceTime(value?: string): string {
  if (!value) return "";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return value;
  return date.toLocaleString("zh-CN", {
    year: "numeric",
    month: "2-digit",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
    hour12: false,
  });
}

export function formatReferenceFileType(document?: PublicReferenceDocument): string {
  const explicit = String(document?.file_type || "").replace(/^\./, "").trim();
  if (explicit) return explicit.toUpperCase();
  const fileName = String(document?.file_name || document?.title || "");
  const extension = fileName.includes(".") ? fileName.split(".").pop() || "" : "";
  return extension ? extension.toUpperCase() : "文档";
}

export function formatSourceLocator(locator?: Record<string, unknown> | null): string {
  if (!locator || typeof locator !== "object") return "";
  const labels: string[] = [];
  const page = positiveNumber(locator.page ?? locator.page_number);
  const sheet = cleanText(locator.sheet ?? locator.sheet_name);
  const rowStart = positiveNumber(locator.row_start);
  const rowEnd = positiveNumber(locator.row_end);
  const lineStart = positiveNumber(locator.line_start);
  const lineEnd = positiveNumber(locator.line_end);
  const slide = positiveNumber(locator.slide ?? locator.slide_number);
  if (page) labels.push(`第 ${page} 页`);
  if (slide) labels.push(`第 ${slide} 页幻灯片`);
  if (sheet) labels.push(`工作表 ${sheet}`);
  if (rowStart) labels.push(rowEnd && rowEnd !== rowStart ? `第 ${rowStart}–${rowEnd} 行` : `第 ${rowStart} 行`);
  if (lineStart) labels.push(lineEnd && lineEnd !== lineStart ? `第 ${lineStart}–${lineEnd} 行` : `第 ${lineStart} 行`);
  return labels.join(" · ");
}

export function wikiPageTypeLabel(type?: string): string {
  const labels: Record<string, string> = {
    summary: "摘要",
    entity: "实体",
    concept: "概念",
    synthesis: "综合",
    comparison: "对比",
    index: "索引",
    log: "记录",
  };
  return type ? labels[type] || type : "Wiki";
}

function publicReferenceURL(path: string, token: string): string {
  const base = getApiBaseUrl().replace(/\/+$/, "");
  return `${base}${path}?${new URLSearchParams({ token }).toString()}`;
}

function loadPublicReferenceWithXHR(
  url: string,
  signal?: AbortSignal,
): Promise<{ status: number; payload: any }> {
  if (signal?.aborted) return Promise.reject(createAbortError());
  return new Promise((resolve, reject) => {
    const request = new XMLHttpRequest();
    const cleanup = bindXHRAbort(request, signal, reject);
    request.open("GET", url, true);
    request.setRequestHeader("Accept", "application/json");
    request.onload = () => {
      cleanup();
      let payload: any = null;
      try {
        payload = JSON.parse(request.responseText);
      } catch {
        payload = null;
      }
      resolve({ status: request.status, payload });
    };
    request.onerror = () => {
      cleanup();
      reject(new Error("引用内容加载失败"));
    };
    request.onabort = () => {
      cleanup();
      reject(createAbortError());
    };
    request.send();
  });
}

function loadPublicReferenceOriginalWithXHR(url: string, signal?: AbortSignal): Promise<Blob> {
  if (signal?.aborted) return Promise.reject(createAbortError());
  return new Promise((resolve, reject) => {
    const request = new XMLHttpRequest();
    const cleanup = bindXHRAbort(request, signal, reject);
    request.open("GET", url, true);
    request.responseType = "blob";
    request.onload = () => {
      cleanup();
      if (request.status >= 200 && request.status < 300 && request.response instanceof Blob) {
        resolve(request.response);
        return;
      }
      reject(new Error("原文档暂时无法查看"));
    };
    request.onerror = () => {
      cleanup();
      reject(new Error("原文档暂时无法查看"));
    };
    request.onabort = () => {
      cleanup();
      reject(createAbortError());
    };
    request.send();
  });
}

function bindXHRAbort(
  request: XMLHttpRequest,
  signal: AbortSignal | undefined,
  reject: (reason?: any) => void,
): () => void {
  if (!signal) return () => undefined;
  const abort = () => {
    request.abort();
    reject(createAbortError());
  };
  if (signal.aborted) {
    abort();
    return () => undefined;
  }
  signal.addEventListener("abort", abort, { once: true });
  return () => signal.removeEventListener("abort", abort);
}

function isAbortError(error: unknown): boolean {
  return Boolean(error && typeof error === "object" && (error as { name?: string }).name === "AbortError");
}

function createAbortError(): Error {
  if (typeof DOMException === "function") return new DOMException("The operation was aborted", "AbortError");
  const error = new Error("The operation was aborted");
  error.name = "AbortError";
  return error;
}

function isPublicReferenceView(value: unknown): value is PublicReferenceView {
  if (!value || typeof value !== "object") return false;
  const candidate = value as PublicReferenceView;
  if (candidate.type === "knowledge") {
    return Boolean(candidate.document && candidate.fragment && typeof candidate.fragment.content === "string");
  }
  if (candidate.type === "wiki") {
    return Boolean(candidate.wiki && typeof candidate.wiki.content === "string");
  }
  return false;
}

function positiveNumber(value: unknown): number | null {
  const number = Number(value);
  return Number.isFinite(number) && number > 0 ? number : null;
}

function cleanText(value: unknown): string {
  return String(value || "").trim();
}
