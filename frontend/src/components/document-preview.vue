// @ts-nocheck
<script setup lang="ts">
import { ref, shallowRef, watch, onUnmounted, nextTick, defineAsyncComponent } from 'vue';
import {
  getKnowledgePreviewPolicy,
  previewKnowledgeFile,
} from '@/api/knowledge-base/index';
import hljs from 'highlight.js';
import 'highlight.js/styles/github.css';
import markedKatex from 'marked-katex-extension';
import 'katex/dist/katex.min.css';
import { useI18n } from 'vue-i18n';
import { sanitizeHTML, safeMarkdownToHTML } from '@/utils/security';
import {
  ensureDocumentPreviewBlobType,
  resolveDocumentPreviewType,
  type DocumentPreviewType,
} from '@/utils/documentPreview';
import {
  DEFAULT_LOADER_BLOB_LIMIT,
  blobExceedsAdmission,
  boundedTextBlob,
  evaluatePreviewAdmission,
  unwrapKnowledgePreviewPolicy,
} from '@/custom/modules/documentPreview/policy';
import {
  mountPDFCanvasPreview,
  type PDFCanvasPreviewSession,
} from '@/custom/modules/documentPreview/pdfCanvas';


const VueOfficePptx = defineAsyncComponent(() => import('@vue-office/pptx'));

const { t } = useI18n();

const props = defineProps<{
  knowledgeId?: string;
  sourceKey?: string;
  fileType: string;
  fileName: string;
  active: boolean;
  fileSize?: number | string;
  chunkCount?: number | string;
  parseStatus?: string;
  blobLoader?: (signal?: AbortSignal) => Promise<Blob>;
  mobileFit?: boolean;
}>();
const emit = defineEmits<{
  (event: 'useChunks'): void;
}>();

const loading = ref(false);
const contentRendering = ref(false);
const error = ref('');
const previewType = ref<DocumentPreviewType>('unsupported');
const blobUrl = ref('');
const textContent = ref('');
const highlightedCode = ref('');
const markdownHtml = ref('');
const excelHtml = ref('');
const pptxData = shallowRef<ArrayBuffer | null>(null);
const docxContainer = ref<HTMLElement | null>(null);
const pdfContainer = ref<HTMLElement | null>(null);
const imageNaturalWidth = ref(0);
const imageNaturalHeight = ref(0);
const boundedPreview = ref(false);
const boundedReason = ref('');
const textTruncated = ref(false);
let loadedForId = '';
let activeController: AbortController | null = null;
let pdfSession: PDFCanvasPreviewSession | null = null;
let loadGeneration = 0;

const isFullscreen = ref(false);

function toggleFullscreen() {
  isFullscreen.value = !isFullscreen.value;
  if (isFullscreen.value) {
    document.body.style.overflow = 'hidden';
  } else {
    document.body.style.overflow = '';
  }
}


const langMap: Record<string, string> = {
  js: 'javascript', ts: 'typescript', py: 'python', rb: 'ruby',
  sh: 'bash', yml: 'yaml', md: 'markdown', rs: 'rust',
  kt: 'kotlin', pl: 'perl', conf: 'ini', log: 'plaintext',
};

function getHighlightLang(ft: string): string {
  const lower = ft?.toLowerCase() || '';
  return langMap[lower] || lower;
}

const preprocessMathDelimiters = (rawText: string): string => {
  if (!rawText || typeof rawText !== 'string') {
    return '';
  }
  return rawText
    .replace(/\\\[([\s\S]*?)\\\]/g, '$$$$$1$$$$')
    .replace(/\\\(([\s\S]*?)\\\)/g, '$$$1$$');
};

async function renderDocx(blob: Blob) {
  const { renderAsync } = await import('docx-preview');
  if (docxContainer.value) {
    docxContainer.value.innerHTML = '';
    await renderAsync(blob, docxContainer.value, undefined, {
      className: 'docx-preview-wrapper',
      inWrapper: true,
      // Reflow print-sized pages and tables only in the public mobile reader.
      // Desktop previews retain their print-faithful dimensions.
      ignoreWidth: props.mobileFit === true,
      ignoreHeight: props.mobileFit === true,
      ignoreFonts: false,
      breakPages: true,
      ignoreLastRenderedPageBreak: true,
      experimental: false,
      trimXmlDeclaration: true,
      useBase64URL: true,
    });
  }
}

function isValidUTF8(bytes: Uint8Array): boolean {
  for (let i = 0; i < bytes.length;) {
    const b = bytes[i];
    let remaining = 0;
    if (b <= 0x7F) { remaining = 0; }
    else if ((b & 0xE0) === 0xC0) { remaining = 1; }
    else if ((b & 0xF0) === 0xE0) { remaining = 2; }
    else if ((b & 0xF8) === 0xF0) { remaining = 3; }
    else { return false; }
    if (i + remaining >= bytes.length) return false;
    for (let j = 1; j <= remaining; j++) {
      if ((bytes[i + j] & 0xC0) !== 0x80) return false;
    }
    i += 1 + remaining;
  }
  return true;
}

function decodeCSVBlob(arrayBuffer: ArrayBuffer): string {
  const bytes = new Uint8Array(arrayBuffer);
  if (bytes[0] === 0xEF && bytes[1] === 0xBB && bytes[2] === 0xBF) {
    return new TextDecoder('utf-8').decode(bytes);
  }
  if (isValidUTF8(bytes)) {
    return new TextDecoder('utf-8').decode(bytes);
  }
  return new TextDecoder('gbk').decode(bytes);
}

async function renderExcel(blob: Blob, fileType?: string) {
  const XLSX = await import('xlsx');
  const arrayBuffer = await blob.arrayBuffer();

  let workbook;
  if (fileType?.toLowerCase() === 'csv') {
    const csvText = decodeCSVBlob(arrayBuffer);
    workbook = XLSX.read(csvText, { type: 'string' });
  } else {
    workbook = XLSX.read(arrayBuffer, { type: 'array' });
  }

  let html = '';
  workbook.SheetNames.forEach((name, sheetIdx) => {
    const sheet = workbook.Sheets[name];
    const sheetHtml = XLSX.utils.sheet_to_html(sheet, { id: `sheet-${sheetIdx}` });
    html += `<div class="excel-sheet">`;
    if (workbook.SheetNames.length > 1) {
      html += `<div class="excel-sheet-name">${name}</div>`;
    }
    html += sheetHtml;
    html += `</div>`;
  });
  excelHtml.value = sanitizeHTML(html);
}

async function renderText(blob: Blob, fileType: string) {
  const text = await blob.text();
  textContent.value = text;

  const lang = getHighlightLang(fileType);
  if (lang && hljs.getLanguage(lang)) {
    try {
      highlightedCode.value = hljs.highlight(text, { language: lang }).value;
      return;
    } catch { /* fallthrough */ }
  }
  const auto = hljs.highlightAuto(text);
  highlightedCode.value = auto.value;
}

async function renderMarkdown(blob: Blob) {
  const { marked } = await import('marked');
  const text = await blob.text();

  // 校验文本内容是否有效
  if (!text || typeof text !== 'string') {
    markdownHtml.value = '<p style="color: var(--td-text-color-disabled); text-align: center; padding: 20px;">文档内容为空</p>';
    return;
  }

  marked.use({
    breaks: true,
    gfm: true,
  });
  marked.use(markedKatex({ throwOnError: false, nonStandard: true }));
  const renderer = new marked.Renderer();
  renderer.code = function ({text, lang}) {
    // 空值校验：防止 text 为 undefined 或 null
    if (!text || typeof text !== 'string') {
      text = '';
    }

    let highlighted = '';
    if (lang && hljs.getLanguage(lang)) {
      try { highlighted = hljs.highlight(text, { language: lang }).value; }
      catch { highlighted = hljs.highlightAuto(text).value; }
    } else {
      highlighted = hljs.highlightAuto(text).value;
    }
    return `<pre><code class="hljs">${highlighted}</code></pre>`;
  };
  marked.use({ renderer });
  const mathSafeText = preprocessMathDelimiters(text);
  const safeText = safeMarkdownToHTML(mathSafeText);
  const rawHtml = marked.parse(safeText) as string;
  markdownHtml.value = sanitizeHTML(rawHtml);
}

function onImageLoad(e: Event) {
  const img = e.target as HTMLImageElement;
  imageNaturalWidth.value = img.naturalWidth;
  imageNaturalHeight.value = img.naturalHeight;
}

async function loadPreview() {
  const id = props.sourceKey || props.knowledgeId;
  const ft = props.fileType;
  if (!id || !ft || (!props.blobLoader && !props.knowledgeId)) return;
  const loadKey = `${id}:${String(ft).toLowerCase()}`;
  if (loadedForId === loadKey) return;

  cleanup();
  const generation = ++loadGeneration;
  const controller = new AbortController();
  activeController = controller;
  loading.value = true;
  contentRendering.value = false;
  error.value = '';
  previewType.value = resolveDocumentPreviewType(ft);

  if (previewType.value === 'unsupported') {
    loading.value = false;
    activeController = null;
    return;
  }

  try {
    let maxOriginalBytes = DEFAULT_LOADER_BLOB_LIMIT;
    if (!props.blobLoader && props.knowledgeId) {
      const payload = await getKnowledgePreviewPolicy(props.knowledgeId, {
        signal: controller.signal,
      });
      if (generation !== loadGeneration || controller.signal.aborted) return;
      const policy = unwrapKnowledgePreviewPolicy(payload);
      const admission = evaluatePreviewAdmission(policy, {
        fileType: ft,
        fileSize: props.fileSize,
        chunkCount: props.chunkCount,
      });
      maxOriginalBytes = admission.maxOriginalBytes || maxOriginalBytes;
      if (admission.mode !== 'original') {
        boundedPreview.value = true;
        boundedReason.value = admission.reason;
        loadedForId = loadKey;
        return;
      }
    }

    const rawBlob = props.blobLoader
      ? await props.blobLoader(controller.signal)
      : await previewKnowledgeFile(props.knowledgeId!, { signal: controller.signal });
    if (generation !== loadGeneration || controller.signal.aborted) return;
    const blob = ensureDocumentPreviewBlobType(rawBlob, ft);
    if (blobExceedsAdmission(blob, maxOriginalBytes)) {
      boundedPreview.value = true;
      boundedReason.value = 'runtime_size_mismatch';
      loadedForId = loadKey;
      return;
    }
    loadedForId = loadKey;

    // These engines still have an in-browser layout phase after the source
    // finishes downloading. Keep a loading cover visible until their output
    // is ready so mobile WebViews never expose a blank document frame.
    contentRendering.value = ['pdf', 'docx', 'excel', 'text', 'markdown', 'pptx'].includes(previewType.value);
    loading.value = false;
    await nextTick();
    if (generation !== loadGeneration || controller.signal.aborted) return;

    switch (previewType.value) {
      case 'pdf': {
        if (!pdfContainer.value) throw new Error('PDF preview container is unavailable');
        pdfSession = mountPDFCanvasPreview({
          blob,
          container: pdfContainer.value,
          signal: controller.signal,
        });
        await pdfSession.ready;
        contentRendering.value = false;
        break;
      }
      case 'image': {
        blobUrl.value = URL.createObjectURL(blob);
        break;
      }
      case 'docx': {
        await renderDocx(blob);
        contentRendering.value = false;
        break;
      }
      case 'excel': {
        const bounded = String(ft).toLowerCase() === 'csv'
          ? boundedTextBlob(blob)
          : { blob, truncated: false };
        textTruncated.value = bounded.truncated;
        await renderExcel(bounded.blob, ft);
        contentRendering.value = false;
        break;
      }
      case 'text': {
        const bounded = boundedTextBlob(blob);
        textTruncated.value = bounded.truncated;
        await renderText(bounded.blob, ft);
        contentRendering.value = false;
        break;
      }
      case 'markdown': {
        const bounded = boundedTextBlob(blob);
        textTruncated.value = bounded.truncated;
        await renderMarkdown(bounded.blob);
        contentRendering.value = false;
        break;
      }
      case 'pptx': {
        pptxData.value = await blob.arrayBuffer();
        break;
      }
      case 'audio': {
        blobUrl.value = URL.createObjectURL(blob);
        break;
      }
    }
  } catch (err: any) {
    if (generation !== loadGeneration || controller.signal.aborted || err?.name === 'CanceledError') {
      return;
    }
    if (err?.status === 413 || err?.error?.code === 'PREVIEW_REQUIRES_PAGED_CHUNKS') {
      boundedPreview.value = true;
      boundedReason.value = 'server_policy';
      loadedForId = loadKey;
      return;
    }
    console.error('Document preview failed:', err);
    contentRendering.value = false;
    error.value = err?.message || t('preview.loadFailed');
  } finally {
    if (generation === loadGeneration) {
      loading.value = false;
      if (activeController === controller) {
        activeController = null;
      }
    }
  }
}

function cleanup() {
  loadGeneration += 1;
  if (activeController) {
    activeController.abort();
    activeController = null;
  }
  if (blobUrl.value) {
    URL.revokeObjectURL(blobUrl.value);
    blobUrl.value = '';
  }
  textContent.value = '';
  highlightedCode.value = '';
  markdownHtml.value = '';
  excelHtml.value = '';
  pptxData.value = null;
  imageNaturalWidth.value = 0;
  imageNaturalHeight.value = 0;
  boundedPreview.value = false;
  boundedReason.value = '';
  textTruncated.value = false;
  loading.value = false;
  contentRendering.value = false;
  loadedForId = '';
  if (docxContainer.value) {
    docxContainer.value.innerHTML = '';
  }
  pdfSession?.destroy();
  pdfSession = null;
}

function onPptxRendered() {
  contentRendering.value = false;
}

function onPptxError(reason: any) {
  contentRendering.value = false;
  error.value = reason?.message || t('preview.loadFailed');
}

watch(
  () => [props.active, props.knowledgeId, props.sourceKey, props.fileType],
  ([active]) => {
    if (active && (props.knowledgeId || props.blobLoader)) {
      loadPreview();
    } else if (!active) {
      cleanup();
    }
  },
  { immediate: true }
);

onUnmounted(() => {
  document.body.style.overflow = '';
  cleanup();
});
</script>

<template>
  <Teleport to="body" :disabled="!isFullscreen">
  <div
    class="document-preview"
    :class="{ 'is-fullscreen': isFullscreen, 'is-mobile-fit': mobileFit }"
  >
    <!-- Toolbar -->
    <div class="preview-toolbar" v-if="!loading && !contentRendering && !error && !boundedPreview && previewType !== 'unsupported'">
      <button
        type="button"
        class="preview-fullscreen-button"
        :title="isFullscreen ? $t('preview.exitFullscreen') : $t('preview.fullscreen')"
        :aria-label="isFullscreen ? $t('preview.exitFullscreen') : $t('preview.fullscreen')"
        data-testid="document-preview-fullscreen-toggle"
        @click="toggleFullscreen"
      >
        <svg v-if="isFullscreen" viewBox="0 0 24 24" aria-hidden="true">
          <path d="M9 4v5H4M15 4v5h5M9 20v-5H4M15 20v-5h5" />
        </svg>
        <svg v-else viewBox="0 0 24 24" aria-hidden="true">
          <path d="M9 4H4v5M15 4h5v5M9 20H4v-5M15 20h5v-5" />
        </svg>
      </button>
    </div>

    <div v-if="contentRendering" class="preview-rendering-overlay" data-testid="document-preview-rendering">
      <t-loading size="medium" />
      <span class="loading-text">{{ $t('preview.loading') }}</span>
    </div>

    <!-- Loading -->
    <div v-if="loading" class="preview-loading">
      <t-loading size="medium" />
      <span class="loading-text">{{ $t('preview.loading') }}</span>
    </div>

    <!-- Error -->
    <div v-else-if="error" class="preview-error">
      <t-icon name="error-circle" size="48px" />
      <p>{{ error }}</p>
      <t-button theme="primary" size="small" @click="loadedForId = ''; loadPreview()">
        {{ $t('preview.retry') }}
      </t-button>
    </div>

    <!-- Large/structurally-heavy documents never enter browser memory as one Blob. -->
    <div v-else-if="boundedPreview" class="preview-bounded" :data-preview-reason="boundedReason">
      <t-icon name="shield" size="42px" />
      <p class="bounded-title">{{ $t('preview.safePreviewTitle') }}</p>
      <p class="bounded-hint">{{ $t('preview.safePreviewHint') }}</p>
      <t-button theme="primary" size="small" @click="emit('useChunks')">
        {{ $t('preview.viewPagedContent') }}
      </t-button>
    </div>

    <!-- Unsupported -->
    <div v-else-if="previewType === 'unsupported'" class="preview-unsupported">
      <t-icon name="file-unknown" size="48px" />
      <p>{{ $t('preview.unsupported') }}</p>
      <p class="unsupported-hint">{{ $t('preview.unsupportedHint') }}</p>
    </div>

    <!-- PDF -->
    <div v-else-if="previewType === 'pdf'" class="preview-pdf">
      <div ref="pdfContainer" class="pdf-page-list" />
    </div>

    <!-- Image -->
    <div v-else-if="previewType === 'image' && blobUrl" class="preview-image">
      <div class="image-wrapper">
        <img :src="blobUrl" :alt="fileName" @load="onImageLoad" />
        <div v-if="imageNaturalWidth" class="image-info">
          {{ imageNaturalWidth }} × {{ imageNaturalHeight }} px
        </div>
      </div>
    </div>

    <!-- DOCX -->
    <div v-else-if="previewType === 'docx'" class="preview-docx">
      <div ref="docxContainer" class="docx-container" />
    </div>

    <!-- PPTX -->
    <div v-else-if="previewType === 'pptx' && pptxData" class="preview-pptx">
      <vue-office-pptx :src="pptxData" @rendered="onPptxRendered" @error="onPptxError" />
    </div>

    <!-- Excel -->
    <div v-else-if="previewType === 'excel' && excelHtml" class="preview-excel">
      <div v-if="textTruncated" class="preview-truncated">{{ $t('preview.truncatedHint') }}</div>
      <div class="excel-container" v-html="excelHtml" />
    </div>

    <!-- Markdown -->
    <div v-else-if="previewType === 'markdown' && markdownHtml" class="preview-markdown">
      <div v-if="textTruncated" class="preview-truncated">{{ $t('preview.truncatedHint') }}</div>
      <div class="markdown-body" v-html="markdownHtml" />
    </div>

    <!-- Text / Code -->
    <div v-else-if="previewType === 'text' && highlightedCode" class="preview-text">
      <div v-if="textTruncated" class="preview-truncated">{{ $t('preview.truncatedHint') }}</div>
      <pre class="code-preview"><code class="hljs" v-html="highlightedCode"></code></pre>
    </div>

    <!-- Audio -->
    <div v-else-if="previewType === 'audio' && blobUrl" class="preview-audio">
      <div class="audio-wrapper">
        <t-icon name="sound" size="48px" />
        <p class="audio-filename">{{ fileName }}</p>
        <audio controls :src="blobUrl" class="audio-element">
          {{ $t('preview.audioNotSupported') }}
        </audio>
      </div>
    </div>
  </div>
  </Teleport>
</template>

<style scoped lang="less">
// ── Design tokens ──
@border-color: var(--td-component-stroke);
@border-radius: 6px;
@bg-white: var(--td-bg-color-container);
@bg-subtle: var(--td-bg-color-container);
@bg-muted: var(--td-bg-color-secondarycontainer);
@text-primary: var(--td-text-color-primary);
@text-secondary: var(--td-text-color-secondary);
@text-tertiary: var(--td-text-color-placeholder);
@text-disabled: var(--td-text-color-disabled);
@accent: var(--td-brand-color);
@accent-hover: var(--td-brand-color-active);
@accent-bg: var(--td-success-color-light);
@accent-bg-hover: var(--td-success-color-light);
@error-color: var(--td-error-color);
@table-border: var(--td-component-stroke);
@preview-max-h: calc(100vh - 200px);
// Note: <html> carries a `zoom` multiplier for font-size control, so 100vh
// is evaluated against the unscaled viewport and the resulting max-height
// may exceed the real viewport by the zoom factor (≤12.5% at "large").
// That produces an extra bit of scroll inside the non-fullscreen preview,
// which is acceptable for document reading. Not worth the complexity of
// inverse-scaling here.
@transition: all 0.2s ease;

// ── Shared container mixin ──
.preview-container() {
  border: 1px solid @border-color;
  border-radius: @border-radius;
  overflow: auto;
  max-height: @preview-max-h;
  background: @bg-white;
}

.document-preview {
  min-height: 200px;
  position: relative;
}

.preview-rendering-overlay {
  position: absolute;
  z-index: 9;
  inset: 0;
  display: flex;
  min-height: 200px;
  align-items: center;
  justify-content: center;
  flex-direction: column;
  gap: 12px;
  border-radius: @border-radius;
  background: rgb(255 255 255 / 88%);
  color: @text-tertiary;
  backdrop-filter: blur(2px);
}

.is-fullscreen {
  position: fixed;
  top: 0;
  left: 0;
  right: 0;
  bottom: 0;
  z-index: 2001;
  background: var(--td-bg-color-container);
  padding: 0;
  overflow-y: auto;

  .preview-toolbar {
    position: fixed;
    top: 12px;
    right: 32px;
    z-index: 2002;
  }

  /* Children use height: 100% rather than 100vh because <html> carries a
     `zoom` multiplier for font-size control; 100vh resolves against the
     unscaled viewport and then gets scaled, overshooting the screen. The
     fullscreen container is already inset 0 on all sides, so 100% resolves
     to the true viewport height. */
  .preview-pdf {
    height: 100%;
  }

  .preview-pptx {
    height: auto;
    min-height: 100%;
    overflow: visible;
    border: none;

    :deep(.pptx-preview-wrapper) {
      height: auto !important;
      overflow-y: visible !important;
    }
  }

  .preview-docx {
    height: 100%;
    display: flex;
    flex-direction: column;
    .docx-container {
      max-height: 100%;
      height: 100%;
      flex: 1;
    }
  }

  .preview-image {
    min-height: 100%;
    display: flex;
    justify-content: center;
    align-items: center;
    .image-wrapper img {
      max-height: calc(100% - 80px);
    }
  }

  .preview-excel .excel-container,
  .preview-markdown,
  .preview-text .code-preview {
    max-height: 100%;
  }
}

.preview-toolbar {
  position: absolute;
  top: 8px;
  right: 24px;
  z-index: 10;
  background: var(--td-bg-color-container);
  border: 1px solid var(--td-component-border);
  border-radius: var(--td-radius-default);
  box-shadow: var(--td-shadow-1);
  padding: 4px;
  opacity: 0.6;
  transition: opacity 0.2s;

  &:hover {
    opacity: 1;
  }
}

// ── States ──
.preview-loading {
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  padding: 60px 20px;
  gap: 16px;
  .loading-text { color: @text-tertiary; font-size: 14px; }
}

.preview-error {
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  padding: 60px 20px;
  gap: 12px;
  color: @error-color;
  p {
    max-width: 100%;
    margin: 0;
    color: @text-secondary;
    font-size: 14px;
    overflow-wrap: anywhere;
    text-align: center;
    word-break: break-word;
  }
}

.preview-bounded {
  min-height: 240px;
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  gap: 10px;
  padding: 36px 24px;
  border: 1px solid @border-color;
  border-radius: @border-radius;
  background: @bg-subtle;
  color: @accent;

  .bounded-title {
    margin: 2px 0 0;
    color: @text-primary;
    font-size: 15px;
    font-weight: 600;
  }

  .bounded-hint {
    max-width: 520px;
    margin: 0 0 6px;
    color: @text-secondary;
    font-size: 13px;
    line-height: 1.6;
    text-align: center;
  }
}

.preview-fullscreen-button {
  display: grid;
  width: 34px;
  height: 34px;
  place-items: center;
  border: 0;
  border-radius: 5px;
  background: transparent;
  color: @text-primary;
  cursor: pointer;
  padding: 0;

  &:hover { background: @bg-muted; }
  &:focus-visible { outline: 2px solid @accent; outline-offset: 1px; }

  svg {
    width: 21px;
    height: 21px;
    fill: none;
    stroke: currentColor;
    stroke-width: 1.9;
    stroke-linecap: round;
    stroke-linejoin: round;
  }
}

.is-mobile-fit {
  width: 100%;
  min-width: 0;

  .preview-toolbar {
    top: 8px;
    right: 8px;
    opacity: 1;
  }

  &:not(.is-fullscreen) .preview-toolbar {
    position: relative;
    top: auto;
    right: auto;
    width: max-content;
    margin: 8px 8px 8px auto;
  }

  .preview-pdf,
  .preview-pptx {
    min-height: min(68dvh, 620px);
    max-width: 100%;
  }

  .preview-docx .docx-container,
  .preview-excel .excel-container,
  .preview-markdown,
  .preview-text .code-preview {
    max-width: 100%;
    overflow-x: auto;
  }

  :deep(.docx-preview-wrapper-wrapper) {
    width: 100%;
    max-width: 100%;
    align-items: stretch;
    overflow-x: hidden;
    background: #eef1ef;
    padding: 12px;
  }

  :deep(.docx-preview-wrapper-wrapper > section.docx-preview-wrapper) {
    width: 100% !important;
    max-width: 100% !important;
    min-height: 0 !important;
    margin: 0 0 12px !important;
    overflow: hidden;
    padding: 22px 16px !important;
  }

  :deep(.docx-preview-wrapper-wrapper > section.docx-preview-wrapper:last-child) {
    margin-bottom: 0 !important;
  }

  :deep(.docx-preview-wrapper-wrapper article),
  :deep(.docx-preview-wrapper-wrapper p),
  :deep(.docx-preview-wrapper-wrapper span),
  :deep(.docx-preview-wrapper-wrapper table),
  :deep(.docx-preview-wrapper-wrapper td),
  :deep(.docx-preview-wrapper-wrapper th) {
    max-width: 100% !important;
    overflow-wrap: anywhere;
    word-break: break-word;
  }

  :deep(.docx-preview-wrapper-wrapper table) {
    width: 100% !important;
    table-layout: fixed;
  }

  :deep(.docx-preview-wrapper-wrapper col) {
    width: auto !important;
  }

  :deep(.vue-office-pptx),
  :deep(.vue-office-pptx-main),
  :deep(.pptx-preview-wrapper) {
    width: 100% !important;
    max-width: 100% !important;
  }
}

.is-mobile-fit.is-fullscreen {
  width: 100%;
  padding: max(8px, env(safe-area-inset-top)) 0 max(8px, env(safe-area-inset-bottom));

  .preview-toolbar {
    top: max(10px, env(safe-area-inset-top));
    right: 10px;
  }

  :deep(.docx-preview-wrapper-wrapper) { padding-top: 54px; }
}

.preview-truncated {
  position: sticky;
  top: 0;
  z-index: 2;
  padding: 8px 12px;
  border-bottom: 1px solid @border-color;
  background: var(--td-warning-color-light);
  color: @text-secondary;
  font-size: 12px;
}

.preview-unsupported {
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  padding: 60px 20px;
  gap: 12px;
  color: @text-disabled;
  p { margin: 0; font-size: 14px; color: @text-secondary; }
  .unsupported-hint { font-size: 12px; color: @text-tertiary; }
}

// ── PDF ──
.preview-pdf {
  width: 100%;
  height: @preview-max-h;
  min-height: 500px;
  overflow: auto;
  border: 1px solid @border-color;
  border-radius: @border-radius;
  background: #d9dddb;
  overscroll-behavior: contain;
  -webkit-overflow-scrolling: touch;

  .pdf-page-list {
    display: flex;
    min-width: 0;
    flex-direction: column;
    align-items: center;
    gap: 12px;
    padding: 12px;
  }

  :deep(.pdf-page-slot) {
    position: relative;
    display: grid;
    width: 100%;
    place-items: center;
    overflow: hidden;
    background: #fff;
    box-shadow: 0 1px 5px rgb(28 45 36 / 18%);
  }

  :deep(.pdf-page-canvas) {
    display: block;
    max-width: 100%;
    height: auto !important;
    background: #fff;
  }

  :deep(.pdf-page-status) {
    position: absolute;
    z-index: 1;
    top: 10px;
    right: 10px;
    border-radius: 5px;
    background: rgb(32 46 39 / 72%);
    color: #fff;
    font-size: 11px;
    padding: 4px 7px;
  }

  :deep(.pdf-page-slot.is-rendered .pdf-page-status) {
    opacity: 0;
    pointer-events: none;
    transition: opacity .2s;
  }

  :deep(.pdf-page-slot.has-error .pdf-page-status) {
    top: 50%;
    right: auto;
    max-width: calc(100% - 32px);
    background: #a3483b;
    opacity: 1;
    transform: translateY(-50%);
    text-align: center;
  }
}

// ── Image ──
.preview-image {
  display: flex;
  justify-content: center;
  padding: 20px 0;
  .image-wrapper {
    display: flex;
    flex-direction: column;
    align-items: center;
    gap: 8px;
    img {
      max-width: 100%;
      max-height: calc(100vh - 280px);
      border-radius: @border-radius;
      box-shadow: 0 2px 12px rgba(7, 192, 95, 0.08);
      object-fit: contain;
    }
    .image-info { font-size: 12px; color: @text-tertiary; }
  }
}

// ── Markdown ──
.preview-markdown {
  .preview-container();
  padding: 20px 24px;
}

// ── DOCX ──
.preview-docx {
  .docx-container { .preview-container(); }
}

// ── PPTX ──
.preview-pptx {
  max-height: @preview-max-h;
  min-height: 500px;
  border: 1px solid @border-color;
  border-radius: @border-radius;
  overflow: auto;
  background: @bg-subtle;

  :deep(.pptx-preview-wrapper) {
    height: auto !important;
    overflow-y: visible !important;
  }
}

// ── Excel ──
.preview-excel {
  .excel-container { .preview-container(); }
}

// ── Text / Code ──
.preview-text {
  .code-preview {
    .preview-container();
    margin: 0;
    padding: 16px;
    background: @bg-subtle;
    font-size: 13px;
    line-height: 1.6;
    code {
      white-space: pre;
      word-wrap: normal;
      display: block;
      background: transparent;
    }
  }
}

// ── Audio ──
.preview-audio {
  display: flex;
  justify-content: center;
  padding: 40px 20px;
  .audio-wrapper {
    display: flex;
    flex-direction: column;
    align-items: center;
    gap: 16px;
    color: @text-secondary;
    .audio-filename { font-size: 14px; color: @text-primary; margin: 0; }
    .audio-element { width: 100%; max-width: 480px; }
  }
}

// ── Deep styles (v-html / third-party components) ──

// Shared table mixin for v-html content
.preview-table() {
  width: 100%;
  border-collapse: collapse;
  font-size: 13px;
  th, td {
    border: 1px solid @table-border;
    padding: 6px 12px;
    text-align: left;
  }
  th {
    background: @accent-bg;
    font-weight: 600;
    color: @text-primary;
  }
  tr:hover td {
    background: @accent-bg;
    transition: @transition;
  }
}

:deep(.markdown-body) {
  font-size: 14px;
  line-height: 1.7;
  color: @text-primary;
  word-break: break-word;

  h1, h2, h3, h4, h5, h6 {
    margin-top: 20px;
    margin-bottom: 10px;
    font-weight: 600;
    line-height: 1.4;
  }
  h1 { font-size: 24px; border-bottom: 1px solid @border-color; padding-bottom: 8px; }
  h2 { font-size: 20px; border-bottom: 1px solid @border-color; padding-bottom: 6px; }
  h3 { font-size: 17px; }

  p { margin: 8px 0; }
  blockquote {
    margin: 12px 0;
    padding: 8px 16px;
    border-left: 4px solid @accent;
    background: @bg-subtle;
    color: var(--td-text-color-secondary);
  }
  ul, ol { padding-left: 24px; margin: 8px 0; }
  li { margin: 4px 0; }

  table { .preview-table(); margin: 12px 0; }

  pre {
    margin: 12px 0;
    padding: 14px;
    background: @bg-subtle;
    border-radius: @border-radius;
    overflow: auto;
    font-size: 13px;
    line-height: 1.5;
    code { background: transparent; padding: 0; }
  }
  code {
    background: var(--td-bg-color-secondarycontainer);
    padding: 2px 6px;
    border-radius: 3px;
    font-size: 0.9em;
  }
  img { max-width: 100%; border-radius: 4px; }
  hr { border: none; border-top: 1px solid @border-color; margin: 20px 0; }
  a { color: @accent; text-decoration: none; &:hover { color: @accent-hover; text-decoration: underline; } }
  strong { font-weight: 600; }
}

:deep(.docx-preview-wrapper) {
  padding: 20px;
  max-width: 100%;
  width: 100%;
  box-sizing: border-box;
  overflow-x: auto; // 如果内容过宽，允许水平滚动而不是溢出
  
  // 约束所有子元素的宽度
  * {
    max-width: 100%;
    box-sizing: border-box;
  }
  
  // 特别处理表格
  table {
    width: 100%;
    table-layout: auto;
    word-wrap: break-word;
  }
  
  // 处理图片
  img {
    max-width: 100%;
    height: auto;
  }
  
  // 处理可能的固定宽度元素
  [style*="width"] {
    max-width: 100% !important;
  }
}

:deep(.vue-office-pptx) {
  width: 100%;
  min-height: 100%;
}

:deep(.vue-office-pptx-main) {
  width: 100%;
  min-height: 100%;
}

:deep(.excel-sheet) {
  padding: 0;
  .excel-sheet-name {
    position: sticky;
    top: 0;
    background: @accent-bg;
    padding: 8px 16px;
    font-weight: 600;
    font-size: 13px;
    color: @text-primary;
    border-bottom: 1px solid @border-color;
    z-index: 1;
  }
  table {
    .preview-table();
    th, td {
      white-space: nowrap;
      max-width: 300px;
      overflow: hidden;
      text-overflow: ellipsis;
    }
  }
}
</style>

<!-- highlight.js github.css is a light theme imported globally; its token
     colors (notably the base #24292e) become unreadable on the dark
     container background used in dark mode. Remap the palette to the
     github-dark colors when dark mode is active. Non-scoped on purpose so
     it covers every hljs block (txt/code preview, markdown code fences). -->
<style lang="less">
html[theme-mode="dark"] {
  .hljs {
    color: #c9d1d9;
    background: transparent;
  }
  .hljs-doctag,
  .hljs-keyword,
  .hljs-meta .hljs-keyword,
  .hljs-template-tag,
  .hljs-template-variable,
  .hljs-type,
  .hljs-variable.language_ {
    color: #ff7b72;
  }
  .hljs-title,
  .hljs-title.class_,
  .hljs-title.class_.inherited__,
  .hljs-title.function_ {
    color: #d2a8ff;
  }
  .hljs-attr,
  .hljs-attribute,
  .hljs-literal,
  .hljs-meta,
  .hljs-number,
  .hljs-operator,
  .hljs-variable,
  .hljs-selector-attr,
  .hljs-selector-class,
  .hljs-selector-id {
    color: #79c0ff;
  }
  .hljs-regexp,
  .hljs-string,
  .hljs-meta .hljs-string {
    color: #a5d6ff;
  }
  .hljs-built_in,
  .hljs-symbol {
    color: #ffa657;
  }
  .hljs-comment,
  .hljs-code,
  .hljs-formula {
    color: #8b949e;
  }
  .hljs-name,
  .hljs-quote,
  .hljs-selector-tag,
  .hljs-selector-pseudo {
    color: #7ee787;
  }
  .hljs-subst {
    color: #c9d1d9;
  }
  .hljs-section {
    color: #1f6feb;
    font-weight: bold;
  }
  .hljs-bullet {
    color: #f2cc60;
  }
  .hljs-emphasis {
    color: #c9d1d9;
    font-style: italic;
  }
  .hljs-strong {
    color: #c9d1d9;
    font-weight: bold;
  }
  .hljs-addition {
    color: #aff5b4;
    background-color: #033a16;
  }
  .hljs-deletion {
    color: #ffdcd7;
    background-color: #67060c;
  }
}
</style>
