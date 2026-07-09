import {
  PROVIDER_IMAGE_PLACEHOLDER,
  providerFileSource,
  sharedFileProxyURL,
} from "./media";

type SharedImageLoadResult =
  | { status: "loaded"; blobURL: string }
  | { status: "missing" }
  | { status: "failed" };

const blobByRequest = new Map<string, string>();
const blobBySource = new Map<string, string>();
const missingSources = new Set<string>();
const failures = new Map<string, number>();
const inflight = new Map<string, Promise<SharedImageLoadResult>>();
const RETRY_COOLDOWN_MS = 5000;

function protectedImageSource(img: HTMLImageElement): string {
  return (
    providerFileSource(img.getAttribute("data-protected-src") || "") ||
    providerFileSource(img.getAttribute("src") || "")
  );
}

function authHeaders(): Record<string, string> {
  const headers: Record<string, string> = {};
  const token = (localStorage.getItem("weknora_token") || "").trim();
  if (token) headers.Authorization = `Bearer ${token}`;
  const selectedTenantId = (localStorage.getItem("weknora_selected_tenant_id") || "").trim();
  if (selectedTenantId) headers["X-Tenant-ID"] = selectedTenantId;
  return headers;
}

function forEachSharedImageWithSource(
  root: ParentNode,
  sourceURL: string,
  callback: (img: HTMLImageElement) => void,
): void {
  root.querySelectorAll<HTMLImageElement>(
    'img[data-protected-src], img[src^="local://"], img[src^="minio://"], img[src^="cos://"], img[src^="tos://"], img[src^="s3://"], img[src^="oss://"], img[src^="ks3://"], img[src^="obs://"]',
  ).forEach((candidate) => {
    if (protectedImageSource(candidate) === sourceURL) callback(candidate);
  });
}

function removeMissingSharedImages(root: ParentNode, sourceURL: string): void {
  forEachSharedImageWithSource(root, sourceURL, (img) => {
    const parent = img.parentElement;
    img.remove();
    if (parent?.tagName === "P" && !parent.textContent?.trim() && parent.children.length === 0) {
      parent.remove();
    }
  });
}

function applyHydratedSharedImage(root: ParentNode, sourceURL: string, blobURL: string): void {
  forEachSharedImageWithSource(root, sourceURL, (img) => {
    img.src = blobURL;
    img.dataset.shareHydrated = "1";
    img.removeAttribute("data-img-loading");
  });
}

async function loadSharedImage(requestURL: string): Promise<SharedImageLoadResult> {
  const cached = blobByRequest.get(requestURL);
  if (cached) return { status: "loaded", blobURL: cached };

  const lastFailure = failures.get(requestURL);
  if (lastFailure !== undefined && Date.now() - lastFailure < RETRY_COOLDOWN_MS) {
    return { status: "failed" };
  }

  let task = inflight.get(requestURL);
  if (!task) {
    task = (async () => {
      try {
        const resp = await fetch(requestURL, {
          method: "GET",
          headers: authHeaders(),
          credentials: "include",
        });
        if (resp.status === 404) {
          failures.set(requestURL, Date.now());
          return { status: "missing" as const };
        }
        if (!resp.ok) throw new Error(`HTTP ${resp.status}`);
        const blobURL = URL.createObjectURL(await resp.blob());
        blobByRequest.set(requestURL, blobURL);
        failures.delete(requestURL);
        return { status: "loaded" as const, blobURL };
      } catch (error) {
        console.warn("[chatshare] hydrate shared protected image failed:", error);
        failures.set(requestURL, Date.now());
        return { status: "failed" as const };
      } finally {
        inflight.delete(requestURL);
      }
    })();
    inflight.set(requestURL, task);
  }
  return task;
}

export async function hydrateSharedProtectedFileImages(
  root: ParentNode | null | undefined,
  token: string,
): Promise<void> {
  if (!root || !token || typeof window === "undefined") return;
  const images = root.querySelectorAll<HTMLImageElement>(
    'img[data-protected-src], img[src^="local://"], img[src^="minio://"], img[src^="cos://"], img[src^="tos://"], img[src^="s3://"], img[src^="oss://"], img[src^="ks3://"], img[src^="obs://"]',
  );
  if (!images.length) return;

  await Promise.all(Array.from(images).map(async (img) => {
    const sourceURL = protectedImageSource(img);
    if (!sourceURL) return;
    if (missingSources.has(sourceURL)) {
      removeMissingSharedImages(root, sourceURL);
      return;
    }

    img.setAttribute("data-protected-src", sourceURL);
    if (img.dataset.shareHydrated === "1") return;

    const cachedBySource = blobBySource.get(sourceURL);
    if (cachedBySource) {
      applyHydratedSharedImage(root, sourceURL, cachedBySource);
      return;
    }

    img.dataset.shareHydrated = "1";
    if (!img.getAttribute("src")?.startsWith("blob:")) {
      img.setAttribute("src", PROVIDER_IMAGE_PLACEHOLDER);
      img.setAttribute("data-img-loading", "1");
    }

    const requestURL = sharedFileProxyURL(token, sourceURL);
    const result = await loadSharedImage(requestURL);
    if (result.status === "loaded") {
      blobBySource.set(sourceURL, result.blobURL);
      missingSources.delete(sourceURL);
      applyHydratedSharedImage(root, sourceURL, result.blobURL);
      return;
    }
    if (result.status === "missing") {
      missingSources.add(sourceURL);
      removeMissingSharedImages(root, sourceURL);
      return;
    }
    img.dataset.shareHydrated = "0";
  }));
}
