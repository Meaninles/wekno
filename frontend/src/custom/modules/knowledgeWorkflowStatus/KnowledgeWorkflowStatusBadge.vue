<script setup lang="ts">
import { computed, onUnmounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { getKnowledgeSpans } from '@/api/knowledge-base'
import {
  latestSpanGroupDetailStatus,
  resolveCoreIndexDetailStatus,
  resolveKnowledgeDetailStatus,
  resolveKnowledgeWorkflowFeatures,
  resolveKnowledgeWorkflowStatus,
  type KnowledgeDetailStatus,
  type KnowledgeWorkflowFeatureSource,
  type KnowledgeWorkflowStatus,
  type SpanNode,
} from './status'

type KnowledgeStatusItem = {
  id: string
  parse_status?: string
  summary_status?: string
  enrichment_status?: string
  wiki_status?: string
  error_message?: string
  wiki_error_message?: string
  metadata?: unknown
}

const props = defineProps<{
  knowledge: KnowledgeStatusItem
  featureSource?: KnowledgeWorkflowFeatureSource | null
  compact?: boolean
}>()

const { t } = useI18n()
const trace = ref<SpanNode>()
const traceError = ref('')
const traceLoading = ref(false)
const lastLoadedAt = ref(0)

const workflowStatus = computed(() => resolveKnowledgeWorkflowStatus(props.knowledge))
const detailVisible = ref(false)
let detailCloseTimer: ReturnType<typeof setTimeout> | undefined
const features = computed(() => (
  resolveKnowledgeWorkflowFeatures(props.featureSource, props.knowledge)
))

const statusPresentation = computed(() => {
  const status = workflowStatus.value
  switch (status) {
    case 'pending':
      return { label: t('knowledgeBase.statusPending'), theme: 'warning', icon: 'time' }
    case 'processing':
      return { label: t('knowledgeBase.statusProcessing'), theme: 'primary', icon: 'loading', spin: true }
    case 'cancelling':
      return { label: t('knowledgeWorkflowStatus.status.cancelling'), theme: 'warning', icon: 'loading', spin: true }
    case 'deleting':
      return { label: t('knowledgeWorkflowStatus.status.deleting'), theme: 'warning', icon: 'loading', spin: true }
    case 'completed':
      return { label: t('knowledgeBase.statusCompleted'), theme: 'success', icon: 'check-circle' }
    case 'failed':
      return { label: t('knowledgeBase.statusFailed'), theme: 'danger', icon: 'close-circle' }
    case 'cancelled':
      return { label: t('knowledgeBase.statusCancelled'), theme: 'warning', icon: 'close-circle' }
    case 'draft':
      return { label: t('knowledgeBase.statusDraft'), theme: 'warning', icon: 'file' }
    default:
      return { label: t('knowledgeWorkflowStatus.unknown'), theme: 'default', icon: 'help-circle' }
  }
})

function coreFallback(): KnowledgeDetailStatus {
  switch (String(props.knowledge.parse_status ?? '').toLowerCase()) {
    case 'pending':
      return 'pending'
    case 'processing':
      return 'processing'
    case 'finalizing':
    case 'completed':
      return 'completed'
    case 'failed':
      return 'failed'
    case 'cancelled':
      return 'cancelled'
    case 'cancelling':
      return 'cancelling'
    case 'deleting':
      return 'deleting'
    case 'draft':
      return 'not_started'
    default:
      return 'unknown'
  }
}

function spanStatus(
  matches: (name: string) => boolean,
  fallback: KnowledgeDetailStatus,
): KnowledgeDetailStatus {
  return latestSpanGroupDetailStatus(trace.value, matches) ?? fallback
}

const detailRows = computed(() => {
  const core = coreFallback()
  const parseStatus = props.knowledge.parse_status
  const sharedEnrichment = (enabled?: boolean): KnowledgeDetailStatus => {
    if (enabled === false) return 'disabled'
    return resolveKnowledgeDetailStatus(
      props.knowledge.enrichment_status,
      parseStatus,
      enabled,
    )
  }
  const rows = [
    {
      key: 'parse',
      label: t('knowledgeWorkflowStatus.stage.parse'),
      status: spanStatus(
        (name) => name === 'docreader' || name === 'chunking',
        core,
      ),
    },
    {
      key: 'embedding',
      label: t('knowledgeWorkflowStatus.stage.embedding'),
      status: spanStatus(
        (name) => name === 'embedding',
        resolveCoreIndexDetailStatus(parseStatus, features.value.embedding),
      ),
    },
  ]

  const multimodal = latestSpanGroupDetailStatus(
    trace.value,
    (name) => name === 'multimodal' || name.startsWith('multimodal.'),
  )
  rows.push({
    key: 'multimodal',
    label: t('knowledgeWorkflowStatus.stage.multimodal'),
    status: multimodal ?? resolveKnowledgeDetailStatus(
      undefined,
      parseStatus,
      features.value.multimodal,
    ),
  })

  rows.push(
    {
      key: 'summary',
      label: t('knowledgeWorkflowStatus.stage.summary'),
      status: spanStatus(
        (name) => name === 'postprocess.summary',
        resolveKnowledgeDetailStatus(
          props.knowledge.summary_status,
          parseStatus,
          features.value.summary,
        ),
      ),
    },
    {
      key: 'question',
      label: t('knowledgeWorkflowStatus.stage.question'),
      status: spanStatus(
        (name) => name === 'postprocess.question' || name.startsWith('postprocess.question.'),
        sharedEnrichment(features.value.question),
      ),
    },
    {
      key: 'graph',
      label: t('knowledgeWorkflowStatus.stage.graph'),
      status: spanStatus(
        (name) => name.startsWith('postprocess.graph'),
        sharedEnrichment(features.value.graph),
      ),
    },
    {
      key: 'wiki',
      label: t('knowledgeWorkflowStatus.stage.wiki'),
      status: spanStatus(
        (name) => name === 'postprocess.wiki' || name.startsWith('postprocess.wiki.'),
        resolveKnowledgeDetailStatus(
          props.knowledge.wiki_status,
          parseStatus,
          features.value.wiki,
        ),
      ),
    },
  )
  return rows
})

function detailLabel(status: KnowledgeDetailStatus): string {
  return t(`knowledgeWorkflowStatus.status.${status}`)
}

async function loadTrace() {
  if (!props.knowledge.id || traceLoading.value) return
  const isActive = workflowStatus.value === 'pending' || workflowStatus.value === 'processing'
  if (trace.value && (!isActive || Date.now() - lastLoadedAt.value < 5000)) return
  traceLoading.value = true
  traceError.value = ''
  try {
    const response: any = await getKnowledgeSpans(props.knowledge.id)
    trace.value = response?.success ? response.data?.trace : undefined
    traceError.value = String(
      response?.data?.last_error?.error_message ||
      props.knowledge.wiki_error_message ||
      props.knowledge.error_message ||
      '',
    )
    lastLoadedAt.value = Date.now()
  } catch {
    traceError.value = String(
      props.knowledge.wiki_error_message ||
      props.knowledge.error_message ||
      t('knowledgeWorkflowStatus.detailsUnavailable'),
    )
  } finally {
    traceLoading.value = false
  }
}

function onVisibleChange(visible: boolean) {
  if (visible) void loadTrace()
}

function openWorkflowDetail() {
  if (detailCloseTimer) {
    clearTimeout(detailCloseTimer)
    detailCloseTimer = undefined
  }
  if (detailVisible.value) return
  detailVisible.value = true
  onVisibleChange(true)
}

function openWorkflowDetailFromTag(context?: { e?: MouseEvent }) {
  context?.e?.stopPropagation()
  openWorkflowDetail()
}

function scheduleWorkflowDetailClose() {
  if (detailCloseTimer) clearTimeout(detailCloseTimer)
  detailCloseTimer = setTimeout(() => {
    detailVisible.value = false
    detailCloseTimer = undefined
  }, 180)
}

onUnmounted(() => {
  if (detailCloseTimer) clearTimeout(detailCloseTimer)
})

</script>

<template>
  <t-popup
    v-model:visible="detailVisible"
    trigger="hover"
    placement="top-left"
    show-arrow
    destroy-on-close
    @update:visible="onVisibleChange"
  >
    <t-tag
      size="small"
      :theme="statusPresentation.theme as any"
      variant="light-outline"
      class="knowledge-workflow-badge"
      :class="{ compact }"
      :aria-label="`${t('knowledgeWorkflowStatus.title')}：${statusPresentation.label}`"
      :aria-expanded="detailVisible"
      tabindex="0"
      data-testid="knowledge-workflow-status"
      @mouseenter="openWorkflowDetail"
      @mouseleave="scheduleWorkflowDetailClose"
      @focus="openWorkflowDetail"
      @blur="scheduleWorkflowDetailClose"
      @click="openWorkflowDetailFromTag"
      @keydown.enter.prevent.stop="openWorkflowDetail"
      @keydown.space.prevent.stop="openWorkflowDetail"
    >
      <template #icon>
        <t-icon
          :name="statusPresentation.icon"
          :class="{ 'knowledge-workflow-badge__spin': statusPresentation.spin }"
        />
      </template>
      {{ statusPresentation.label }}
    </t-tag>
    <template #content>
      <div class="knowledge-workflow-detail" @click.stop
        @mouseenter="openWorkflowDetail" @mouseleave="scheduleWorkflowDetailClose">
        <div class="knowledge-workflow-detail__title">
          <span>{{ t('knowledgeWorkflowStatus.title') }}</span>
          <t-loading v-if="traceLoading" size="small" />
        </div>
        <div
          v-for="row in detailRows"
          :key="row.key"
          class="knowledge-workflow-detail__row"
          :data-stage="row.key"
        >
          <span class="knowledge-workflow-detail__label">{{ row.label }}</span>
          <span
            class="knowledge-workflow-detail__status"
            :class="`is-${row.status}`"
          >
            <span class="knowledge-workflow-detail__dot"></span>
            {{ detailLabel(row.status) }}
          </span>
        </div>
        <div v-if="traceError && workflowStatus === 'failed'" class="knowledge-workflow-detail__error">
          {{ traceError }}
        </div>
        <div class="knowledge-workflow-detail__hint">
          {{ t('knowledgeWorkflowStatus.hoverHint') }}
        </div>
      </div>
    </template>
  </t-popup>
</template>

<style scoped lang="less">
.knowledge-workflow-badge {
  flex: 0 0 auto;
  cursor: help;
  font-size: 11px;

  &.compact {
    font-size: 10px;
  }
}

.knowledge-workflow-badge__spin {
  animation: knowledge-workflow-spin 1s linear infinite;
}

.knowledge-workflow-detail {
  width: 252px;
  padding: 11px 12px 9px;
  color: var(--td-text-color-primary);
}

.knowledge-workflow-detail__title {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-bottom: 7px;
  font-size: 13px;
  font-weight: 600;
}

.knowledge-workflow-detail__row {
  display: flex;
  align-items: center;
  justify-content: space-between;
  min-height: 25px;
  gap: 16px;
  font-size: 12px;
}

.knowledge-workflow-detail__label {
  color: var(--td-text-color-secondary);
}

.knowledge-workflow-detail__status {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  white-space: nowrap;

  &.is-completed { color: var(--td-success-color); }
  &.is-failed { color: var(--td-error-color); }
  &.is-processing { color: var(--td-brand-color); }
  &.is-pending,
  &.is-cancelling,
  &.is-deleting,
  &.is-cancelled { color: var(--td-warning-color); }
  &.is-blocked { color: var(--td-error-color); }
  &.is-not_started { color: var(--td-warning-color); }
  &.is-disabled,
  &.is-not_applicable,
  &.is-skipped,
  &.is-unknown { color: var(--td-text-color-placeholder); }
}

.knowledge-workflow-detail__dot {
  width: 6px;
  height: 6px;
  border-radius: 50%;
  background: currentColor;
}

.knowledge-workflow-detail__error {
  display: -webkit-box;
  overflow: hidden;
  margin-top: 7px;
  padding-top: 7px;
  border-top: 1px solid var(--td-component-stroke);
  color: var(--td-error-color);
  font-size: 11px;
  line-height: 1.5;
  -webkit-box-orient: vertical;
  -webkit-line-clamp: 3;
}

.knowledge-workflow-detail__hint {
  margin-top: 7px;
  color: var(--td-text-color-placeholder);
  font-size: 10px;
}

@keyframes knowledge-workflow-spin {
  to { transform: rotate(360deg); }
}
</style>
