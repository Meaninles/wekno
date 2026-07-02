<template>
  <div class="db-metadata-result">
    <div v-if="tables.length === 0" class="empty-result">未找到相关数据库表</div>
    <div v-for="table in tables" :key="table.sql_table_name" class="metadata-table">
      <div class="metadata-table__head">
        <div>
          <div class="metadata-table__name">{{ table.sql_table_name }}</div>
          <div class="metadata-table__meta">
            {{ table.source_name || table.source_id || 'database' }} · {{ table.schema_name }}.{{ table.table_name }}
          </div>
        </div>
        <span class="metadata-table__rows">{{ table.row_estimate ?? 0 }} rows</span>
      </div>
      <div v-if="table.description" class="metadata-table__desc">{{ table.description }}</div>
      <div v-if="table.columns?.length" class="metadata-columns">
        <div v-for="column in table.columns.slice(0, 12)" :key="column.name" class="metadata-column">
          <span class="metadata-column__name">{{ column.name }}</span>
          <span class="metadata-column__type">{{ column.type }}</span>
          <span v-if="column.description" class="metadata-column__desc">{{ column.description }}</span>
        </div>
        <div v-if="table.columns.length > 12" class="metadata-more">还有 {{ table.columns.length - 12 }} 个字段</div>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue';
import type { DatabaseMetadataData } from '@/types/tool-results';

const props = defineProps<{
  data: DatabaseMetadataData;
}>();

const tables = computed(() => props.data.tables || []);
</script>

<style scoped lang="less">
.db-metadata-result {
  display: flex;
  flex-direction: column;
  gap: 10px;
  font-size: 13px;
}

.metadata-table {
  border: 1px solid var(--td-component-stroke);
  border-radius: 6px;
  background: var(--td-bg-color-container);
  overflow: hidden;
}

.metadata-table__head {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 12px;
  padding: 10px 12px;
  background: var(--td-bg-color-secondarycontainer);
}

.metadata-table__name {
  font-weight: 600;
  color: var(--td-text-color-primary);
}

.metadata-table__meta,
.metadata-table__rows,
.metadata-column__type,
.metadata-more {
  color: var(--td-text-color-secondary);
  font-size: 12px;
}

.metadata-table__desc {
  padding: 8px 12px 0;
  color: var(--td-text-color-secondary);
}

.metadata-columns {
  padding: 8px 12px 10px;
}

.metadata-column {
  display: grid;
  grid-template-columns: minmax(120px, 1fr) minmax(90px, 0.7fr) minmax(160px, 2fr);
  gap: 8px;
  padding: 5px 0;
  border-bottom: 1px solid var(--td-component-stroke);
}

.metadata-column:last-child {
  border-bottom: 0;
}

.metadata-column__name {
  font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, "Liberation Mono", monospace;
  color: var(--td-text-color-primary);
}

.metadata-column__desc {
  color: var(--td-text-color-secondary);
  overflow-wrap: anywhere;
}

.empty-result {
  padding: 24px;
  text-align: center;
  color: var(--td-text-color-placeholder);
  border: 1px solid var(--td-component-stroke);
  border-radius: 6px;
  background: var(--td-bg-color-secondarycontainer);
}
</style>
