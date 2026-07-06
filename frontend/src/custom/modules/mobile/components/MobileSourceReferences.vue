<script setup lang="ts">
import { computed, ref } from "vue";

type SourceKind = "knowledge" | "wiki" | "web" | "data_source";

type SourceReference = {
  id?: string;
  knowledge_id?: string;
  knowledge_title?: string;
  knowledge_filename?: string;
  knowledge_base_id?: string;
  chunk_type?: string;
  metadata?: Record<string, string>;
};

const props = defineProps<{
  references?: SourceReference[];
}>();

const expanded = ref(false);

const sourceKind = (ref: SourceReference): SourceKind => {
  const metadataType = ref.metadata?.source_type;
  if (metadataType === "wiki") return "wiki";
  if (metadataType === "web") return "web";
  if (metadataType === "data_source") return "data_source";
  if (ref.chunk_type === "wiki_page") return "wiki";
  if (ref.chunk_type === "web_search") return "web";
  if (ref.chunk_type === "data_source") return "data_source";
  return "knowledge";
};

const rows = computed(() => {
  const seen = new Set<string>();
  return (props.references || [])
    .map((ref) => {
      const type = sourceKind(ref);
      const metadata = ref.metadata || {};
      const title =
        ref.knowledge_title ||
        ref.knowledge_filename ||
        metadata.title ||
        metadata.url ||
        ref.knowledge_id ||
        ref.id ||
        "未命名来源";
      const key = `${type}:${ref.knowledge_id || ref.id || title}`;
      if (seen.has(key)) return null;
      seen.add(key);
      return {
        key,
        type,
        title,
        meta:
          type === "web"
            ? "搜索"
            : type === "wiki"
              ? "Wiki"
              : type === "data_source"
                ? "数据源"
                : "知识库",
      };
    })
    .filter(Boolean) as Array<{ key: string; type: SourceKind; title: string; meta: string }>;
});

const summary = computed(() => {
  const counts = rows.value.reduce<Record<SourceKind, number>>(
    (acc, row) => {
      acc[row.type] += 1;
      return acc;
    },
    { knowledge: 0, wiki: 0, web: 0, data_source: 0 },
  );
  const parts: string[] = [];
  if (counts.knowledge) parts.push(`${counts.knowledge}篇文档`);
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
  overflow: hidden;
  font-size: 13px;
  font-weight: 650;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.source-ref-list {
  display: flex;
  flex-direction: column;
  gap: 4px;
  padding: 0 9px 8px;
}

.source-ref-row {
  display: grid;
  grid-template-columns: 8px minmax(0, 1fr) auto;
  align-items: center;
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
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.source-ref-row__meta {
  color: #7a8b83;
}
</style>
