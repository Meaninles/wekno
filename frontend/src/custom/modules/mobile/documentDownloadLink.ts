export const MOBILE_DOCUMENT_DOWNLOAD_PATH = "/api/v1/custom/mobile-documents/download";

export interface MobileDocumentDownloadLink {
  url: string;
  expires_at?: string;
}

export function unwrapMobileDocumentDownloadLink(payload: any): MobileDocumentDownloadLink {
  const value = payload?.data ?? payload;
  const url = String(value?.url || "").trim();
  if (!url) {
    throw new Error("下载链接创建失败");
  }
  return {
    url,
    expires_at: String(value?.expires_at || ""),
  };
}

export function resolveMobileDocumentDownloadURL(rawURL: string, pageURL: string): string {
  const resolved = new URL(rawURL, pageURL);
  const page = new URL(pageURL);
  if (resolved.origin !== page.origin || resolved.pathname !== MOBILE_DOCUMENT_DOWNLOAD_PATH) {
    throw new Error("服务端返回了无效的下载链接");
  }
  return resolved.toString();
}
