<template>
  <t-popup
    v-if="enabled"
    trigger="hover"
    placement="bottom"
    destroy-on-close
    :on-visible-change="handleVisibleChange"
  >
    <div class="knowledge-task-overview-trigger" @click.stop>
      <slot />
    </div>
    <template #content>
      <div class="knowledge-task-overview" @click.stop>
        <div class="knowledge-task-overview__title">
          {{ t('knowledgeFolders.taskOverview') }}
        </div>
        <div v-if="loading && !stats" class="knowledge-task-overview__loading">
          <t-loading size="small" />
        </div>
        <template v-else-if="stats">
          <div class="knowledge-task-overview__item">
            <span>{{ t('knowledgeFolders.parsePending') }}</span>
            <strong>{{ stats.parse_pending_count }}</strong>
          </div>
          <div class="knowledge-task-overview__item">
            <span>{{ t('knowledgeFolders.parseRunning') }}</span>
            <strong>{{ stats.parse_running_count }}</strong>
          </div>
          <div class="knowledge-task-overview__item">
            <span>{{ t('knowledgeFolders.derivativeTasks') }}</span>
            <strong>{{ stats.enrichment_pending_task_count + stats.wiki_pending_task_count }}</strong>
          </div>
          <div class="knowledge-task-overview__item is-warning">
            <span>{{ t('knowledgeFolders.abnormalDocuments') }}</span>
            <strong>{{ stats.abnormal_document_count }}</strong>
          </div>
          <div class="knowledge-task-overview__item is-danger">
            <span>{{ t('knowledgeFolders.failedDocuments') }}</span>
            <strong>{{ stats.failed_document_count }}</strong>
          </div>
        </template>
        <button v-else type="button" class="knowledge-task-overview__retry" @click="loadStats">
          {{ t('knowledgeFolders.taskOverviewLoadFailed') }}
        </button>
      </div>
    </template>
  </t-popup>
  <t-tooltip v-else :content="fallbackContent" placement="top">
    <slot />
  </t-tooltip>
</template>

<script setup lang="ts">
import { onBeforeUnmount, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { getKnowledgeBaseTaskStats } from '../api'
import type { KnowledgeBaseTaskStats } from '../types'

const props = withDefaults(defineProps<{
  knowledgeBaseId: string
  enabled?: boolean
  fallbackContent?: string
}>(), {
  enabled: true,
  fallbackContent: '',
})

const { t } = useI18n()
const stats = ref<KnowledgeBaseTaskStats | null>(null)
const loading = ref(false)
let visible = false
let requestGeneration = 0
let refreshTimer: ReturnType<typeof setTimeout> | null = null

const clearRefreshTimer = () => {
  if (refreshTimer) {
    clearTimeout(refreshTimer)
    refreshTimer = null
  }
}

const hasActiveTasks = (value: KnowledgeBaseTaskStats) =>
  value.parse_pending_count > 0
  || value.parse_running_count > 0
  || value.enrichment_pending_task_count > 0
  || value.wiki_pending_task_count > 0

const loadStats = async () => {
  if (!props.enabled || !props.knowledgeBaseId || loading.value) return
  const generation = ++requestGeneration
  loading.value = true
  clearRefreshTimer()
  try {
    const result: any = await getKnowledgeBaseTaskStats(props.knowledgeBaseId)
    if (generation !== requestGeneration) return
    stats.value = result?.data || null
    if (visible && stats.value && hasActiveTasks(stats.value)) {
      refreshTimer = setTimeout(loadStats, 2500)
    }
  } catch {
    if (generation === requestGeneration) stats.value = null
  } finally {
    if (generation === requestGeneration) loading.value = false
  }
}

const handleVisibleChange = (nextVisible: boolean) => {
  visible = nextVisible
  if (nextVisible) {
    void loadStats()
  } else {
    requestGeneration += 1
    loading.value = false
    clearRefreshTimer()
  }
}

onBeforeUnmount(() => {
  requestGeneration += 1
  clearRefreshTimer()
})
</script>

<style scoped lang="less">
.knowledge-task-overview-trigger {
  display: inline-flex;
}

.knowledge-task-overview {
  width: 210px;
  padding: 10px 12px;
}

.knowledge-task-overview__title {
  margin-bottom: 7px;
  color: var(--td-text-color-primary);
  font-size: 13px;
  font-weight: 600;
}

.knowledge-task-overview__loading {
  display: flex;
  min-height: 104px;
  align-items: center;
  justify-content: center;
}

.knowledge-task-overview__item {
  display: flex;
  min-height: 26px;
  align-items: center;
  justify-content: space-between;
  color: var(--td-text-color-secondary);
  font-size: 12px;

  strong {
    color: var(--td-text-color-primary);
    font-variant-numeric: tabular-nums;
  }

  &.is-danger strong {
    color: var(--td-error-color);
  }

  &.is-warning strong {
    color: var(--td-warning-color);
  }
}

.knowledge-task-overview__retry {
  width: 100%;
  min-height: 52px;
  border: 0;
  background: transparent;
  color: var(--td-brand-color);
  cursor: pointer;
  font-size: 12px;
}
</style>
