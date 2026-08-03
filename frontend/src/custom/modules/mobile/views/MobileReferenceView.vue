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
} from "@/custom/modules/imoutput/publicReference";
import { renderMobileMarkdown } from "../mobileMarkdown";
import MobileIcon from "../components/MobileIcon.vue";

const route = useRoute();
const router = useRouter();
const loading = ref(true);
const error = ref("");
const reference = ref<PublicReferenceView | null>(null);
let activeController: AbortController | null = null;
let referrerMeta: HTMLMetaElement | null = null;
let previousReferrerContent: string | null = null;

const token = computed(() => publicReferenceToken(route.query));
const originalMode = computed(() => route.query.view === "original");
const isDocument = computed(() => reference.value?.type === "knowledge");
const documentInfo = computed(() => reference.value?.document);
const fragment = computed(() => reference.value?.fragment);
const wiki = computed(() => reference.value?.wiki);
const fragmentHtml = computed(() => renderMobileMarkdown(fragment.value?.content));
const wikiHtml = computed(() => renderMobileMarkdown(wiki.value?.content || wiki.value?.summary));
const sourceLocator = computed(() => formatSourceLocator(fragment.value?.source_locator));
const fileType = computed(() => formatReferenceFileType(documentInfo.value));

watch(token, () => void loadReference(), { immediate: true });

onMounted(() => {
  const existing = document.head.querySelector<HTMLMetaElement>('meta[name="referrer"]');
  if (existing) {
    previousReferrerContent = existing.content;
    existing.content = "no-referrer";
    referrerMeta = existing;
  } else {
    const meta = document.createElement("meta");
    meta.name = "referrer";
    meta.content = "no-referrer";
    document.head.appendChild(meta);
    referrerMeta = meta;
    previousReferrerContent = "";
  }
});

onBeforeUnmount(() => {
  activeController?.abort();
  if (referrerMeta && previousReferrerContent !== null) referrerMeta.content = previousReferrerContent;
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

</script>

<template>
  <main class="mobile-public-reference" data-testid="mobile-public-reference">
    <header class="mobile-public-reference__topbar">
      <div>
        <strong>{{ originalMode ? '原文档' : reference?.title || '引用来源' }}</strong>
      </div>
      <span class="brand-mark">W</span>
    </header>

    <section v-if="loading" class="mobile-reference-state" data-testid="mobile-public-reference-loading">
      <i />
      <p>正在读取引用来源</p>
    </section>
    <section v-else-if="error" class="mobile-reference-state error" data-testid="mobile-public-reference-error">
      <MobileIcon name="info" />
      <h1>无法打开引用</h1>
      <p>{{ error }}</p>
    </section>

    <template v-else-if="reference">
      <section v-if="isDocument && documentInfo && fragment" class="mobile-reference-body">
        <div v-if="!originalMode" class="mobile-reference-title">
          <span>文档分片</span>
          <h1>{{ reference.title }}</h1>
        </div>

        <template v-if="!originalMode">
          <section class="mobile-meta-card">
            <div><span>文档</span><strong>{{ documentInfo.file_name || documentInfo.title }}</strong></div>
            <div><span>类型</span><strong>{{ fileType }}</strong></div>
            <div><span>分片</span><strong>第 {{ fragment.chunk_index + 1 }} 个</strong></div>
            <div v-if="sourceLocator"><span>位置</span><strong>{{ sourceLocator }}</strong></div>
          </section>

          <section class="mobile-content-card">
            <h2>文档片段</h2>
            <div class="mobile-rich-content" v-html="fragmentHtml" />
          </section>

          <button type="button" class="mobile-primary-action" @click="showOriginal">
            <MobileIcon name="eye" />
            查看原文档
          </button>
        </template>

        <template v-else>
          <div class="mobile-original-actions">
            <button type="button" @click="showCitation">返回引用分片</button>
            <button type="button" @click="downloadOriginal">下载</button>
          </div>
          <div class="mobile-original-preview" data-testid="mobile-public-original">
            <DocumentPreview
              :sourceKey="`im-reference-mobile:${token}`"
              :fileType="documentInfo.file_type || fileType"
              :fileName="documentInfo.file_name || documentInfo.title"
              :fileSize="documentInfo.file_size"
              :active="true"
              :blobLoader="(signal) => loadPublicReferenceOriginal(token, signal)"
            />
          </div>
        </template>
      </section>

      <article v-else-if="wiki" class="mobile-reference-body mobile-wiki-reader">
        <div class="mobile-reference-title">
          <div class="wiki-chips">
            <span>{{ wikiPageTypeLabel(wiki.page_type) }}</span>
            <em v-if="wiki.version">v{{ wiki.version }}</em>
          </div>
          <h1>{{ wiki.title || reference.title }}</h1>
        </div>
        <p v-if="wiki.summary" class="mobile-wiki-summary">{{ wiki.summary }}</p>
        <section class="mobile-content-card">
          <div class="mobile-rich-content" v-html="wikiHtml" />
        </section>
      </article>
    </template>
  </main>
</template>

<style scoped>
.mobile-public-reference {
  min-height: 100dvh;
  background: #f4f7f5;
  color: #17251f;
}

.mobile-public-reference__topbar {
  position: sticky;
  z-index: 20;
  top: 0;
  display: grid;
  min-height: calc(58px + env(safe-area-inset-top));
  grid-template-columns: minmax(0, 1fr) 36px;
  align-items: center;
  gap: 8px;
  border-bottom: 1px solid #e2eae5;
  background: rgb(255 255 255 / 97%);
  padding: calc(env(safe-area-inset-top) + 7px) 12px 7px;
  backdrop-filter: blur(14px);
}

.mobile-public-reference__topbar div { min-width: 0; }
.mobile-public-reference__topbar strong { display: block; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.mobile-public-reference__topbar strong { font-size: 16px; font-weight: 620; }

.brand-mark {
  display: grid;
  width: 32px;
  height: 32px;
  place-items: center;
  border-radius: 9px;
  background: #07c160;
  color: #fff;
  font-size: 17px;
  font-weight: 700;
}

.mobile-reference-state {
  display: grid;
  min-height: calc(100dvh - 70px);
  place-content: center;
  padding: 30px;
  color: #6b7b73;
  text-align: center;
}

.mobile-reference-state i {
  width: 28px;
  height: 28px;
  border: 3px solid #d6e6dc;
  border-top-color: #07c160;
  border-radius: 50%;
  margin: 0 auto 12px;
  animation: spin .8s linear infinite;
}

.mobile-reference-state h1 { margin: 12px 0 4px; color: #9f4035; font-size: 21px; }
.mobile-reference-state p { margin: 0; line-height: 1.7; }
.mobile-reference-body { padding: 20px 14px calc(34px + env(safe-area-inset-bottom)); }

.mobile-reference-title { padding: 4px 4px 18px; }
.mobile-reference-title > span,
.wiki-chips span {
  display: inline-flex;
  border: 1px solid #bce7cd;
  border-radius: 5px;
  background: #f0fbf5;
  color: #079c52;
  font-size: 12px;
  padding: 2px 7px;
}
.mobile-reference-title h1 { margin: 12px 0 0; font-size: 23px; font-weight: 660; line-height: 1.4; }
.wiki-chips { display: flex; align-items: center; gap: 8px; }
.wiki-chips em { border-radius: 5px; background: #e9eeeb; color: #617169; font-size: 12px; font-style: normal; padding: 3px 7px; }

.mobile-meta-card,
.mobile-content-card,
.mobile-wiki-summary {
  border: 1px solid #e0e8e3;
  border-radius: 14px;
  background: #fff;
  box-shadow: 0 5px 18px rgb(38 72 55 / 4%);
}

.mobile-meta-card { display: grid; gap: 12px; padding: 16px; }
.mobile-meta-card div { display: grid; grid-template-columns: 62px minmax(0, 1fr); gap: 10px; font-size: 14px; line-height: 1.55; }
.mobile-meta-card span { color: #7b8982; }
.mobile-meta-card strong { min-width: 0; color: #26372f; font-weight: 480; overflow-wrap: anywhere; }
.mobile-content-card { margin-top: 14px; padding: 17px; }
.mobile-content-card h2 { margin: 0 0 14px; font-size: 17px; font-weight: 650; }
.mobile-content-card h2::before { display: inline-block; width: 3px; height: 15px; border-radius: 99px; background: #07c160; content: ""; margin-right: 8px; vertical-align: -2px; }
.mobile-rich-content { color: #22322b; font-size: 16px; line-height: 1.95; overflow-wrap: anywhere; }
.mobile-rich-content :deep(img) { max-width: 100%; }
.mobile-rich-content :deep(table) { display: block; max-width: 100%; overflow-x: auto; }

.mobile-primary-action {
  display: flex;
  width: 100%;
  height: 48px;
  align-items: center;
  justify-content: center;
  gap: 7px;
  border: 0;
  border-radius: 13px;
  background: #07c160;
  color: #fff;
  font-size: 16px;
  font-weight: 600;
  margin-top: 16px;
}

.mobile-original-actions { display: grid; grid-template-columns: 1fr 1fr; gap: 10px; margin-bottom: 12px; }
.mobile-original-actions button {
  display: grid;
  height: 42px;
  place-items: center;
  border: 1px solid #d7e2dc;
  border-radius: 11px;
  background: #fff;
  color: #26372f;
}
.mobile-original-preview { min-height: calc(100dvh - 132px); overflow: hidden; border: 1px solid #e0e8e3; border-radius: 14px; background: #fff; }
.mobile-original-preview :deep(.document-preview) { min-height: calc(100dvh - 145px); }
.mobile-wiki-summary { margin: 0 0 14px; border-left: 3px solid #07c160; color: #405149; font-size: 15px; line-height: 1.8; padding: 13px 15px; }

@keyframes spin { to { transform: rotate(360deg); } }
</style>
