<script setup lang="ts">
import { computed, ref } from "vue";
import {
  buildSourceReferenceItems,
  sourceTypeLabel,
  type SourceReference,
  type SourceReferenceKind,
} from "@/utils/sourceReferences";

const props = defineProps<{
  references?: SourceReference[];
}>();

const expanded = ref(false);

const rows = computed(() => buildSourceReferenceItems(props.references || [])
  .map((item) => ({
    key: item.key,
    type: item.type,
    title: item.title || "未命名来源",
    meta: item.type === "knowledge" && (item.fragmentCount || item.count) > 1
      ? `${item.fragmentCount || item.count} 个文档片段`
      : sourceTypeLabel(item.type),
  })));

const summary = computed(() => {
  const counts = rows.value.reduce<Record<SourceReferenceKind, number>>(
    (acc, row) => {
      acc[row.type] += 1;
      return acc;
    },
    { knowledge: 0, wiki: 0, web: 0, data_source: 0 },
  );
  const parts: string[] = [];
  if (counts.knowledge) parts.push(`${counts.knowledge}个文档片段`);
  if (counts.wiki) parts.push(`${counts.wiki}个Wiki`);
  if (counts.web) parts.push(`${counts.web}条搜索`);
  if (counts.data_source) parts.push(`${counts.data_source}个数据源`);
  return parts.length ? `引用了${parts.join("、")}` : `引用了${rows.value.length}个来源`;
});
</script>

<template>
  <div v-if="rows.length" class="mobile-source-ref">
    <button type="button" class="source-ref-head" @click="expanded = !expanded">
      <span class="source-ref-head__icon"><MobileIcon name="file-search" /></span>
      <span class="source-ref-head__text">{{ summary }}</span>
      <MobileIcon :name="expanded ? 'chevron-up' : 'chevron-down'" />
    </button>
    <div v-if="expanded" class="source-ref-list">
      <div v-for="item in rows" :key="item.key" class="source-ref-row" :class="`is-${item.type}`">
        <span class="source-ref-row__dot" />
        <span class="source-ref-row__title">{{ item.title }}</span>
        <span class="source-ref-row__meta">{{ item.meta }}</span>
      </div>
    </div>
  </div>
</template>

<style scoped>
.mobile-source-ref {
  overflow: hidden;
  border: 1px solid #dce7e1;
  border-radius: 8px;
  background: #f8fbf9;
}

.source-ref-head {
  display: grid;
  width: 100%;
  grid-template-columns: 24px 1fr 18px;
  align-items: center;
  border: 0;
  background: transparent;
  color: #30433b;
  padding: 8px 9px;
  text-align: left;
}

.source-ref-head__icon {
  color: #07a557;
}

.source-ref-head__text {
  font-size: 13px;
  font-weight: 650;
  overflow-wrap: anywhere;
}

.source-ref-list {
  display: flex;
  flex-direction: column;
  gap: 4px;
  padding: 0 9px 8px;
}

.source-ref-row {
  display: grid;
  grid-template-columns: 8px minmax(0, 1fr);
  align-items: start;
  gap: 7px;
  color: #40554c;
  font-size: 13px;
}

.source-ref-row__dot {
  width: 6px;
  height: 6px;
  border-radius: 50%;
  background: #07c160;
}

.source-ref-row.is-web .source-ref-row__dot {
  background: #168596;
}

.source-ref-row.is-wiki .source-ref-row__dot {
  background: #6958d8;
}

.source-ref-row.is-data_source .source-ref-row__dot {
  background: #b56d13;
}

.source-ref-row__title {
  overflow-wrap: anywhere;
}

.source-ref-row__meta {
  grid-column: 2;
  color: #7a8b83;
}
</style>
