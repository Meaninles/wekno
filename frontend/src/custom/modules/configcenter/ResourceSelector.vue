<template>
  <div class="resource-selector">
    <div v-if="loading" class="selector-state">加载中...</div>
    <div v-else-if="groups.length === 0" class="selector-state">暂无可选资源</div>
    <div v-else class="resource-groups">
      <section v-for="group in groups" :key="group.type" class="resource-group">
        <div class="resource-group__header">
          <span>{{ group.label }}</span>
          <t-tag size="small" variant="light">{{ group.items.length }}</t-tag>
        </div>
        <div class="resource-list">
          <label
            v-for="item in group.items"
            :key="resourceKey(item)"
            class="resource-row"
            :class="[`resource-row--${item.resource_type}`, { 'resource-row--active': isChecked(item) }]"
          >
            <t-checkbox :checked="isChecked(item)" @change="(checked: unknown) => toggle(item, Boolean(checked))" />
            <span class="resource-row__badge" aria-hidden="true">{{ badgeText(group.label) }}</span>
            <SettingCard
              class="resource-row__card"
              :title="item.name"
              :description="item.description || resourceDescription(item, group.label)"
              :disabled="!item.enabled"
            >
              <template #tags>
                <t-tag size="small" variant="light">{{ group.label }}</t-tag>
                <t-tag v-if="item.kind" size="small" variant="light-outline">{{ item.kind }}</t-tag>
                <t-tag
                  size="small"
                  :theme="item.enabled ? 'success' : 'warning'"
                  variant="light"
                >
                  {{ item.enabled ? '可用' : '已禁用' }}
                </t-tag>
              </template>
              <template #meta>
                <span>源用户自身工作区配置</span>
              </template>
            </SettingCard>
          </label>
        </div>
      </section>
    </div>
  </div>
</template>

<script setup lang="ts">
import SettingCard from '@/components/settings/SettingCard.vue'
import type { ResourceSummary } from '@/api/custom-admin'

interface ResourceGroup {
  type: string
  label: string
  items: ResourceSummary[]
}

const props = withDefaults(defineProps<{
  modelValue: string[]
  groups: ResourceGroup[]
  loading?: boolean
}>(), {
  modelValue: () => [],
  groups: () => [],
  loading: false,
})

const emit = defineEmits<{
  (event: 'update:modelValue', value: string[]): void
}>()

const resourceKey = (item: ResourceSummary) => {
  return item.config_key || `${item.resource_type}|${item.source_tenant_id}|${item.id}`
}

const isChecked = (item: ResourceSummary) => props.modelValue.includes(resourceKey(item))

const toggle = (item: ResourceSummary, checked: boolean) => {
  const key = resourceKey(item)
  const next = new Set(props.modelValue)
  if (checked) next.add(key)
  else next.delete(key)
  emit('update:modelValue', Array.from(next))
}

const badgeText = (label: string) => label.slice(0, 1)

const resourceDescription = (item: ResourceSummary, groupLabel: string) => {
  if (item.kind) return `${groupLabel} · ${item.kind}`
  return groupLabel
}
</script>

<style lang="less" scoped>
.resource-selector {
  width: 100%;
}

.resource-groups {
  display: flex;
  flex-direction: column;
  gap: 18px;
}

.resource-group__header {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-bottom: 8px;
  font-size: 13px;
  font-weight: 600;
  color: var(--td-text-color-secondary);
}

.resource-list {
  display: flex;
  flex-direction: column;
  gap: 8px;
}

.resource-row {
  display: grid;
  grid-template-columns: 24px 36px minmax(0, 1fr);
  align-items: flex-start;
  gap: 12px;
  cursor: pointer;
  min-width: 0;

  :deep(.t-checkbox) {
    margin-top: 17px;
  }

  :deep(.setting-card) {
    padding: 14px 16px;
    min-height: 72px;
  }

  :deep(.setting-card__desc) {
    -webkit-line-clamp: 1;
  }
}

.resource-row__badge {
  width: 36px;
  height: 36px;
  border-radius: 9px;
  margin-top: 14px;
  display: flex;
  align-items: center;
  justify-content: center;
  flex-shrink: 0;
  font-size: 15px;
  font-weight: 600;
  background: rgba(0, 82, 217, 0.1);
  color: #0052D9;
}

.resource-row__card {
  min-width: 0;
}

.resource-row--active {
  :deep(.setting-card) {
    border-color: var(--td-brand-color-3, var(--td-brand-color));
    background: color-mix(in srgb, var(--td-brand-color) 5%, var(--td-bg-color-container));
  }
}

.resource-row--model .resource-row__badge {
  background: rgba(0, 82, 217, 0.1);
  color: #0052D9;
}

.resource-row--vector_store .resource-row__badge {
  background: rgba(98, 53, 187, 0.1);
  color: #6235BB;
}

.resource-row--parser_engine .resource-row__badge {
  background: rgba(17, 128, 83, 0.1);
  color: #118053;
}

.resource-row--storage_engine .resource-row__badge {
  background: rgba(184, 92, 0, 0.1);
  color: #B85C00;
}

.resource-row--web_search .resource-row__badge {
  background: rgba(41, 50, 225, 0.12);
  color: #2932E1;
}

.resource-row--mcp_service .resource-row__badge {
  background: rgba(70, 70, 70, 0.12);
  color: #464646;
}

.selector-state {
  min-height: 80px;
  border: 1px dashed var(--td-component-stroke);
  border-radius: 8px;
  color: var(--td-text-color-placeholder);
  display: flex;
  align-items: center;
  justify-content: center;
}
</style>
