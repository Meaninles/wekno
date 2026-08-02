<template>
  <div v-if="summaryHtml" class="completed-simple-run-summary" role="status" v-html="summaryHtml" />
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import {
  formatCompletedRunDuration,
  isSimpleCompletedConversation,
} from './retrievalSummary'

const props = defineProps<{
  message?: Record<string, any>
}>()

const { t } = useI18n()

const summaryHtml = computed(() => {
  if (!isSimpleCompletedConversation(props.message)) return ''
  const duration = formatCompletedRunDuration(Number(props.message?.agent_duration_ms) || 0)
  return duration ? t('agent.durationSuffix', { duration }) : ''
})
</script>

<style scoped lang="less">
.completed-simple-run-summary {
  width: fit-content;
  margin: 0 0 16px;
  color: var(--td-text-color-placeholder);
  font-size: 13px;
  line-height: 22px;

  :deep(strong) {
    color: var(--td-text-color-secondary);
    font-weight: 600;
  }
}
</style>
