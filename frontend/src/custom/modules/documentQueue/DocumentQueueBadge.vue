<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { documentQueueBadgeView } from './presentation'
import type { DocumentQueueItemStatus } from './types'

const props = defineProps<{
  status?: DocumentQueueItemStatus | null
  waitingTotal?: number | null
}>()

const { t } = useI18n()
const view = computed(() => documentQueueBadgeView(props.status, props.waitingTotal))
const label = computed(() => view.value.total === null
  ? t('knowledgeBase.documentQueuePositionOnly', { position: view.value.position })
  : t('knowledgeBase.documentQueuePosition', {
      position: view.value.position,
      total: view.value.total,
    }))
const tooltip = computed(() => view.value.total === null
  ? t('knowledgeBase.documentQueueTooltipPositionOnly', { position: view.value.position })
  : t('knowledgeBase.documentQueueTooltip', {
      position: view.value.position,
      total: view.value.total,
    }))
</script>

<template>
  <t-tooltip v-if="view.visible" :content="tooltip" placement="top">
    <span
      class="document-queue-badge"
      :aria-label="tooltip"
      data-testid="document-queue-badge"
    >
      <t-icon name="queue" size="11px" />
      <span>{{ label }}</span>
    </span>
  </t-tooltip>
</template>

<style scoped lang="less">
.document-queue-badge {
  display: inline-flex;
  flex: 0 0 auto;
  align-items: center;
  gap: 3px;
  height: 18px;
  padding: 0 6px;
  border: 1px solid color-mix(in srgb, var(--td-brand-color) 24%, transparent);
  border-radius: 999px;
  color: var(--td-brand-color);
  background: color-mix(in srgb, var(--td-brand-color) 7%, transparent);
  font-size: 10px;
  font-weight: 500;
  font-variant-numeric: tabular-nums;
  line-height: 16px;
  white-space: nowrap;
}
</style>
