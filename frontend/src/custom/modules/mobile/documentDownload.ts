import { post } from "@/utils/request";
import {
  resolveMobileArtifactDownloadURL,
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

export async function requestMobileArtifactDownloadLink(
  artifactID: string,
): Promise<MobileDocumentDownloadLink> {
  const id = String(artifactID || "").trim();
  if (!id) throw new Error("产物标识为空");
  const payload = await post(
    `/api/v1/custom/mobile-documents/artifacts/${encodeURIComponent(id)}/download-link`,
  );
  return unwrapMobileDocumentDownloadLink(payload);
}

export async function downloadArtifactNatively(
  artifactID: string,
  navigate: (url: string) => void = (url) => window.location.assign(url),
): Promise<void> {
  const link = await requestMobileArtifactDownloadLink(artifactID);
  const url = resolveMobileArtifactDownloadURL(link.url, window.location.href);

  // Keep the artifact path identical to knowledge downloads in Enterprise
  // WeChat: the system download process receives a retrievable signed URL,
  // never a page-local blob: URL.
  navigate(url);
}

export {
  resolveMobileArtifactDownloadURL,
  resolveMobileDocumentDownloadURL,
  unwrapMobileDocumentDownloadLink,
} from "./documentDownloadLink";
