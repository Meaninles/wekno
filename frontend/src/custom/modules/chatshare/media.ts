export const PROVIDER_IMAGE_PLACEHOLDER =
  "data:image/gif;base64,R0lGODlhAQABAIAAAAAAAP///ywAAAAAAQABAAACAUwAOw==";

const PROVIDER_FILE_SCHEME_RE = /^(local|minio|cos|tos|s3|oss|ks3|obs):\/\/\S+$/i;

export type NormalizedMessageImage = Record<string, any> & {
  url: string;
  name?: string;
};

export type NormalizedMessageAttachment = Record<string, any> & {
  url?: string;
  file_name: string;
  file_size?: number;
  file_type?: string;
};

export type NormalizedArtifact = Record<string, any> & {
  artifact_id: string;
  filename: string;
  file_type: string;
  file_size: number;
  sha256: string;
  download_url: string;
};

export type NormalizedToolResult = Record<string, any> & {
  display_type: string;
  tool_data: Record<string, any>;
  output?: string;
  arguments?: Record<string, any>;
};

export function decodeProviderURL(raw: unknown): string {
  return String(raw || "")
    .trim()
    .replace(/&#x2f;/gi, "/")
    .replace(/&#47;/g, "/")
    .replace(/&amp;/g, "&")
    .replace(/&quot;/g, '"');
}

export function isProviderFileURL(url: unknown): boolean {
  return PROVIDER_FILE_SCHEME_RE.test(decodeProviderURL(url));
}

export function providerFileSource(raw: unknown): string {
  const decoded = decodeProviderURL(raw);
  if (!decoded) return "";
  if (isProviderFileURL(decoded)) return decoded;

  try {
    const baseURL = typeof window !== "undefined" ? window.location.origin : "http://localhost";
    const url = new URL(decoded, baseURL);
    const filePath = decodeProviderURL(url.searchParams.get("file_path") || "");
    return isProviderFileURL(filePath) ? filePath : "";
  } catch {
    return "";
  }
}

export function sharedFileProxyURL(token: string, filePath: string): string {
  return (
    `/api/v1/custom/chat-share/${encodeURIComponent(token)}/files?` +
    new URLSearchParams({ file_path: filePath }).toString()
  );
}

function firstNonEmptyString(...values: unknown[]): string {
  for (const value of values) {
    const text = decodeProviderURL(value);
    if (text) return text;
  }
  return "";
}

function sourceFromMediaItem(item: any): string {
  if (typeof item === "string") return decodeProviderURL(item);
  if (!item || typeof item !== "object") return "";
  return firstNonEmptyString(
    item.url,
    item.data,
    item.src,
    item.file_path,
    item.filePath,
    item.path,
    item.preview_url,
    item.previewUrl,
    item.download_url,
    item.downloadUrl,
  );
}

export function normalizeMessageImages(value: unknown): NormalizedMessageImage[] {
  if (!Array.isArray(value)) return [];
  return value
    .map<NormalizedMessageImage | null>((item, index) => {
      const url = sourceFromMediaItem(item);
      if (!url) return null;
      const base = typeof item === "object" && item !== null ? { ...(item as Record<string, any>) } : {};
      return {
        ...base,
        url,
        name: firstNonEmptyString(base.name, base.file_name, base.filename) || `图片 ${index + 1}`,
      };
    })
    .filter((item): item is NormalizedMessageImage => Boolean(item));
}

export function normalizeMessageAttachments(value: unknown): NormalizedMessageAttachment[] {
  if (!Array.isArray(value)) return [];
  return value
    .map<NormalizedMessageAttachment | null>((item, index) => {
      if (!item || typeof item !== "object") return null;
      const source = sourceFromMediaItem(item);
      const fileName = firstNonEmptyString(
        (item as any).file_name,
        (item as any).filename,
        (item as any).name,
      ) || `附件 ${index + 1}`;
      return {
        ...(item as Record<string, any>),
        ...(source ? { url: source } : {}),
        file_name: fileName,
        file_size: Number((item as any).file_size || (item as any).size || 0),
        file_type: firstNonEmptyString((item as any).file_type, (item as any).type),
      };
    })
    .filter((item): item is NormalizedMessageAttachment => Boolean(item));
}

export function normalizeMessageArtifacts(value: unknown): NormalizedArtifact[] {
  if (!Array.isArray(value)) return [];
  return value
    .map<NormalizedArtifact | null>((item, index) => {
      if (!item || typeof item !== "object") return null;
      const artifactId = firstNonEmptyString(
        (item as any).artifact_id,
        (item as any).id,
        (item as any).file_token,
      );
      const filename = firstNonEmptyString(
        (item as any).filename,
        (item as any).file_name,
        (item as any).name,
      ) || `产物 ${index + 1}`;
      if (!artifactId && !(item as any).download_url) return null;
      return {
        ...(item as Record<string, any>),
        artifact_id: artifactId || firstNonEmptyString((item as any).download_url, filename),
        filename,
        file_type: firstNonEmptyString((item as any).file_type, (item as any).type),
        file_size: Number((item as any).file_size || (item as any).size || 0),
        sha256: firstNonEmptyString((item as any).sha256),
        download_url: firstNonEmptyString((item as any).download_url, (item as any).url),
      };
    })
    .filter((item): item is NormalizedArtifact => Boolean(item));
}

export function normalizeMessageToolResults(value: unknown): NormalizedToolResult[] {
  if (!Array.isArray(value)) return [];
  return value
    .map<NormalizedToolResult | null>((item) => {
      if (!item || typeof item !== "object") return null;
      const source = item as Record<string, any>;
      const toolData = (
        source.tool_data && typeof source.tool_data === "object"
          ? source.tool_data
          : source.data && typeof source.data === "object"
            ? source.data
            : source
      ) as Record<string, any>;
      const displayType = firstNonEmptyString(source.display_type, toolData.display_type);
      if (!displayType || !toolData || typeof toolData !== "object") return null;
      return {
        ...source,
        display_type: displayType,
        tool_data: {
          ...toolData,
          display_type: displayType,
        },
        output: firstNonEmptyString(source.output),
        arguments: source.arguments && typeof source.arguments === "object"
          ? source.arguments as Record<string, any>
          : undefined,
      };
    })
    .filter((item): item is NormalizedToolResult => Boolean(item));
}

export function shareImageSrc(rawURL: unknown, shareMode?: boolean): string {
  const url = decodeProviderURL(rawURL);
  if (shareMode && providerFileSource(url)) return PROVIDER_IMAGE_PLACEHOLDER;
  return url;
}

export function shareImageProtectedSrc(rawURL: unknown, shareMode?: boolean): string | undefined {
  if (!shareMode) return undefined;
  return providerFileSource(rawURL) || undefined;
}
