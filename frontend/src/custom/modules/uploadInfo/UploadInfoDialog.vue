<template>
  <t-dialog
    v-model:visible="visible"
    :header="t('knowledgeBase.uploadInfoTitle')"
    width="760px"
    :footer="false"
    attach="body"
    destroy-on-close
    dialog-class-name="upload-info-dialog"
  >
    <div class="upload-info-panel">
      <template v-if="activeBatch">
        <div class="upload-info-summary">
          <div class="upload-info-summary__main">
            <div class="upload-info-summary__title">{{ batchTitle(activeBatch) }}</div>
            <div class="upload-info-summary__meta">
              {{ t('knowledgeBase.uploadInfoStartedAt') }} {{ formatTime(activeBatch.createdAt) }}
              <span v-if="activeBatch.finishedAt">
                · {{ t('knowledgeBase.uploadInfoElapsed') }} {{ formatDuration(activeBatch.createdAt, activeBatch.finishedAt) }}
              </span>
            </div>
          </div>
          <div class="upload-info-summary__stats">
            <span>{{ t('knowledgeBase.uploadInfoTotal', { count: summary.total }) }}</span>
            <span>{{ t('knowledgeBase.uploadInfoSuccess', { count: summary.success }) }}</span>
            <span :class="{ danger: summary.failed > 0 }">
              {{ t('knowledgeBase.uploadInfoFailed', { count: summary.failed }) }}
            </span>
          </div>
        </div>

        <div class="upload-info-total-progress" aria-hidden="true">
          <div class="upload-info-progress__bar" :style="{ width: `${summary.percent}%` }" />
        </div>

        <div v-if="batches.length > 1" class="upload-info-batches">
          <button
            v-for="batch in batches"
            :key="batch.id"
            type="button"
            class="upload-info-batch"
            :class="{ active: batch.id === activeBatch.id }"
            @click="uploadInfo.selectBatch(batch.id)"
          >
            <span class="upload-info-batch__name">{{ batchTitle(batch) }}</span>
            <span class="upload-info-batch__status">{{ batchStatusText(batch) }}</span>
          </button>
        </div>

        <div class="upload-info-toolbar">
          <span class="upload-info-toolbar__hint">{{ activeStatusText }}</span>
          <div class="upload-info-toolbar__actions">
            <t-button
              theme="default"
              variant="text"
              size="small"
              :disabled="!hasCompletedItems"
              @click="uploadInfo.clearCompletedItems()"
            >
              {{ t('knowledgeBase.uploadInfoClearCompleted') }}
            </t-button>
            <t-button
              theme="default"
              variant="text"
              size="small"
              :disabled="!hasAnyItems"
              @click="uploadInfo.clearAllBatches()"
            >
              {{ t('knowledgeBase.uploadInfoClearAll') }}
            </t-button>
          </div>
        </div>

        <div class="upload-info-list">
          <div
            v-for="item in activeBatch.items"
            :key="item.id"
            class="upload-info-item"
            :class="`is-${item.status}`"
          >
            <div class="upload-info-item__icon">
              <t-icon :name="item.source === 'url' ? 'link' : 'file'" size="18px" />
            </div>
            <div class="upload-info-item__body">
              <div class="upload-info-item__top">
                <span class="upload-info-item__name" :title="item.name">{{ item.name }}</span>
                <span class="upload-info-item__status">
                  {{ itemStatusText(item) }}
                  <t-tooltip
                    v-if="item.status === 'failed' && item.error"
                    :content="item.error"
                    placement="top"
                  >
                    <t-icon name="error-circle" size="14px" class="upload-info-item__error-icon" />
                  </t-tooltip>
                </span>
              </div>
              <div class="upload-info-item__meta">
                <span v-if="item.source === 'file'">
                  {{ formatFileSize(item.size || item.total || 0) }}
                </span>
                <span v-else>{{ t('knowledgeBase.sourceUrl') }}</span>
                <span v-if="item.startedAt">
                  · {{ t('knowledgeBase.uploadInfoElapsed') }} {{ formatDuration(item.startedAt, item.finishedAt || nowTick) }}
                </span>
                <span v-if="item.message"> · {{ item.message }}</span>
              </div>
              <div class="upload-info-progress">
                <div class="upload-info-progress__bar" :style="{ width: `${item.percent}%` }" />
              </div>
            </div>
            <div class="upload-info-item__percent">{{ item.percent }}%</div>
          </div>
        </div>
      </template>

      <div v-else class="upload-info-empty">
        <t-icon name="queue" size="28px" />
        <div>{{ t('knowledgeBase.uploadInfoEmpty') }}</div>
      </div>
    </div>
  </t-dialog>
</template>

<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { formatFileSize } from '@/utils/files'
import { useUploadInfoStore, type UploadInfoBatch, type UploadInfoItem } from './store'

const { t } = useI18n()
const uploadInfo = useUploadInfoStore()
const nowTick = ref(Date.now())
let tickTimer: ReturnType<typeof setInterval> | null = null

const visible = computed({
  get: () => uploadInfo.state.visible,
  set: (value: boolean) => {
    if (value) uploadInfo.openPanel()
    else uploadInfo.closePanel()
  },
})

const batches = computed(() => uploadInfo.state.batches)
const activeBatch = computed(() => uploadInfo.activeBatch.value)
const summary = computed(() => uploadInfo.latestSummary.value)
const hasCompletedItems = computed(() => uploadInfo.completedItemCount.value > 0)
const hasAnyItems = computed(() => uploadInfo.totalItemCount.value > 0)

const activeStatusText = computed(() => {
  if (!activeBatch.value) return ''
  if (activeBatch.value.finishedAt) return batchStatusText(activeBatch.value)
  if (summary.value.running > 0) {
    return t('knowledgeBase.uploadInfoRunning', { count: summary.value.running })
  }
  if (summary.value.queued > 0) {
    return t('knowledgeBase.uploadInfoQueued', { count: summary.value.queued })
  }
  return t('knowledgeBase.uploadInfoWaiting')
})

function batchTitle(batch: UploadInfoBatch) {
  return t('knowledgeBase.uploadInfoBatchTitle', { count: batch.items.length })
}

function batchStatusText(batch: UploadInfoBatch) {
  const total = batch.items.length
  const success = batch.items.filter((item) => item.status === 'success').length
  const failed = batch.items.filter((item) => item.status === 'failed').length
  const active = batch.items.filter((item) => item.status !== 'success' && item.status !== 'failed').length

  if (active > 0) return t('knowledgeBase.uploadInfoBatchRunning', { done: success + failed, total })
  if (failed === 0) return t('knowledgeBase.uploadInfoBatchSuccess', { total })
  if (success === 0) return t('knowledgeBase.uploadInfoBatchFailed', { total })
  return t('knowledgeBase.uploadInfoBatchPartial', { success, failed })
}

function itemStatusText(item: UploadInfoItem) {
  if (item.status === 'queued') return t('knowledgeBase.uploadInfoStatusQueued')
  if (item.status === 'uploading') return t('knowledgeBase.uploadInfoStatusUploading')
  if (item.status === 'processing') return t('knowledgeBase.uploadInfoStatusProcessing')
  if (item.status === 'success') return t('knowledgeBase.uploadInfoStatusSuccess')
  return t('knowledgeBase.uploadInfoStatusFailed')
}

function formatTime(value: number) {
  const date = new Date(value)
  return `${String(date.getHours()).padStart(2, '0')}:${String(date.getMinutes()).padStart(2, '0')}:${String(date.getSeconds()).padStart(2, '0')}`
}

function formatDuration(start?: number, end?: number) {
  if (!start || !end || end < start) return '0s'
  const seconds = Math.max(0, Math.round((end - start) / 1000))
  const minutes = Math.floor(seconds / 60)
  const remainSeconds = seconds % 60
  if (minutes <= 0) return `${remainSeconds}s`
  return `${minutes}m ${remainSeconds}s`
}

onMounted(() => {
  tickTimer = setInterval(() => {
    nowTick.value = Date.now()
  }, 1000)
})

onUnmounted(() => {
  if (tickTimer) clearInterval(tickTimer)
})
</script>

<style lang="less" scoped>
.upload-info-panel {
  min-height: 260px;
}

.upload-info-summary {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 16px;
  padding-bottom: 12px;
  border-bottom: 1px solid var(--td-component-stroke);
}

.upload-info-summary__main {
  min-width: 0;
}

.upload-info-summary__title {
  font-size: 16px;
  font-weight: 600;
  color: var(--td-text-color-primary);
}

.upload-info-summary__meta {
  margin-top: 4px;
  font-size: 12px;
  color: var(--td-text-color-placeholder);
}

.upload-info-summary__stats {
  display: flex;
  flex-shrink: 0;
  align-items: center;
  gap: 10px;
  font-size: 12px;
  color: var(--td-text-color-secondary);

  .danger {
    color: var(--td-error-color);
  }
}

.upload-info-total-progress,
.upload-info-progress {
  position: relative;
  height: 4px;
  overflow: hidden;
  border-radius: 999px;
  background: var(--td-bg-color-component);
}

.upload-info-total-progress {
  margin-top: 12px;
}

.upload-info-progress__bar {
  height: 100%;
  border-radius: inherit;
  background: var(--td-brand-color);
  transition: width 0.2s ease;
}

.upload-info-batches {
  display: flex;
  gap: 8px;
  margin-top: 14px;
  padding-bottom: 2px;
  overflow-x: auto;
}

.upload-info-batch {
  display: inline-flex;
  align-items: center;
  gap: 8px;
  max-width: 220px;
  height: 28px;
  padding: 0 10px;
  border: 1px solid var(--td-component-stroke);
  border-radius: 6px;
  color: var(--td-text-color-secondary);
  background: var(--td-bg-color-container);
  cursor: pointer;

  &.active {
    border-color: var(--td-brand-color);
    color: var(--td-brand-color);
    background: var(--td-brand-color-light);
  }
}

.upload-info-batch__name,
.upload-info-batch__status {
  overflow: hidden;
  font-size: 12px;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.upload-info-batch__status {
  color: var(--td-text-color-placeholder);
}

.upload-info-toolbar {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  height: 40px;
}

.upload-info-toolbar__hint {
  font-size: 12px;
  color: var(--td-text-color-secondary);
}

.upload-info-toolbar__actions {
  display: inline-flex;
  flex-shrink: 0;
  align-items: center;
  gap: 4px;
}

.upload-info-list {
  max-height: 430px;
  overflow-y: auto;
  border: 1px solid var(--td-component-stroke);
  border-radius: 8px;
}

.upload-info-item {
  display: grid;
  grid-template-columns: 32px minmax(0, 1fr) 48px;
  gap: 10px;
  align-items: center;
  padding: 12px;
  border-bottom: 1px solid var(--td-component-stroke);

  &:last-child {
    border-bottom: none;
  }

  &.is-failed {
    .upload-info-progress__bar {
      background: var(--td-error-color);
    }

    .upload-info-item__status,
    .upload-info-item__percent {
      color: var(--td-error-color);
    }
  }

  &.is-success {
    .upload-info-item__status,
    .upload-info-item__percent {
      color: var(--td-brand-color);
    }
  }
}

.upload-info-item__icon {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 32px;
  height: 32px;
  border-radius: 6px;
  color: var(--td-brand-color);
  background: var(--td-brand-color-light);
}

.upload-info-item__body {
  min-width: 0;
}

.upload-info-item__top {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
}

.upload-info-item__name {
  min-width: 0;
  overflow: hidden;
  font-size: 13px;
  font-weight: 500;
  color: var(--td-text-color-primary);
  text-overflow: ellipsis;
  white-space: nowrap;
}

.upload-info-item__status {
  display: inline-flex;
  flex-shrink: 0;
  align-items: center;
  gap: 4px;
  font-size: 12px;
  color: var(--td-text-color-secondary);
}

.upload-info-item__meta {
  margin: 3px 0 7px;
  overflow: hidden;
  font-size: 12px;
  color: var(--td-text-color-placeholder);
  text-overflow: ellipsis;
  white-space: nowrap;
}

.upload-info-item__error-icon {
  display: block;
  flex-shrink: 0;
  color: var(--td-error-color);
  cursor: default;
  line-height: 1;
}

.upload-info-item__percent {
  justify-self: end;
  font-size: 12px;
  font-variant-numeric: tabular-nums;
  color: var(--td-text-color-secondary);
}

.upload-info-empty {
  display: flex;
  min-height: 260px;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  gap: 12px;
  color: var(--td-text-color-placeholder);
  font-size: 13px;
}
</style>
