<script setup lang="ts">
import { computed, nextTick, ref, watch } from "vue";
import {
  hostFromUrl,
  sourceTypeLabel,
  type SourceReferenceItem,
} from "@/utils/sourceReferences";
import { createSafeImage, hydrateProtectedFileImages, sanitizeMarkdownHTML } from "@/utils/security";

const props = defineProps<{
  item: SourceReferenceItem | null;
}>();

const emit = defineEmits<{
  close: [];
  open: [item: SourceReferenceItem];
}>();

const imagePreviewRef = ref<HTMLElement | null>(null);

const iconFor = (item: SourceReferenceItem | null) => {
  if (!item) return "file";
  if (item.type === "web") return "internet";
  if (item.type === "wiki") return "bookmark";
  if (item.type === "data_source") return "tools";
  return "file";
};

const sourceMeta = (item: SourceReferenceItem) => {
  const typeText = sourceTypeLabel(item.type);
  const meta = item.type === "web" ? hostFromUrl(item.url) || item.sourceLabel || "" : item.sourceLabel || "";
  return meta && meta !== typeText ? meta : "";
};

const citationImageSource = computed(() =>
  firstImageSource(`${props.item?.content || ""}\n${props.item?.snippet || ""}`),
);

const citationImageHtml = computed(() => {
  if (!citationImageSource.value) return "";
  return sanitizeMarkdownHTML(createSafeImage(citationImageSource.value, props.item?.title || "", ""));
});

const citationPreview = computed(() => {
  const snippet = stripImageMarkup(String(props.item?.snippet || "")).trim();
  if (snippet) return snippet;

  const content = stripImageMarkup(String(props.item?.content || ""))
    .replace(/\s+/g, " ")
    .trim();
  if (content.length <= 120) return content;
  return `${content.slice(0, 120)}...`;
});

watch(
  citationImageHtml,
  async () => {
    if (!citationImageHtml.value) return;
    await nextTick();
    await hydrateProtectedFileImages(imagePreviewRef.value);
  },
  { immediate: true, flush: "post" },
);

function decodeImageSource(value: string) {
  const source = value
    .trim()
    .replace(/^<([\s\S]*)>$/, "$1")
    .replace(/^['"]|['"]$/g, "");
  if (!source) return "";
  if (typeof document === "undefined") {
    return source
      .replace(/&amp;/g, "&")
      .replace(/&quot;/g, '"')
      .replace(/&#x2f;/gi, "/")
      .replace(/&#47;/g, "/");
  }
  const textarea = document.createElement("textarea");
  textarea.innerHTML = source;
  return textarea.value;
}

function firstImageSource(content: string) {
  if (!content) return "";
  const htmlMatch = content.match(/<img\b[^>]*>/i);
  if (htmlMatch?.[0]) {
    const protectedMatch = htmlMatch[0].match(/\bdata-protected-src=(["'])(.*?)\1/i);
    if (protectedMatch?.[2]) return decodeImageSource(protectedMatch[2]);
    const srcMatch = htmlMatch[0].match(/\bsrc=(["'])(.*?)\1/i);
    if (srcMatch?.[2]) return decodeImageSource(srcMatch[2]);
  }

  const markdownMatch = content.match(/!\[[^\]]*]\(\s*(<[^>]+>|[^\s)]+)(?:\s+["'][^"']*["'])?\s*\)/);
  return markdownMatch?.[1] ? decodeImageSource(markdownMatch[1]) : "";
}

function stripImageMarkup(content: string) {
  if (!content) return "";
  return content
    .replace(/<img\b[^>]*>/gi, "")
    .replace(/!\[[^\]]*]\(\s*(?:<[^>]+>|[^)]+)\s*\)/g, "")
    .trim();
}

const openLabel = (item: SourceReferenceItem) => {
  if (item.type === "web") return "打开网页";
  if (item.type === "wiki") return "查看 Wiki";
  if (item.type === "knowledge") return "查看文档片段";
  return "查看来源";
};
</script>

<template>
  <div v-if="props.item" class="citation-sheet-layer" @click.self="emit('close')">
    <section class="citation-sheet" role="dialog" aria-modal="true">
      <div class="citation-sheet__grip" />
      <button type="button" class="citation-card" :disabled="!props.item.clickable" @click="emit('open', props.item)">
        <div class="citation-card__head">
          <span class="citation-card__number">{{ props.item.number }}.</span>
          <strong>{{ props.item.title }}</strong>
        </div>
        <div
          v-if="citationImageHtml"
          ref="imagePreviewRef"
          class="citation-card__image-preview"
          v-html="citationImageHtml"
        />
        <p v-if="citationPreview" class="citation-card__snippet">{{ citationPreview }}</p>
        <div class="citation-card__meta">
          <span class="citation-card__icon"><MobileIcon :name="iconFor(props.item)" /></span>
          <span>{{ sourceTypeLabel(props.item.type) }}</span>
          <span v-if="sourceMeta(props.item)" class="citation-card__source">{{ sourceMeta(props.item) }}</span>
        </div>
      </button>
      <div class="citation-sheet__actions">
        <button type="button" class="citation-sheet__ghost" @click="emit('close')">关闭</button>
        <button
          type="button"
          class="citation-sheet__primary"
          :disabled="!props.item.clickable"
          @click="emit('open', props.item)"
        >
          {{ openLabel(props.item) }}
        </button>
      </div>
    </section>
  </div>
</template>

<style scoped>
.citation-sheet-layer {
  position: fixed;
  z-index: 70;
  inset: 0;
  display: flex;
  align-items: flex-end;
  background: rgba(10, 22, 16, 0.34);
}

.citation-sheet {
  width: 100%;
  max-height: 86dvh;
  overflow-y: auto;
  border-radius: 20px 20px 0 0;
  background: #f7f8f8;
  padding: 10px 14px calc(env(safe-area-inset-bottom) + 14px);
  -webkit-overflow-scrolling: touch;
}

.citation-sheet__grip {
  width: 44px;
  height: 5px;
  border-radius: 999px;
  background: #d6ddd9;
  margin: 0 auto 14px;
}

.citation-card {
  display: flex;
  width: 100%;
  min-width: 0;
  flex-direction: column;
  gap: 12px;
  border: 0;
  border-radius: 18px;
  background: #fff;
  color: #1b2923;
  padding: 18px 16px 16px;
  text-align: left;
  box-shadow: 0 8px 24px rgba(18, 35, 27, 0.08);
}

.citation-card:disabled {
  opacity: 0.82;
}

.citation-card__head {
  display: flex;
  min-width: 0;
  align-items: flex-start;
  gap: 8px;
}

.citation-card__number {
  flex: 0 0 auto;
  color: #07a557;
  font-size: 20px;
  font-weight: 650;
  line-height: 1.45;
}

.citation-card__head strong {
  display: -webkit-box;
  min-width: 0;
  overflow: hidden;
  color: #1b2923;
  font-size: 20px;
  font-weight: 560;
  line-height: 1.45;
  overflow-wrap: anywhere;
  -webkit-box-orient: vertical;
  -webkit-line-clamp: 2;
}

.citation-card__image-preview {
  width: 100%;
  overflow: hidden;
  border-radius: 12px;
  background: #edf3f0;
}

.citation-card__image-preview :deep(img.markdown-image),
.citation-card__image-preview :deep(img[data-protected-src]) {
  display: block;
  width: 100%;
  max-height: min(44dvh, 340px);
  object-fit: contain;
  background: #edf3f0;
}

.citation-card__image-preview :deep(img[data-img-loading]) {
  min-height: 150px;
  opacity: 0.9;
}

.citation-card__snippet {
  display: -webkit-box;
  overflow: hidden;
  margin: 0;
  color: #6d7b75;
  font-size: 16px;
  line-height: 1.8;
  overflow-wrap: anywhere;
  white-space: pre-wrap;
  -webkit-box-orient: vertical;
  -webkit-line-clamp: 4;
}

.citation-card__meta {
  display: flex;
  min-width: 0;
  align-items: flex-start;
  flex-wrap: wrap;
  gap: 7px;
  color: #7b8c84;
  font-size: 14px;
}

.citation-card__icon {
  display: grid;
  width: 22px;
  height: 22px;
  place-items: center;
  border-radius: 50%;
  background: #eef9f3;
  color: #07a557;
}

.citation-card__source {
  min-width: 0;
  overflow-wrap: anywhere;
}

.citation-sheet__actions {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: 10px;
  margin-top: 12px;
}

.citation-sheet__ghost,
.citation-sheet__primary {
  height: 44px;
  border: 0;
  border-radius: 12px;
  font-size: 16px;
}

.citation-sheet__ghost {
  background: #fff;
  color: #50645a;
}

.citation-sheet__primary {
  background: #07c160;
  color: #fff;
  font-weight: 560;
}

.citation-sheet__primary:disabled {
  background: #c8d7d0;
}
</style>
