<script setup lang="ts">
import { computed, ref, watch } from "vue";
import { MessagePlugin } from "tdesign-vue-next";
import { downKnowledgeDetails, getKnowledgeDetails } from "@/api/knowledge-base";
import { getWikiPage, type WikiPage } from "@/api/wiki";
import {
  hostFromUrl,
  sourceTypeLabel,
  type SourceReferenceItem,
} from "@/utils/sourceReferences";
import { downloadBlob } from "../utils";
import { renderMobileMarkdown } from "../mobileMarkdown";

const props = defineProps<{
  item: SourceReferenceItem | null;
}>();

const emit = defineEmits<{
  close: [];
}>();

const loading = ref(false);
const error = ref("");
const knowledgeDetail = ref<Record<string, any> | null>(null);
const wikiPage = ref<WikiPage | null>(null);
const wikiStack = ref<WikiPage[]>([]);
const downloading = ref(false);

const isKnowledge = computed(() => props.item?.type === "knowledge");
const isWiki = computed(() => props.item?.type === "wiki");
const isWeb = computed(() => props.item?.type === "web");

const displayTitle = computed(() => {
  if (isWiki.value) return wikiPage.value?.title || props.item?.title || "Wiki";
  if (isKnowledge.value) {
    return knowledgeDetail.value?.file_name ||
      knowledgeDetail.value?.title ||
      props.item?.title ||
      "知识库文档";
  }
  return props.item?.title || sourceTypeLabel(props.item?.type || "knowledge");
});

const displayFileType = computed(() => {
  const explicit = knowledgeDetail.value?.file_type || knowledgeDetail.value?.type || "";
  if (explicit) return String(explicit).replace(/^\./, "").toUpperCase();
  const title = displayTitle.value;
  const ext = title.includes(".") ? title.split(".").pop() : "";
  return ext ? String(ext).toUpperCase() : "文档";
});

const displayTime = computed(() =>
  formatTime(knowledgeDetail.value?.created_at || knowledgeDetail.value?.updated_at || knowledgeDetail.value?.time),
);

const knowledgeSummary = computed(() =>
  String(knowledgeDetail.value?.description || props.item?.snippet || "").trim(),
);

const wikiHtml = computed(() => {
  const page = wikiPage.value;
  if (!page) return "";
  return renderMobileMarkdown(page.content || page.summary || "");
});

const sourceLabel = computed(() => {
  if (!props.item) return "";
  const typeText = sourceTypeLabel(props.item.type);
  const label = props.item.type === "web" ? hostFromUrl(props.item.url) || props.item.sourceLabel || "" : props.item.sourceLabel || "";
  return label && label !== typeText ? label : "";
});

const canDownloadKnowledge = computed(() =>
  isKnowledge.value && Boolean(knowledgeDetail.value?.id || props.item?.knowledgeId),
);

watch(
  () => props.item,
  (item) => {
    knowledgeDetail.value = null;
    wikiPage.value = null;
    wikiStack.value = [];
    error.value = "";
    if (!item) return;
    if (item.type === "knowledge") {
      void loadKnowledge(item);
    } else if (item.type === "wiki") {
      void loadWiki(item.knowledgeBaseId, item.slug, false);
    }
  },
  { immediate: true },
);

async function loadKnowledge(item: SourceReferenceItem) {
  if (!item.knowledgeId) {
    error.value = item.snippet ? "" : "这个引用没有关联到可打开的文档";
    return;
  }
  loading.value = true;
  error.value = "";
  try {
    const res: any = await getKnowledgeDetails(item.knowledgeId);
    knowledgeDetail.value = res?.data || res || null;
  } catch (err: any) {
    console.error("[mobile] load citation knowledge failed", err);
    error.value = err?.message || "文档信息加载失败";
  } finally {
    loading.value = false;
  }
}

async function loadWiki(kbId: string, slug: string, pushCurrent: boolean) {
  if (!kbId || !slug) {
    error.value = "这个引用没有关联到可打开的 Wiki 页面";
    return;
  }
  loading.value = true;
  error.value = "";
  const previous = wikiPage.value;
  try {
    const res: any = await getWikiPage(kbId, slug);
    const nextPage = (res?.data || res) as WikiPage;
    if (pushCurrent && previous) {
      wikiStack.value = [...wikiStack.value, previous];
    }
    wikiPage.value = nextPage;
  } catch (err: any) {
    console.error("[mobile] load citation wiki failed", err);
    error.value = err?.message || "Wiki 页面加载失败";
  } finally {
    loading.value = false;
  }
}

function backWikiPage() {
  const stack = [...wikiStack.value];
  const previous = stack.pop();
  if (!previous) return;
  wikiStack.value = stack;
  wikiPage.value = previous;
}

function handleWikiBodyClick(event: MouseEvent) {
  const target = event.target as HTMLElement;
  const link = target.closest?.(".citation-wiki, .wiki-content-link") as HTMLElement | null;
  if (!link || !props.item?.knowledgeBaseId) return;
  const slug = link.getAttribute("data-slug") || "";
  if (!slug) return;
  event.preventDefault();
  event.stopPropagation();
  void loadWiki(props.item.knowledgeBaseId, slug, true);
}

function openWeb() {
  const url = props.item?.url || "";
  if (!url) return;
  const anchor = document.createElement("a");
  anchor.href = url;
  anchor.target = "_blank";
  anchor.rel = "noopener noreferrer";
  anchor.style.display = "none";
  document.body.appendChild(anchor);
  anchor.click();
  anchor.remove();
}

async function downloadKnowledge() {
  const id = knowledgeDetail.value?.id || props.item?.knowledgeId || "";
  if (!id || downloading.value) return;
  downloading.value = true;
  try {
    const result = await downKnowledgeDetails(id);
    const blob = result instanceof Blob ? result : new Blob([result as any]);
    downloadBlob(blob, displayTitle.value || "知识库文档");
  } catch (err: any) {
    MessagePlugin.error(err?.message || "下载失败");
  } finally {
    downloading.value = false;
  }
}

function formatTime(value?: string) {
  if (!value) return "";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return value;
  return date.toLocaleString("zh-CN", {
    year: "numeric",
    month: "2-digit",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
    hour12: false,
  });
}

function pageTypeLabel(type?: string) {
  const map: Record<string, string> = {
    summary: "摘要",
    entity: "实体",
    concept: "概念",
    synthesis: "综合",
    comparison: "对比",
    index: "索引",
    log: "记录",
  };
  return type ? map[type] || type : "Wiki";
}
</script>

<template>
  <div v-if="props.item" class="source-detail-layer">
    <section class="source-detail" role="dialog" aria-modal="true">
      <header class="source-detail__topbar">
        <button
          v-if="isWiki && wikiStack.length"
          type="button"
          class="source-detail__icon-button"
          aria-label="返回上一页"
          @click="backWikiPage"
        >
          <MobileIcon name="chevron-left" />
        </button>
        <span v-else class="source-detail__leading-icon">
          <MobileIcon :name="isWiki ? 'bookmark' : isWeb ? 'internet' : 'file'" />
        </span>
        <strong>{{ displayTitle }}</strong>
        <button
          v-if="canDownloadKnowledge"
          type="button"
          class="source-detail__icon-button"
          :class="{ loading: downloading }"
          aria-label="下载文档"
          @click="downloadKnowledge"
        >
          <MobileIcon name="download" />
        </button>
        <button
          type="button"
          class="source-detail__icon-button source-detail__close-button"
          aria-label="关闭"
          @click="emit('close')"
        >
          <MobileIcon name="close" />
        </button>
      </header>

      <main class="source-detail__body">
        <div v-if="loading" class="source-detail__state">正在加载来源信息</div>
        <div v-else-if="error" class="source-detail__state error">{{ error }}</div>

        <template v-else-if="isWiki">
          <div class="wiki-meta">
            <span>{{ pageTypeLabel(wikiPage?.page_type) }}</span>
            <em v-if="wikiPage?.version">v{{ wikiPage.version }}</em>
            <small v-if="sourceLabel">{{ sourceLabel }}</small>
          </div>
          <h1 class="wiki-title">{{ wikiPage?.title || displayTitle }}</h1>
          <p v-if="wikiPage?.summary" class="wiki-summary">{{ wikiPage.summary }}</p>
          <div class="wiki-content mobile-rich-content" v-html="wikiHtml" @click="handleWikiBodyClick" />
        </template>

        <template v-else-if="isKnowledge">
          <section class="detail-section">
            <h2>基本信息</h2>
            <div class="detail-grid">
              <span>上传时间</span>
              <strong>{{ displayTime || '未知' }}</strong>
              <span>类型</span>
              <strong><em class="file-type-chip">{{ displayFileType }}</em></strong>
              <span v-if="sourceLabel">来源</span>
              <strong v-if="sourceLabel">{{ sourceLabel }}</strong>
            </div>
          </section>

          <section class="detail-section">
            <h2>摘要</h2>
            <div class="summary-card">
              <p v-if="knowledgeSummary">{{ knowledgeSummary }}</p>
              <p v-else>暂无摘要。可以下载原文档查看完整内容。</p>
            </div>
          </section>
        </template>

        <template v-else-if="isWeb">
          <section class="detail-section">
            <h2>网页来源</h2>
            <div class="summary-card">
              <p v-if="props.item?.snippet">{{ props.item.snippet }}</p>
              <p v-else>{{ props.item?.url }}</p>
            </div>
            <button type="button" class="open-web-button" @click="openWeb">打开网页</button>
          </section>
        </template>

        <template v-else>
          <section class="detail-section">
            <h2>{{ sourceTypeLabel(props.item.type) }}</h2>
            <div class="summary-card">
              <p>{{ props.item.snippet || '移动端暂不支持查看这个来源的详情。' }}</p>
            </div>
          </section>
        </template>
      </main>
    </section>
  </div>
</template>

<style scoped>
.source-detail-layer {
  position: fixed;
  z-index: 80;
  inset: 0;
  background: #fff;
}

.source-detail {
  display: grid;
  width: 100%;
  height: 100dvh;
  grid-template-rows: auto minmax(0, 1fr);
  background: #fff;
}

.source-detail__topbar {
  display: grid;
  grid-template-columns: 38px minmax(0, 1fr) 38px 38px;
  align-items: center;
  gap: 6px;
  padding: calc(env(safe-area-inset-top) + 8px) 10px 8px;
  border-bottom: 1px solid #e6ece8;
}

.source-detail__topbar strong {
  overflow: hidden;
  color: #1a2a23;
  font-size: 17px;
  font-weight: 560;
  line-height: 1.35;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.source-detail__leading-icon,
.source-detail__icon-button {
  display: grid;
  width: 36px;
  height: 36px;
  place-items: center;
  border: 0;
  border-radius: 10px;
  padding: 0;
}

.source-detail__leading-icon {
  background: #eaf8f0;
  color: #07a557;
}

.source-detail__icon-button {
  background: transparent;
  color: #24372f;
  font-size: 20px;
}

.source-detail__close-button {
  grid-column: 4;
}

.source-detail__icon-button.loading {
  animation: sourceDownloadPulse 0.82s ease-in-out infinite;
}

.source-detail__body {
  min-width: 0;
  overflow-y: auto;
  padding: 18px 18px calc(env(safe-area-inset-bottom) + 30px);
  -webkit-overflow-scrolling: touch;
}

.source-detail__state {
  border-radius: 12px;
  background: #f4f8f6;
  color: #63746d;
  font-size: 15px;
  padding: 18px;
  text-align: center;
}

.source-detail__state.error {
  background: #fff3f0;
  color: #bd4b38;
}

.wiki-meta {
  display: flex;
  min-width: 0;
  align-items: center;
  gap: 8px;
  color: #71847b;
  font-size: 13px;
  margin-bottom: 18px;
}

.wiki-meta span {
  border: 1px solid #bce7cd;
  border-radius: 5px;
  color: #07a557;
  padding: 2px 6px;
}

.wiki-meta em,
.wiki-meta small {
  overflow: hidden;
  font-style: normal;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.wiki-title {
  margin: 0 0 14px;
  color: #17251f;
  font-size: 25px;
  font-weight: 660;
  line-height: 1.35;
}

.wiki-summary {
  margin: 0 0 18px;
  color: #3c4c45;
  font-size: 16px;
  line-height: 1.85;
}

.detail-section + .detail-section {
  margin-top: 24px;
}

.detail-section h2 {
  display: flex;
  align-items: center;
  gap: 8px;
  margin: 0 0 14px;
  color: #17251f;
  font-size: 18px;
  font-weight: 650;
  line-height: 1.35;
}

.detail-section h2::before {
  width: 3px;
  height: 16px;
  border-radius: 999px;
  background: #07c160;
  content: "";
}

.detail-grid {
  display: grid;
  grid-template-columns: 82px minmax(0, 1fr);
  gap: 12px 14px;
  color: #6f7f78;
  font-size: 14px;
  line-height: 1.5;
}

.detail-grid strong {
  min-width: 0;
  overflow-wrap: anywhere;
  color: #26372f;
  font-weight: 450;
}

.file-type-chip {
  display: inline-flex;
  border-radius: 5px;
  background: #f0f3f2;
  color: #4d6057;
  font-style: normal;
  padding: 2px 7px;
}

.summary-card {
  border-radius: 10px;
  background: #f5f6f6;
  color: #1f2d27;
  font-size: 16px;
  line-height: 1.9;
  padding: 14px;
}

.summary-card p {
  margin: 0;
  white-space: pre-wrap;
  overflow-wrap: anywhere;
}

.open-web-button {
  width: 100%;
  height: 46px;
  border: 0;
  border-radius: 12px;
  background: #07c160;
  color: #fff;
  font-size: 16px;
  font-weight: 560;
  margin-top: 14px;
}

.mobile-rich-content {
  color: #1f2d27;
  font-size: 16px;
  line-height: 1.9;
  overflow-wrap: anywhere;
}

.mobile-rich-content :deep(h1),
.mobile-rich-content :deep(h2),
.mobile-rich-content :deep(h3) {
  color: #17251f;
  line-height: 1.35;
}

.mobile-rich-content :deep(h1) {
  font-size: 23px;
}

.mobile-rich-content :deep(h2) {
  font-size: 20px;
  margin-top: 22px;
}

.mobile-rich-content :deep(h3) {
  font-size: 18px;
}

.mobile-rich-content :deep(p) {
  margin: 0 0 14px;
}

.mobile-rich-content :deep(ul),
.mobile-rich-content :deep(ol) {
  padding-left: 20px;
}

.mobile-rich-content :deep(a),
.mobile-rich-content :deep(.citation-wiki) {
  color: #07a557;
  font-weight: 560;
  text-decoration: none;
}

.mobile-rich-content :deep(table) {
  display: block;
  width: 100%;
  overflow-x: auto;
  border-collapse: collapse;
}

.mobile-rich-content :deep(th),
.mobile-rich-content :deep(td) {
  border-bottom: 1px solid #e4ede8;
  padding: 8px 6px;
}

@keyframes sourceDownloadPulse {
  0%,
  100% {
    opacity: 0.58;
    transform: translateY(-2px);
  }
  50% {
    opacity: 1;
    transform: translateY(2px);
  }
}
</style>
