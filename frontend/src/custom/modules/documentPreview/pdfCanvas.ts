import pdfWorkerUrl from "pdfjs-dist/legacy/build/pdf.worker.min.mjs?url";

type PDFCanvasPreviewOptions = {
  blob: Blob;
  container: HTMLElement;
  signal?: AbortSignal;
};

export type PDFCanvasPreviewSession = {
  ready: Promise<void>;
  destroy: () => void;
};

const MAX_RENDERED_PAGE_DISTANCE = 4;
const MAX_CANVAS_PIXELS = 8_000_000;
// The first production rollout served this immutable asset with the default
// octet-stream MIME type. Keep an explicit cache key so embedded WebViews do
// not reuse that rejected response after the server-side MIME fix.
const PDF_WORKER_CACHE_KEY = "20260806-mjs-mime-v2";
const versionedPDFWorkerUrl = `${pdfWorkerUrl}${pdfWorkerUrl.includes("?") ? "&" : "?"}v=${PDF_WORKER_CACHE_KEY}`;
const PDF_ASSET_BASE_URL = `${String(import.meta.env.BASE_URL || "/").replace(/\/?$/, "/")}pdfjs/`;

function abortError() {
  return new DOMException("PDF preview aborted", "AbortError");
}

/**
 * Render PDF pages into canvases instead of relying on the browser PDF plugin.
 *
 * Android enterprise WebViews (including the WeCom embedded browser) commonly
 * display a blank frame for `blob:` PDF iframes. PDF.js keeps the preview fully
 * inside our application. Pages are rendered lazily and distant canvases are
 * released so a long document cannot exhaust a mobile device's memory.
 */
export function mountPDFCanvasPreview(
  options: PDFCanvasPreviewOptions,
): PDFCanvasPreviewSession {
  const { blob, container, signal } = options;
  let destroyed = false;
  let loadingTask: any = null;
  let pdfDocument: any = null;
  let observer: IntersectionObserver | null = null;
  let resizeObserver: ResizeObserver | null = null;
  let resizeTimer: number | null = null;
  let lastWidth = 0;
  const renderTasks = new Map<number, any>();
  const renderedPages = new Set<number>();
  const renderingPages = new Set<number>();
  const queuedPages = new Set<number>();
  const queue: number[] = [];
  const slots: HTMLElement[] = [];
  let queueRunning = false;

  const scrollRoot = container.parentElement || container;

  function assertActive() {
    if (destroyed || signal?.aborted) throw abortError();
  }

  function unloadDistantPages(anchor: number) {
    for (const pageNumber of renderedPages) {
      if (Math.abs(pageNumber - anchor) <= MAX_RENDERED_PAGE_DISTANCE) continue;
      const slot = slots[pageNumber - 1];
      const canvas = slot?.querySelector<HTMLCanvasElement>("canvas");
      if (!canvas) continue;
      canvas.width = 1;
      canvas.height = 1;
      canvas.remove();
      slot.classList.remove("is-rendered");
      renderedPages.delete(pageNumber);
    }
  }

  async function renderPage(pageNumber: number, required = false) {
    if (
      destroyed ||
      renderedPages.has(pageNumber) ||
      renderingPages.has(pageNumber) ||
      !pdfDocument
    ) return;

    const slot = slots[pageNumber - 1];
    if (!slot) return;
    renderingPages.add(pageNumber);
    slot.classList.add("is-rendering");
    slot.classList.remove("has-error");

    try {
      const page = await pdfDocument.getPage(pageNumber);
      assertActive();
      const baseViewport = page.getViewport({ scale: 1 });
      const availableWidth = Math.max(280, scrollRoot.clientWidth - 24);
      const cssScale = availableWidth / Math.max(1, baseViewport.width);
      const cssViewport = page.getViewport({ scale: cssScale });
      let pixelRatio = Math.min(Math.max(window.devicePixelRatio || 1, 1), 2);
      const projectedPixels = cssViewport.width * cssViewport.height * pixelRatio * pixelRatio;
      if (projectedPixels > MAX_CANVAS_PIXELS) {
        pixelRatio *= Math.sqrt(MAX_CANVAS_PIXELS / projectedPixels);
      }
      const renderViewport = page.getViewport({ scale: cssScale * pixelRatio });
      const canvas = document.createElement("canvas");
      const context = canvas.getContext("2d", { alpha: false });
      if (!context) throw new Error("Canvas 2D is unavailable");
      canvas.className = "pdf-page-canvas";
      canvas.setAttribute("aria-label", `PDF 第 ${pageNumber} 页`);
      canvas.width = Math.max(1, Math.floor(renderViewport.width));
      canvas.height = Math.max(1, Math.floor(renderViewport.height));
      canvas.style.width = `${Math.floor(cssViewport.width)}px`;
      canvas.style.height = `${Math.floor(cssViewport.height)}px`;
      slot.style.minHeight = `${Math.floor(cssViewport.height)}px`;

      const task = page.render({ canvasContext: context, viewport: renderViewport });
      renderTasks.set(pageNumber, task);
      await task.promise;
      assertActive();
      slot.querySelector("canvas")?.remove();
      slot.prepend(canvas);
      slot.classList.add("is-rendered");
      renderedPages.add(pageNumber);
      unloadDistantPages(pageNumber);
      page.cleanup();
    } catch (reason: any) {
      if (!destroyed && !signal?.aborted && reason?.name !== "RenderingCancelledException") {
        console.error(`PDF page ${pageNumber} render failed:`, reason);
        slot.classList.add("has-error");
        const status = slot.querySelector<HTMLElement>(".pdf-page-status");
        if (status) status.textContent = `第 ${pageNumber} 页加载失败，请滚动后重试`;
        if (required) {
          throw new Error(`PDF 第 ${pageNumber} 页解码失败`);
        }
      }
    } finally {
      renderTasks.delete(pageNumber);
      renderingPages.delete(pageNumber);
      slot.classList.remove("is-rendering");
    }
  }

  async function drainQueue() {
    if (queueRunning) return;
    queueRunning = true;
    try {
      while (!destroyed && queue.length) {
        const pageNumber = queue.shift()!;
        queuedPages.delete(pageNumber);
        await renderPage(pageNumber);
      }
    } finally {
      queueRunning = false;
    }
  }

  function enqueue(pageNumber: number, priority = false) {
    if (
      destroyed ||
      renderedPages.has(pageNumber) ||
      renderingPages.has(pageNumber) ||
      queuedPages.has(pageNumber)
    ) return;
    queuedPages.add(pageNumber);
    if (priority) queue.unshift(pageNumber);
    else queue.push(pageNumber);
    void drainQueue();
  }

  function createPageSlots(pageCount: number) {
    container.replaceChildren();
    for (let pageNumber = 1; pageNumber <= pageCount; pageNumber += 1) {
      const slot = document.createElement("section");
      slot.className = "pdf-page-slot";
      slot.dataset.pageNumber = String(pageNumber);
      slot.style.minHeight = `${Math.max(396, Math.round((scrollRoot.clientWidth - 24) * 1.414))}px`;
      const status = document.createElement("span");
      status.className = "pdf-page-status";
      status.textContent = `第 ${pageNumber} / ${pageCount} 页`;
      slot.appendChild(status);
      container.appendChild(slot);
      slots.push(slot);
    }
  }

  async function initialize() {
    assertActive();
    const pdfjs = await import("pdfjs-dist/legacy/build/pdf.mjs");
    assertActive();
    pdfjs.GlobalWorkerOptions.workerSrc = versionedPDFWorkerUrl;
    const data = await blob.arrayBuffer();
    assertActive();
    loadingTask = pdfjs.getDocument({
      data,
      cMapUrl: `${PDF_ASSET_BASE_URL}cmaps/`,
      cMapPacked: true,
      standardFontDataUrl: `${PDF_ASSET_BASE_URL}standard_fonts/`,
      wasmUrl: `${PDF_ASSET_BASE_URL}wasm/`,
      iccUrl: `${PDF_ASSET_BASE_URL}iccs/`,
      isEvalSupported: false,
      useWorkerFetch: true,
      useWasm: true,
    });
    pdfDocument = await loadingTask.promise;
    assertActive();
    createPageSlots(pdfDocument.numPages);

    observer = new IntersectionObserver(
      (entries) => {
        for (const entry of entries) {
          if (!entry.isIntersecting) continue;
          const pageNumber = Number((entry.target as HTMLElement).dataset.pageNumber || 0);
          if (pageNumber > 0) enqueue(pageNumber, true);
        }
      },
      { root: scrollRoot, rootMargin: "120% 0px", threshold: 0.01 },
    );
    slots.forEach((slot) => observer?.observe(slot));

    await renderPage(1, true);
    assertActive();
    if (pdfDocument.numPages > 1) enqueue(2);

    if (typeof ResizeObserver !== "undefined") {
      lastWidth = scrollRoot.clientWidth;
      resizeObserver = new ResizeObserver(() => {
        if (destroyed || Math.abs(scrollRoot.clientWidth - lastWidth) < 24) return;
        lastWidth = scrollRoot.clientWidth;
        if (resizeTimer !== null) window.clearTimeout(resizeTimer);
        resizeTimer = window.setTimeout(() => {
          resizeTimer = null;
          const visible = slots.findIndex((slot) => {
            const rect = slot.getBoundingClientRect();
            const rootRect = scrollRoot.getBoundingClientRect();
            return rect.bottom >= rootRect.top && rect.top <= rootRect.bottom;
          });
          const anchor = visible >= 0 ? visible + 1 : 1;
          for (const pageNumber of [...renderedPages]) {
            const canvas = slots[pageNumber - 1]?.querySelector("canvas");
            canvas?.remove();
            renderedPages.delete(pageNumber);
          }
          enqueue(anchor, true);
        }, 120);
      });
      resizeObserver.observe(scrollRoot);
    }
  }

  function destroy() {
    if (destroyed) return;
    destroyed = true;
    observer?.disconnect();
    resizeObserver?.disconnect();
    if (resizeTimer !== null) window.clearTimeout(resizeTimer);
    renderTasks.forEach((task) => {
      try { task.cancel(); } catch { /* already finished */ }
    });
    renderTasks.clear();
    try { void loadingTask?.destroy(); } catch { /* already finished */ }
    try { void pdfDocument?.destroy(); } catch { /* already finished */ }
    container.replaceChildren();
  }

  signal?.addEventListener("abort", destroy, { once: true });
  return { ready: initialize(), destroy };
}
