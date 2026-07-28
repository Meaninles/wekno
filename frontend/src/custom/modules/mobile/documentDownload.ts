import { post } from "@/utils/request";
import {
  resolveMobileDocumentDownloadURL,
  unwrapMobileDocumentDownloadLink,
  type MobileDocumentDownloadLink,
} from "./documentDownloadLink";

export async function requestMobileDocumentDownloadLink(
  knowledgeID: string,
): Promise<MobileDocumentDownloadLink> {
  const id = String(knowledgeID || "").trim();
  if (!id) throw new Error("文档标识为空");
  const payload = await post(
    `/api/v1/custom/mobile-documents/knowledge/${encodeURIComponent(id)}/download-link`,
  );
  return unwrapMobileDocumentDownloadLink(payload);
}

export async function downloadKnowledgeNatively(
  knowledgeID: string,
  navigate: (url: string) => void = (url) => window.location.assign(url),
): Promise<void> {
  const link = await requestMobileDocumentDownloadLink(knowledgeID);
  const url = resolveMobileDocumentDownloadURL(link.url, window.location.href);

  // Do not turn the response into a Blob. Enterprise WeChat delegates downloads
  // to a process that cannot dereference a page-local blob: URL. A top-level
  // navigation to the short-lived signed server response is retrievable by both
  // the WebView and its system download handler.
  navigate(url);
}

export {
  resolveMobileDocumentDownloadURL,
  unwrapMobileDocumentDownloadLink,
} from "./documentDownloadLink";
