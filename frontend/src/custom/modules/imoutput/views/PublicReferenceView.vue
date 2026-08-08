<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from "vue";
import { useRoute, useRouter } from "vue-router";
import DocumentPreview from "@/components/document-preview.vue";
import {
  downloadPublicReferenceOriginalNatively,
  formatReferenceFileType,
  formatSourceLocator,
  loadPublicReference,
  loadPublicReferenceOriginal,
  publicReferenceToken,
  type PublicReferenceView,
  wikiPageTypeLabel,
} from "../publicReference";
import { renderPublicReferenceMarkdown } from "../publicReferenceMarkdown";
import { buildMobileReferenceReaderURL, shouldUseMobileReferenceReader } from "../referenceReaderRoute";
import { BRAND_LOGO_URL, BRAND_NAME } from "@/custom/modules/branding";

const route = useRoute();
const router = useRouter();
const loading = ref(true);
const error = ref("");
const reference = ref<PublicReferenceView | null>(null);
let activeController: AbortController | null = null;
let referrerMeta: HTMLMetaElement | null = null;
let previousReferrerContent: string | null = null;

const userAgentData = typeof navigator === "undefined"
  ? undefined
  : (navigator as Navigator & { userAgentData?: { mobile?: boolean } }).userAgentData;
const redirectToMobileReader = typeof navigator !== "undefined" &&
  shouldUseMobileReferenceReader(navigator.userAgent, userAgentData?.mobile === true);

const token = computed(() => publicReferenceToken(route.query));
const originalMode = computed(() => route.query.view === "original");
const isDocument = computed(() => reference.value?.type === "knowledge");
const documentInfo = computed(() => reference.value?.document);
const fragment = computed(() => reference.value?.fragment);
const wiki = computed(() => reference.value?.wiki);
const fragmentHtml = computed(() => renderPublicReferenceMarkdown(fragment.value?.content));
const wikiHtml = computed(() => renderPublicReferenceMarkdown(wiki.value?.content || wiki.value?.summary));
const sourceLocator = computed(() => formatSourceLocator(fragment.value?.source_locator));
const fileType = computed(() => formatReferenceFileType(documentInfo.value));

watch(token, () => void loadReference(), { immediate: !redirectToMobileReader });

onMounted(() => {
  if (redirectToMobileReader) {
    window.location.replace(buildMobileReferenceReaderURL(window.location.origin, token.value, String(route.query.view || "")));
    return;
  }
  referrerMeta = documentHeadReferrerMeta();
});

onBeforeUnmount(() => {
  activeController?.abort();
  if (referrerMeta && previousReferrerContent !== null) {
    referrerMeta.content = previousReferrerContent;
  }
});

async function loadReference() {
  activeController?.abort();
  const controller = new AbortController();
  activeController = controller;
  loading.value = true;
  error.value = "";
  reference.value = null;
  try {
    reference.value = await loadPublicReference(token.value, controller.signal);
  } catch (reason: any) {
    if (reason?.name !== "AbortError") error.value = reason?.message || "引用链接无效或内容已不可用";
  } finally {
    if (activeController === controller) loading.value = false;
  }
}

function showOriginal() {
  void router.replace({ path: route.path, query: { token: token.value, view: "original" } });
}

function showCitation() {
  void router.replace({ path: route.path, query: { token: token.value } });
}

function downloadOriginal() {
  downloadPublicReferenceOriginalNatively(token.value);
}

function documentHeadReferrerMeta(): HTMLMetaElement {
  const existing = document.head.querySelector<HTMLMetaElement>('meta[name="referrer"]');
  if (existing) {
    previousReferrerContent = existing.content;
    existing.content = "no-referrer";
    return existing;
  }
  const meta = document.createElement("meta");
  meta.name = "referrer";
  meta.content = "no-referrer";
  document.head.appendChild(meta);
  previousReferrerContent = "";
  return meta;
}
</script>

<template>
  <div class="public-reference-page" data-testid="desktop-public-reference">
    <header class="public-reference-header">
      <a class="brand" href="/" :aria-label="`${BRAND_NAME}首页`">
        <img :src="BRAND_LOGO_URL" :alt="BRAND_NAME" />
      </a>
    </header>

    <main class="public-reference-main">
      <section v-if="loading" class="state-card" data-testid="public-reference-loading">
        <i />正在读取引用来源
      </section>
      <section v-else-if="error" class="state-card error" data-testid="public-reference-error">
        <h1>无法打开引用</h1>
        <p>{{ error }}</p>
      </section>

      <template v-else-if="reference">
        <section class="reference-title-card">
          <div>
            <span class="type-chip">{{ isDocument ? '文档分片' : 'Wiki' }}</span>
            <span v-if="wiki?.version" class="version-chip">v{{ wiki.version }}</span>
          </div>
          <h1>{{ reference.title }}</h1>
        </section>

        <section v-if="isDocument && documentInfo && fragment" class="reader-card">
          <header class="reader-card__header">
            <div class="document-meta">
              <span>{{ fileType }}</span>
              <span>第 {{ fragment.chunk_index + 1 }} 个分片</span>
              <span v-if="sourceLocator">{{ sourceLocator }}</span>
            </div>
            <div class="reader-actions">
              <button v-if="!originalMode" type="button" class="primary-action" @click="showOriginal">
                查看原文档
              </button>
              <button v-else type="button" @click="showCitation">返回引用分片</button>
              <button
                v-if="originalMode"
                type="button"
                @click="downloadOriginal"
              >下载</button>
            </div>
          </header>

          <div v-if="!originalMode" class="reference-content rich-content" v-html="fragmentHtml" />
          <div v-else class="original-preview" data-testid="desktop-public-original">
            <DocumentPreview
              :sourceKey="`im-reference:${token}`"
              :fileType="documentInfo.file_type || fileType"
              :fileName="documentInfo.file_name || documentInfo.title"
              :fileSize="documentInfo.file_size"
              :active="true"
              :blobLoader="(signal) => loadPublicReferenceOriginal(token, signal)"
            />
          </div>
        </section>

        <article v-else-if="wiki" class="reader-card wiki-reader">
          <header class="wiki-meta">
            <span>{{ wikiPageTypeLabel(wiki.page_type) }}</span>
            <code>{{ wiki.slug }}</code>
          </header>
          <p v-if="wiki.summary" class="wiki-summary">{{ wiki.summary }}</p>
          <div class="reference-content rich-content" v-html="wikiHtml" />
        </article>
      </template>
    </main>
  </div>
</template>

<style scoped>
.public-reference-page {
  min-height: 100vh;
  background: #f4f7f5;
  color: #17251f;
  font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", "PingFang SC", "Microsoft YaHei", sans-serif;
}

.public-reference-header {
  position: sticky;
  z-index: 10;
  top: 0;
  display: flex;
  min-height: 64px;
  align-items: center;
  border-bottom: 1px solid #e3eae6;
  background: rgb(255 255 255 / 96%);
  padding: 0 28px;
  backdrop-filter: blur(14px);
}

.brand {
  display: inline-flex;
  align-items: center;
  color: #18261f;
  text-decoration: none;
}

.brand img {
  display: block;
  width: 128px;
  height: auto;
}

.public-reference-main {
  width: min(1120px, calc(100% - 48px));
  margin: 0 auto;
  padding: 38px 0 64px;
}

.state-card,
.reference-title-card,
.reader-card {
  border: 1px solid #e1e9e4;
  border-radius: 16px;
  background: #fff;
  box-shadow: 0 8px 30px rgb(30 69 49 / 5%);
}

.state-card {
  display: grid;
  min-height: 280px;
  place-content: center;
  color: #607168;
  text-align: center;
}

.state-card i {
  width: 26px;
  height: 26px;
  border: 3px solid #d6e6dc;
  border-top-color: #07c160;
  border-radius: 50%;
  margin: 0 auto 14px;
  animation: spin .8s linear infinite;
}

.state-card.error h1 { margin: 0 0 8px; color: #a93e32; }
.state-card.error p { margin: 0; }

.reference-title-card { padding: 28px 32px; }
.reference-title-card h1 { margin: 14px 0 0; font-size: 28px; line-height: 1.35; }

.type-chip,
.version-chip {
  display: inline-flex;
  border-radius: 6px;
  padding: 3px 8px;
  font-size: 12px;
}

.type-chip { border: 1px solid #bce7cd; background: #f0fbf5; color: #079c52; }
.version-chip { margin-left: 8px; background: #f0f3f2; color: #617169; }
.reader-card { margin-top: 18px; overflow: hidden; }

.reader-card__header {
  display: flex;
  min-height: 68px;
  align-items: center;
  justify-content: space-between;
  gap: 20px;
  border-bottom: 1px solid #e7ece9;
  padding: 12px 24px;
}

.document-meta { display: flex; flex-wrap: wrap; gap: 8px; color: #697a71; font-size: 13px; }
.document-meta span { border-radius: 6px; background: #f3f6f4; padding: 4px 8px; }
.reader-actions { display: flex; flex: none; align-items: center; gap: 9px; }
.reader-actions button,
.reader-actions a {
  border: 1px solid #d7e2dc;
  border-radius: 8px;
  background: #fff;
  color: #26372f;
  cursor: pointer;
  font-size: 13px;
  padding: 8px 13px;
  text-decoration: none;
}
.reader-actions .primary-action { border-color: #07c160; background: #07c160; color: white; }

.reference-content { min-height: 260px; padding: 30px 34px 50px; }
.rich-content { color: #22322b; font-size: 16px; line-height: 1.9; overflow-wrap: anywhere; }
.rich-content :deep(img) { max-width: 100%; }
.rich-content :deep(table) { display: block; max-width: 100%; overflow-x: auto; }
.original-preview { min-height: 620px; padding: 18px; }
.original-preview :deep(.document-preview) { min-height: 580px; }

.wiki-reader { padding: 30px 34px 50px; }
.wiki-meta { display: flex; flex-wrap: wrap; align-items: center; gap: 12px; color: #718079; font-size: 13px; }
.wiki-meta span:first-child { border: 1px solid #bce7cd; border-radius: 5px; color: #079c52; padding: 2px 7px; }
.wiki-meta code { border-radius: 5px; background: #f3f5f4; padding: 3px 7px; }
.wiki-summary { border-left: 3px solid #07c160; margin: 24px 0 6px; background: #f4faf7; color: #405149; line-height: 1.8; padding: 12px 16px; }
.wiki-reader .reference-content { padding-inline: 0; }

@keyframes spin { to { transform: rotate(360deg); } }

@media (max-width: 720px) {
  .public-reference-header { padding: 0 14px; }
  .public-reference-main { width: calc(100% - 24px); padding-top: 18px; }
  .reference-title-card { padding: 22px; }
  .reader-card__header { align-items: flex-start; flex-direction: column; }
  .reader-actions { width: 100%; }
  .reference-content { padding: 22px; }
}
</style>
