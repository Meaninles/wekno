<script setup lang="ts">
import {
  hostFromUrl,
  sourceTypeLabel,
  type SourceReferenceItem,
} from "@/utils/sourceReferences";

const props = defineProps<{
  item: SourceReferenceItem | null;
}>();

const emit = defineEmits<{
  close: [];
  open: [item: SourceReferenceItem];
}>();

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
        <p v-if="props.item.snippet" class="citation-card__snippet">{{ props.item.snippet }}</p>
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
          {{ props.item.type === 'web' ? '打开网页' : props.item.type === 'wiki' ? '查看 Wiki' : '查看来源' }}
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
  border-radius: 20px 20px 0 0;
  background: #f7f8f8;
  padding: 10px 14px calc(env(safe-area-inset-bottom) + 14px);
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
  overflow: hidden;
  color: #1b2923;
  font-size: 20px;
  font-weight: 560;
  line-height: 1.45;
  -webkit-box-orient: vertical;
  -webkit-line-clamp: 2;
}

.citation-card__snippet {
  display: -webkit-box;
  overflow: hidden;
  margin: 0;
  color: #6d7b75;
  font-size: 16px;
  line-height: 1.8;
  -webkit-box-orient: vertical;
  -webkit-line-clamp: 4;
}

.citation-card__meta {
  display: flex;
  min-width: 0;
  align-items: center;
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
  overflow: hidden;
  min-width: 0;
  text-overflow: ellipsis;
  white-space: nowrap;
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
