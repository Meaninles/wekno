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
  const response = await fetch(publicReferenceURL(REFERENCE_DATA_PATH, token), {
    method: "GET",
    headers: { Accept: "application/json" },
    credentials: "omit",
    cache: "no-store",
    referrerPolicy: "no-referrer",
    signal,
  });
  const payload = await response.json().catch(() => null);
  if (!response.ok || !payload?.success || !isPublicReferenceView(payload.data)) {
    throw new Error(String(payload?.message || "引用链接无效或内容已不可用"));
  }
  return payload.data;
}

export async function loadPublicReferenceOriginal(token: string, signal?: AbortSignal): Promise<Blob> {
  if (!token) throw new Error("引用地址缺少访问凭证");
  const response = await fetch(publicReferenceOriginalURL(token), {
    method: "GET",
    credentials: "omit",
    cache: "no-store",
    referrerPolicy: "no-referrer",
    signal,
  });
  if (!response.ok) {
    const payload = await response.json().catch(() => null);
    throw new Error(String(payload?.message || "原文档暂时无法查看"));
  }
  return response.blob();
}

export function publicReferenceOriginalURL(token: string): string {
  return publicReferenceURL(REFERENCE_ORIGINAL_PATH, token);
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
