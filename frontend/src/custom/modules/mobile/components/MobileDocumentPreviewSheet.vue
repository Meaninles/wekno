<script setup lang="ts">
import {
  computed,
  nextTick,
  onBeforeUnmount,
  ref,
  watch,
} from "vue";
import { MessagePlugin } from "tdesign-vue-next";
import {
  getKnowledgeDetails,
  getKnowledgeDetailsCon,
} from "@/api/knowledge-base";
import DocumentPreview from "@/components/document-preview.vue";
import {
  DOCUMENT_CHUNK_PAGE_SIZE,
  documentChunkPageCount,
} from "@/custom/modules/documentPreview/chunkPaging";
import {
  isDocumentPreviewSupported,
  normalizePreviewFileType,
} from "@/utils/documentPreview";
import { hydrateProtectedFileImages } from "@/utils/security";
import { downloadKnowledgeNatively } from "../documentDownload";
import { formatFileSize } from "../utils";
import { renderMobileMarkdown } from "../mobileMarkdown";
import MobileIcon from "./MobileIcon.vue";

const props = defineProps<{
  item: Record<string, any> | null;
  sourceKey?: string;
  blobLoader?: (signal?: AbortSignal) => Promise<Blob>;
  downloadHandler?: () => Promise<void>;
}>();

const emit = defineEmits<{
  close: [];
}>();

const detail = ref<Record<string, any>>({});
const viewMode = ref<"original" | "chunks">("original");
const detailLoading = ref(false);
const chunks = ref<Record<string, any>[]>([]);
const chunkTotal = ref(0);
const chunkPage = ref(1);
const chunkLoading = ref(false);
const chunkError = ref("");
const downloading = ref(false);
const chunkRoot = ref<HTMLElement | null>(null);
let generation = 0;
let bodyLocked = false;
let previousBodyOverflow = "";
let chunkImageObserver: IntersectionObserver | null = null;

const externalSource = computed(() => typeof props.blobLoader === "function");
const knowledgeID = computed(() => externalSource.value
  ? ""
  : String(detail.value?.id || props.item?.id || props.item?.knowledge_id || "").trim(),
);
const previewSourceKey = computed(() =>
  String(props.sourceKey || knowledgeID.value).trim(),
);
const chunkPreviewAvailable = computed(() => !externalSource.value && !!knowledgeID.value);
const downloadAvailable = computed(() =>
  typeof props.downloadHandler === "function" || !!knowledgeID.value,
);

const fileName = computed(() =>
  String(
    detail.value?.original_file_name ||
    detail.value?.file_name ||
    detail.value?.filename ||
    detail.value?.display_name ||
    detail.value?.title ||
    props.item?.original_file_name ||
    props.item?.file_name ||
    props.item?.filename ||
    props.item?.display_name ||
    props.item?.title ||
    "知识库文档",
  ).trim(),
);

const fileType = computed(() => {
  const explicit = normalizePreviewFileType(
    detail.value?.file_type || props.item?.file_type,
  );
  if (explicit) return explicit;
  const name = fileName.value;
  const dot = name.lastIndexOf(".");
  return dot >= 0 ? normalizePreviewFileType(name.slice(dot + 1)) : "";
});

const fileSize = computed(() =>
  Number(detail.value?.file_size || props.item?.file_size || 0),
);
const tenantID = computed(() =>
  String(detail.value?.tenant_id || props.item?.tenant_id || "").trim(),
);
const parseStatus = computed(() =>
  String(detail.value?.parse_status || props.item?.parse_status || ""),
);
const previewSupported = computed(() => isDocumentPreviewSupported(fileType.value));
const chunkPages = computed(() =>
  documentChunkPageCount(chunkTotal.value, DOCUMENT_CHUNK_PAGE_SIZE),
);
const renderedChunks = computed(() =>
  chunks.value.map((chunk, index) => ({
    id: String(chunk.id || `${chunkPage.value}-${index}`),
    index: (chunkPage.value - 1) * DOCUMENT_CHUNK_PAGE_SIZE + index + 1,
    html: renderMobileMarkdown(String(chunk.content || "")),
    meta: [
      chunk.char_count ? `${chunk.char_count} 字符` : "",
      chunk.token_count ? `${chunk.token_count} tokens` : "",
    ].filter(Boolean).join(" · "),
  })),
);

function lockBody() {
  if (bodyLocked || typeof document === "undefined") return;
  previousBodyOverflow = document.body.style.overflow;
  document.body.style.overflow = "hidden";
  bodyLocked = true;
}

function unlockBody() {
  if (!bodyLocked || typeof document === "undefined") return;
  document.body.style.overflow = previousBodyOverflow;
  bodyLocked = false;
}

async function hydrateChunks() {
  await nextTick();
  chunkImageObserver?.disconnect();
  chunkImageObserver = null;
  const root = chunkRoot.value;
  if (!root) return;
  const cards = Array.from(root.querySelectorAll<HTMLElement>("article"));
  if (!cards.length) return;

  const hydrateCard = (card: HTMLElement) => {
    void hydrateProtectedFileImages(card, undefined, tenantID.value);
  };
  if (typeof IntersectionObserver === "undefined") {
    for (const card of cards) await hydrateProtectedFileImages(card, undefined, tenantID.value);
    return;
  }

  const scrollRoot = root.closest<HTMLElement>(".mobile-document-preview__body");
  chunkImageObserver = new IntersectionObserver(
    (entries, observer) => {
      for (const entry of entries) {
        if (!entry.isIntersecting) continue;
        observer.unobserve(entry.target);
        hydrateCard(entry.target as HTMLElement);
      }
    },
    { root: scrollRoot, rootMargin: "100% 0px", threshold: 0.01 },
  );
  cards.forEach((card) => chunkImageObserver?.observe(card));
}

async function loadChunks(page = 1) {
  const id = knowledgeID.value;
  if (!id || chunkLoading.value) return;
  const requestGeneration = generation;
  chunkLoading.value = true;
  chunkError.value = "";
  try {
    const response: any = await getKnowledgeDetailsCon(id, page);
    if (requestGeneration !== generation) return;
    chunks.value = Array.isArray(response?.data) ? response.data : [];
    chunkTotal.value = Number(response?.total || chunks.value.length);
    chunkPage.value = Number(response?.page || page);
  } catch (error: any) {
    if (requestGeneration !== generation) return;
    chunkError.value = error?.message || "解析内容加载失败";
  } finally {
    if (requestGeneration === generation) chunkLoading.value = false;
  }
}

async function showChunks() {
  viewMode.value = "chunks";
  if (!chunks.value.length && !chunkLoading.value) {
    await loadChunks(1);
  }
}

async function showChunkPage(page: number) {
  if (page < 1 || (chunkPages.value > 0 && page > chunkPages.value)) return;
  await loadChunks(page);
  chunkRoot.value?.scrollTo({ top: 0, behavior: "auto" });
}

async function downloadDocument() {
  if (!downloadAvailable.value || downloading.value) return;
  downloading.value = true;
  try {
    if (props.downloadHandler) {
      await props.downloadHandler();
    } else {
      await downloadKnowledgeNatively(knowledgeID.value);
    }
  } catch (error: any) {
    MessagePlugin.error(error?.message || "下载失败");
    downloading.value = false;
  }
}

watch(
  () => props.item,
  async (item) => {
    const requestGeneration = ++generation;
    detail.value = item ? { ...item } : {};
    chunks.value = [];
    chunkImageObserver?.disconnect();
    chunkImageObserver = null;
    chunkTotal.value = 0;
    chunkPage.value = 1;
    chunkError.value = "";
    chunkLoading.value = false;
    detailLoading.value = false;
    downloading.value = false;
    if (!item) {
      unlockBody();
      return;
    }
    lockBody();
    viewMode.value = previewSupported.value || !chunkPreviewAvailable.value ? "original" : "chunks";
    if (viewMode.value === "chunks" && chunkPreviewAvailable.value) void loadChunks(1);

    if (externalSource.value) return;

    const id = knowledgeID.value;
    if (!id) return;
    detailLoading.value = true;
    try {
      const response: any = await getKnowledgeDetails(id);
      if (requestGeneration !== generation) return;
      detail.value = {
        ...item,
        ...(response?.data || response || {}),
      };
      if (!previewSupported.value && chunkPreviewAvailable.value) {
        viewMode.value = "chunks";
        if (!chunks.value.length) void loadChunks(1);
      }
    } catch {
      // List data is sufficient for preview. Keep the sheet usable if the
      // optional metadata refresh races with a document update.
    } finally {
      if (requestGeneration === generation) detailLoading.value = false;
    }
  },
  { immediate: true },
);

watch(
  [renderedChunks, chunkLoading, viewMode],
  ([items, loading, mode]) => {
    if (mode === "chunks" && !loading && items.length) void hydrateChunks();
  },
  { flush: "post" },
);

onBeforeUnmount(() => {
  chunkImageObserver?.disconnect();
  chunkImageObserver = null;
  generation += 1;
  unlockBody();
});
</script>

<template>
  <Teleport to="body">
    <div v-if="props.item" class="mobile-document-preview" data-testid="mobile-document-preview">
      <header class="mobile-document-preview__topbar">
        <button type="button" aria-label="关闭文档预览" @click="emit('close')">
          <MobileIcon name="chevron-left" />
        </button>
        <div>
          <strong>{{ fileName }}</strong>
          <small>
            {{ fileType ? fileType.toUpperCase() : '文档' }}
            <template v-if="fileSize"> · {{ formatFileSize(fileSize) }}</template>
            <template v-if="detailLoading"> · 正在更新信息</template>
          </small>
        </div>
        <button
          type="button"
          class="download-button"
          :class="{ loading: downloading }"
          :disabled="downloading || !downloadAvailable"
          aria-label="下载文档"
          @click="downloadDocument"
        >
          <MobileIcon name="download" />
        </button>
      </header>

      <nav
        v-if="externalSource || previewSupported || chunkPreviewAvailable"
        class="mobile-document-preview__tabs"
        aria-label="文档查看方式"
      >
        <button
          v-if="previewSupported || externalSource"
          type="button"
          :class="{ active: viewMode === 'original' }"
          @click="viewMode = 'original'"
        >
          原文预览
        </button>
        <button
          v-if="chunkPreviewAvailable"
          type="button"
          :class="{ active: viewMode === 'chunks' }"
          @click="showChunks"
        >
          解析内容
          <span v-if="chunkTotal">{{ chunkTotal }}</span>
        </button>
      </nav>

      <main class="mobile-document-preview__body">
        <section v-if="viewMode === 'original'" class="mobile-document-preview__original">
          <DocumentPreview
            :knowledge-id="externalSource ? undefined : knowledgeID"
            :source-key="previewSourceKey"
            :file-type="fileType"
            :file-name="fileName"
            :file-size="fileSize"
            :chunk-count="chunkTotal"
            :parse-status="parseStatus"
            :active="viewMode === 'original'"
            :blob-loader="blobLoader"
            :mobile-fit="true"
            @use-chunks="showChunks"
          />
        </section>

        <section v-else class="mobile-document-preview__chunks">
          <div v-if="chunkLoading" class="preview-state">正在加载解析内容</div>
          <div v-else-if="chunkError" class="preview-state error">
            <span>{{ chunkError }}</span>
            <button type="button" @click="loadChunks(chunkPage)">重试</button>
          </div>
          <div v-else-if="!renderedChunks.length" class="preview-state">
            暂无可预览的解析内容，可下载原文档查看。
          </div>
          <div v-else ref="chunkRoot" class="mobile-document-preview__chunk-list">
            <article v-for="chunk in renderedChunks" :key="chunk.id">
              <header>
                <strong>片段 {{ chunk.index }}</strong>
                <small v-if="chunk.meta">{{ chunk.meta }}</small>
              </header>
              <div class="mobile-rich-content" v-html="chunk.html" />
            </article>
          </div>
          <footer v-if="chunkPages > 1" class="mobile-document-preview__pagination">
            <button
              type="button"
              :disabled="chunkLoading || chunkPage <= 1"
              @click="showChunkPage(chunkPage - 1)"
            >
              上一页
            </button>
            <span>{{ chunkPage }} / {{ chunkPages }}</span>
            <button
              type="button"
              :disabled="chunkLoading || chunkPage >= chunkPages"
              @click="showChunkPage(chunkPage + 1)"
            >
              下一页
            </button>
          </footer>
        </section>
      </main>
    </div>
  </Teleport>
</template>

<style scoped>
.mobile-document-preview {
  position: fixed;
  z-index: 120;
  inset: 0;
  display: grid;
  grid-template-rows: auto auto minmax(0, 1fr);
  background: #f5f7f8;
  color: #17251f;
}

.mobile-document-preview__topbar {
  display: grid;
  grid-template-columns: 40px minmax(0, 1fr) 40px;
  align-items: center;
  gap: 8px;
  border-bottom: 1px solid #e3ebe7;
  background: #fff;
  padding: calc(env(safe-area-inset-top) + 8px) 10px 8px;
}

.mobile-document-preview__topbar > button {
  display: grid;
  width: 38px;
  height: 38px;
  place-items: center;
  border: 0;
  border-radius: 10px;
  background: transparent;
  color: #263830;
  font-size: 21px;
}

.mobile-document-preview__topbar > div {
  display: flex;
  min-width: 0;
  flex-direction: column;
  gap: 2px;
}

.mobile-document-preview__topbar strong,
.mobile-document-preview__topbar small {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.mobile-document-preview__topbar strong {
  font-size: 16px;
  font-weight: 650;
}

.mobile-document-preview__topbar small {
  color: #74857c;
  font-size: 12px;
}

.mobile-document-preview__topbar .download-button {
  color: #078f49;
}

.mobile-document-preview__topbar .download-button:disabled {
  opacity: 0.48;
}

.mobile-document-preview__topbar .download-button.loading {
  animation: mobileDocumentDownloadPulse 0.8s ease-in-out infinite;
}

.mobile-document-preview__tabs {
  display: flex;
  gap: 4px;
  border-bottom: 1px solid #e4ece8;
  background: #fff;
  padding: 7px 12px;
}

.mobile-document-preview__tabs button {
  display: inline-flex;
  min-height: 34px;
  align-items: center;
  gap: 5px;
  border: 0;
  border-radius: 8px;
  background: transparent;
  color: #6a7b72;
  padding: 0 13px;
  font-size: 14px;
}

.mobile-document-preview__tabs button.active {
  background: #e9f8ef;
  color: #078f49;
  font-weight: 650;
}

.mobile-document-preview__tabs span {
  border-radius: 999px;
  background: rgb(7 143 73 / 10%);
  padding: 1px 5px;
  font-size: 11px;
}

.mobile-document-preview__body {
  min-width: 0;
  min-height: 0;
  overflow-y: auto;
  padding: 10px 10px calc(env(safe-area-inset-bottom) + 18px);
  -webkit-overflow-scrolling: touch;
}

.mobile-document-preview__original {
  min-width: 0;
  min-height: 100%;
  border: 1px solid #e0e9e4;
  border-radius: 10px;
  background: #fff;
  overflow: hidden;
}

.mobile-document-preview__original :deep(.preview-toolbar) {
  display: none;
}

.mobile-document-preview__original :deep(.document-preview) {
  min-height: calc(100dvh - 150px);
}

.mobile-document-preview__original :deep(.preview-pdf),
.mobile-document-preview__original :deep(.preview-pptx) {
  height: calc(100dvh - 150px);
  min-height: 420px;
  max-height: none;
  border: 0;
}

.mobile-document-preview__original :deep(.preview-docx .docx-container),
.mobile-document-preview__original :deep(.preview-excel .excel-container),
.mobile-document-preview__original :deep(.preview-markdown),
.mobile-document-preview__original :deep(.preview-text .code-preview) {
  max-height: none;
  border: 0;
  border-radius: 0;
}

.mobile-document-preview__original :deep(.docx-preview-wrapper) {
  padding: 8px;
}

.mobile-document-preview__chunks {
  min-height: 100%;
}

.preview-state {
  display: flex;
  min-height: 180px;
  align-items: center;
  justify-content: center;
  flex-direction: column;
  gap: 12px;
  border: 1px solid #e0e9e4;
  border-radius: 10px;
  background: #fff;
  color: #708179;
  padding: 24px;
  text-align: center;
}

.preview-state.error {
  color: #bd4b38;
}

.preview-state button {
  border: 0;
  border-radius: 8px;
  background: #07b859;
  color: #fff;
  padding: 7px 16px;
}

.mobile-document-preview__chunk-list {
  display: flex;
  flex-direction: column;
  gap: 10px;
}

.mobile-document-preview__chunk-list article {
  overflow: hidden;
  border: 1px solid #e0e9e4;
  border-radius: 10px;
  background: #fff;
}

.mobile-document-preview__chunk-list article > header {
  display: flex;
  min-height: 38px;
  align-items: center;
  justify-content: space-between;
  gap: 10px;
  border-bottom: 1px solid #edf1ef;
  background: #f8fbf9;
  padding: 7px 12px;
}

.mobile-document-preview__chunk-list header strong {
  color: #4b5f55;
  font-size: 13px;
}

.mobile-document-preview__chunk-list header small {
  color: #84948c;
  font-size: 11px;
}

.mobile-rich-content {
  color: #1f2d27;
  font-size: 16px;
  line-height: 1.9;
  overflow-wrap: anywhere;
  padding: 13px 14px 16px;
}

.mobile-rich-content :deep(p) {
  margin: 0 0 13px;
}

.mobile-rich-content :deep(p:last-child) {
  margin-bottom: 0;
}

.mobile-rich-content :deep(img) {
  max-width: 100%;
  height: auto;
}

.mobile-rich-content :deep(img[data-img-loading="1"]) {
  display: block;
  width: 100%;
  min-height: min(68dvh, 620px);
  border-radius: 6px;
  background: linear-gradient(100deg, #f1f4f2 20%, #fafcfb 38%, #f1f4f2 56%);
  background-size: 200% 100%;
  animation: mobileDocumentImageLoading 1.25s ease-in-out infinite;
}

@keyframes mobileDocumentImageLoading {
  from { background-position: 100% 0; }
  to { background-position: -100% 0; }
}

.mobile-rich-content :deep(table) {
  display: block;
  width: 100%;
  overflow-x: auto;
  border-collapse: collapse;
}

.mobile-rich-content :deep(th),
.mobile-rich-content :deep(td) {
  border: 1px solid #e1e9e5;
  padding: 7px;
  white-space: nowrap;
}

.mobile-rich-content :deep(pre) {
  max-width: 100%;
  overflow-x: auto;
}

.mobile-document-preview__pagination {
  position: sticky;
  bottom: 0;
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  margin-top: 10px;
  border: 1px solid #dce7e1;
  border-radius: 10px;
  background: rgb(255 255 255 / 96%);
  padding: 8px 10px;
  color: #71827a;
  font-size: 13px;
}

.mobile-document-preview__pagination button {
  min-width: 76px;
  height: 34px;
  border: 1px solid #bfe4ce;
  border-radius: 8px;
  background: #fff;
  color: #078f49;
}

.mobile-document-preview__pagination button:disabled {
  border-color: #dfe7e3;
  color: #a4b0aa;
}

@keyframes mobileDocumentDownloadPulse {
  0%,
  100% {
    opacity: 0.52;
    transform: translateY(-2px);
  }
  50% {
    opacity: 1;
    transform: translateY(2px);
  }
}
</style>
