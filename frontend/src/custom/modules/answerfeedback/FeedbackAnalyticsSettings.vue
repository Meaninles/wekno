<template>
  <div class="feedback-analytics">
    <div class="section-header">
      <div>
        <h2>反馈数据</h2>
        <p class="section-description">
          {{ isSystemAdmin
            ? '查看全平台反馈，并按用户、智能体或知识库筛选。'
            : '查看当前租户内所有成员资源，以及拥有管理员权限共享资源下的反馈。' }}
        </p>
      </div>
      <t-button variant="outline" size="small" :loading="loading" @click="loadAll">
        <template #icon><t-icon name="refresh" /></template>
        刷新
      </t-button>
    </div>

    <div class="summary-grid">
      <div class="summary-card summary-card--total"><span>反馈总数</span><strong>{{ summary.total }}</strong></div>
      <div class="summary-card summary-card--solved"><span>已解决</span><strong>{{ summary.solved }}</strong></div>
      <div class="summary-card summary-card--off-topic"><span>答非所问</span><strong>{{ summary.off_topic }}</strong></div>
      <div class="summary-card summary-card--inaccurate"><span>回答不准确</span><strong>{{ summary.inaccurate }}</strong></div>
      <div class="summary-card summary-card--unsolved"><span>未解决</span><strong>{{ summary.unsolved }}</strong></div>
    </div>

    <div class="filter-panel">
      <div class="filter-row">
        <t-input
          v-if="isSystemAdmin"
          v-model="userQuery"
          clearable
          class="filter-control filter-control--user"
          placeholder="搜索用户：用户名、姓名、用户 ID 或租户"
          @enter="applyFilters"
        >
          <template #prefix-icon><t-icon name="search" /></template>
        </t-input>
        <t-select
          v-model="selectedAgentKeys"
          multiple
          filterable
          clearable
          :filter="() => true"
          class="filter-control filter-control--resource"
          :options="agentOptions"
          placeholder="按智能体筛选"
          :loading="agentOptionsLoading"
          @search="searchAgents"
        />
        <t-select
          v-model="selectedKnowledgeBaseKeys"
          multiple
          filterable
          clearable
          :filter="() => true"
          class="filter-control filter-control--resource"
          :options="knowledgeBaseOptions"
          placeholder="按知识库筛选"
          :loading="knowledgeBaseOptionsLoading"
          @search="searchKnowledgeBases"
        />
        <t-select v-model="feedbackFilter" clearable class="filter-control filter-control--feedback" :options="feedbackOptions" placeholder="反馈类型" />
        <t-select v-model="channelFilter" clearable class="filter-control filter-control--channel" :options="channelOptions" placeholder="来源渠道" />
        <div class="filter-actions">
          <t-button theme="primary" :loading="loading" @click="applyFilters">
            <template #icon><t-icon name="search" /></template>
            查询
          </t-button>
          <t-button variant="text" @click="clearFilters">重置</t-button>
        </div>
      </div>
    </div>

    <t-alert v-if="errorMessage" theme="error" :message="errorMessage" class="analytics-alert" />

    <section class="feedback-list-card">
      <div class="feedback-list-card__header">
        <div><h3>反馈明细</h3><span>默认按反馈更新时间倒序展示，共 {{ total }} 条</span></div>
        <t-tag v-if="!isSystemAdmin" theme="primary" variant="light">按资源权限展示</t-tag>
        <t-tag v-else theme="warning" variant="light">全平台</t-tag>
      </div>
      <div class="data-table-shell data-table-shell--with-footer">
        <div class="feedback-list">
          <div v-if="loading && items.length === 0" class="list-loading"><t-loading text="正在读取反馈..." /></div>
          <article v-for="row in items" :key="row.id" class="feedback-row">
            <div class="feedback-row__top">
              <div class="feedback-row__time">{{ formatDate(row.updated_at || row.created_at) }}</div>
              <t-tag :theme="feedbackTheme(row.feedback)" variant="light" size="small">{{ feedbackLabel(row.feedback) }}</t-tag>
              <span class="feedback-row__channel">{{ channelLabel(row.channel) }}</span>
              <t-button size="small" variant="text" theme="primary" @click="openDetail(row)">查看问答</t-button>
            </div>
            <div class="feedback-row__meta">
              <div class="feedback-row__field">
                <span class="field-label">用户</span>
                <strong>{{ row.user_name || '外部用户' }}</strong>
                <small>{{ row.user_id || '未记录用户 ID' }}<template v-if="row.tenant_name"> · {{ row.tenant_name }}</template></small>
              </div>
              <div class="feedback-row__field">
                <span class="field-label">智能体</span>
                <strong :title="row.agent?.name || '未记录'">{{ row.agent?.name || '未记录' }}</strong>
                <small v-if="row.agent?.is_deleted" class="deleted-mark">已删除</small>
              </div>
              <div class="feedback-row__field">
                <span class="field-label">知识库</span>
                <div v-if="row.knowledge_bases?.length" class="resource-list">
                  <span v-for="resource in row.knowledge_bases" :key="resource.id" class="resource-chip" :title="resource.name">
                    {{ resource.name }}<em v-if="resource.is_deleted">（已删除）</em>
                  </span>
                </div>
                <small v-else class="muted">未记录</small>
              </div>
            </div>
            <div class="feedback-row__question">
              <span class="field-label">问题</span>
              <span v-if="row.question" :title="row.question">{{ questionPreview(row.question, 220) }}</span>
              <span v-else class="muted">未采集问题</span>
            </div>
          </article>
          <div v-if="!loading && items.length === 0" class="empty-state">暂无符合条件的反馈数据</div>
        </div>
        <div v-if="total > 0" class="data-table-shell__pager">
          <t-pagination
            v-model="page"
            v-model:page-size="pageSize"
            :total="total"
            size="small"
            show-jumper
            show-page-number
            show-page-size
            :page-size-options="PAGE_SIZE_OPTIONS"
            @change="onPageChange"
          />
        </div>
      </div>
    </section>

    <t-dialog v-model:visible="detailVisible" header="反馈对应的问答与会话" width="900px" :footer="false" destroy-on-close>
      <div v-if="detail" class="detail-content">
        <div class="detail-meta">
          <t-tag :theme="feedbackTheme(detail.feedback.feedback)" variant="light">{{ feedbackLabel(detail.feedback.feedback) }}</t-tag>
          <span>{{ formatDate(detail.feedback.updated_at || detail.feedback.created_at) }}</span>
          <span>用户：{{ detail.feedback.user_name || detail.feedback.user_id || '外部用户' }}</span>
          <span>渠道：{{ channelLabel(detail.feedback.channel) }}</span>
        </div>
        <div class="detail-resource-line">
          <span>智能体：{{ detail.feedback.agent?.name || '未记录' }}</span>
          <span>知识库：{{ resourceNames(detail.feedback.knowledge_bases) }}</span>
          <span>会话：{{ detail.feedback.session_title || detail.feedback.session_id }}</span>
        </div>

        <div class="progressive-step progressive-step--question">
          <div class="progressive-step__label">问题</div>
          <div :class="['progressive-step__content', { 'is-expanded': questionExpanded }]">
            {{ questionExpanded ? (detail.feedback.question || '未采集问题内容') : questionPreview(detail.feedback.question || '未采集问题内容', 320) }}
          </div>
          <t-button v-if="(detail.feedback.question || '').length > 320" variant="text" size="small" @click="questionExpanded = !questionExpanded">
            {{ questionExpanded ? '收起问题' : '展开完整问题' }}
          </t-button>
        </div>

        <div class="progressive-actions">
          <t-button v-if="!showAnswer" theme="primary" variant="outline" @click="showAnswer = true">查看回答</t-button>
        </div>

        <div ref="detailMarkdownRoot" class="detail-render-surface" @click.capture="handleSourceCitationClick">
          <div v-if="showAnswer" class="progressive-step progressive-step--answer">
            <SourceReferenceHub
              v-if="detail.feedback.knowledge_references?.length"
              ref="sourceReferenceHub"
              :session="answerCitationSession"
              :content="detail.feedback.answer"
              embedded-mode
            />
            <div class="progressive-step__label">回答</div>
            <div class="detail-markdown progressive-step__content--answer" v-html="renderedAnswerHtml" />
            <div class="progressive-actions progressive-actions--left">
              <t-button v-if="!showConversation" theme="primary" @click="expandConversation">展开会话</t-button>
            </div>
          </div>

          <div v-if="showConversation" class="conversation-block">
            <div class="conversation-block__header">
              <div><h4>所在会话</h4><span>从该回答附近开始，向上滚动加载更早消息</span></div>
              <span>{{ conversationItems.length }} / {{ conversationTotal }}</span>
            </div>
            <div ref="conversationScroll" class="conversation-list" @scroll="handleConversationScroll">
              <div v-if="conversationLoading" class="conversation-loading"><t-loading size="small" text="正在加载更早消息..." /></div>
              <button v-if="hasEarlier && !conversationLoading" type="button" class="load-earlier" @click="loadEarlier">
                加载更早消息
              </button>
              <div v-for="message in conversationItems" :key="message.id" :class="['conversation-message', messageClass(message.role)]">
                <div class="conversation-message__head"><span>{{ roleLabel(message.role) }}</span><span>{{ formatDate(message.created_at) }}</span></div>
                <div class="conversation-message__body detail-markdown" v-html="renderMessageHtml(message)" />
              </div>
              <div v-if="!conversationLoading && conversationItems.length === 0" class="group-empty">暂无可展示的会话消息</div>
            </div>
          </div>
        </div>
      </div>
      <div v-else class="detail-loading"><t-loading text="正在读取问题..." /></div>
    </t-dialog>
    <ChatCitationFloat
      v-if="detailVisible"
      :float="citationFloat"
      :on-enter="cancelCitationClose"
      :on-leave="scheduleCitationClose"
    />
  </div>
</template>

<script setup lang="ts">
import { computed, nextTick, onMounted, reactive, ref, watch } from 'vue'
import { useRouter } from 'vue-router'
import { MessagePlugin } from 'tdesign-vue-next'
import { useAuthStore } from '@/stores/auth'
import SourceReferenceHub from '@/views/chat/components/SourceReferenceHub.vue'
import ChatCitationFloat from '@/components/ChatCitationFloat.vue'
import { useChatCitationPopover } from '@/composables/useChatCitationPopover'
import { createChatMarkdownRenderer, renderChatMarkdown } from '@/utils/chatMarkdownRenderer'
import {
  createMermaidCodeRenderer,
  enhanceMarkdownContainer,
  ensureMermaidInitialized,
} from '@/utils/mermaidShared'
import { refreshMarkdownEnhancements } from '@/utils/markdownEnhancements'
import { hydrateProtectedFileImages, safeMarkdownToHTML, sanitizeMarkdownHTML, createSafeImage, isValidImageURL } from '@/utils/security'
import type { SourceReference } from '@/utils/sourceReferences'
import {
  getFeedbackAnalyticsDetail,
  listFeedbackAnalytics,
  listFeedbackAnalyticsOptions,
  type AnswerFeedbackValue,
  type FeedbackAnalyticsDetail,
  type FeedbackAnalyticsItem,
  type FeedbackAnalyticsResource,
  type FeedbackAnalyticsResourceOption,
} from './api'

type SelectOption = { value: string; label: string }
type ConversationMessage = NonNullable<FeedbackAnalyticsDetail['conversation']['items']>[number]

const PAGE_SIZE_OPTIONS = [10, 20, 50, 100]
const CONVERSATION_PAGE_SIZE = 30
const authStore = useAuthStore()
const router = useRouter()
const isSystemAdmin = computed(() => authStore.isSystemAdmin)

ensureMermaidInitialized()

const loading = ref(false)
const errorMessage = ref('')
const items = ref<FeedbackAnalyticsItem[]>([])
const total = ref(0)
const page = ref(1)
const pageSize = ref(20)
const summary = reactive({ total: 0, solved: 0, off_topic: 0, inaccurate: 0, unsolved: 0 })

const userQuery = ref('')
const selectedAgentKeys = ref<string[]>([])
const selectedKnowledgeBaseKeys = ref<string[]>([])
const feedbackFilter = ref('')
const channelFilter = ref('')
const agentOptions = ref<SelectOption[]>([])
const knowledgeBaseOptions = ref<SelectOption[]>([])
const agentOptionsLoading = ref(false)
const knowledgeBaseOptionsLoading = ref(false)
let agentSearchSequence = 0
let knowledgeBaseSearchSequence = 0
const detailVisible = ref(false)
const detail = ref<FeedbackAnalyticsDetail | null>(null)
const questionExpanded = ref(false)
const showAnswer = ref(false)
const showConversation = ref(false)
const conversationItems = ref<ConversationMessage[]>([])
const conversationTotal = ref(0)
const conversationPage = ref(0)
const conversationLoading = ref(false)
const conversationScroll = ref<HTMLElement | null>(null)
const detailMarkdownRoot = ref<HTMLElement | null>(null)
const sourceReferenceHub = ref<{ activateByElement?: (element: HTMLElement) => boolean } | null>(null)

const feedbackOptions: SelectOption[] = [
  { value: 'solved', label: '已解决' },
  { value: 'off_topic', label: '答非所问' },
  { value: 'inaccurate', label: '回答不准确' },
  { value: 'unsolved', label: '未解决' },
]
const channelOptions: SelectOption[] = [
  { value: 'web', label: '网页端' },
  { value: 'wecom_bot', label: '企业微信' },
  { value: 'im', label: 'IM' },
]

const markdownRenderer = createChatMarkdownRenderer({
  codeRenderer: createMermaidCodeRenderer('mermaid-feedback-detail'),
  imageRenderer: ({ href, title, text }) => createSafeImage(href, text || '', title || ''),
  invalidImageHtml: () => '<p>图片链接无效</p>',
  isValidImageUrl: isValidImageURL,
})

const answerReferences = computed<SourceReference[]>(() => detail.value?.feedback.knowledge_references || [])
const detailReferences = computed<SourceReference[]>(() => {
  const references = [...answerReferences.value]
  for (const message of conversationItems.value) {
    if (message.knowledge_references?.length) references.push(...message.knowledge_references)
  }
  return references
})
const answerCitationSession = computed(() => ({
  is_completed: true,
  knowledge_references: answerReferences.value,
}))
const { float: citationFloat, rebind: rebindCitations, cancelClose: cancelCitationClose, scheduleClose: scheduleCitationClose } =
  useChatCitationPopover(detailMarkdownRoot, {
    getKnowledgeReferences: () => detailReferences.value,
    sessionId: () => detail.value?.feedback.session_id,
  })

const renderedAnswerHtml = computed(() => renderMarkdown(detail.value?.feedback.answer || '', answerReferences.value))

function renderMarkdown(content: unknown, references: SourceReference[] = []) {
  return renderChatMarkdown(content, {
    renderer: markdownRenderer,
    escapeMarkdown: safeMarkdownToHTML,
    sanitizeHtml: sanitizeMarkdownHTML,
    knowledgeReferences: references,
  })
}

function renderMessageHtml(message: ConversationMessage) {
  return renderMarkdown(message.content || '（空消息）', message.knowledge_references || [])
}

function buildQuery() {
  return {
    page: page.value,
    page_size: pageSize.value,
    ...(isSystemAdmin.value && userQuery.value.trim() ? { user_q: userQuery.value.trim() } : {}),
    ...(selectedAgentKeys.value.length ? { agent_keys: selectedAgentKeys.value } : {}),
    ...(selectedKnowledgeBaseKeys.value.length ? { knowledge_base_keys: selectedKnowledgeBaseKeys.value } : {}),
    ...(feedbackFilter.value ? { feedback: feedbackFilter.value as Exclude<AnswerFeedbackValue, ''> } : {}),
    ...(channelFilter.value ? { channel: channelFilter.value } : {}),
  }
}

async function loadData() {
  loading.value = true
  errorMessage.value = ''
  const query = buildQuery()
  try {
    const pageResponse = await listFeedbackAnalytics(query)
    const data = pageResponse.data
    items.value = data.items || []
    total.value = data.total || 0
    Object.assign(summary, data.summary || { total: 0, solved: 0, off_topic: 0, inaccurate: 0, unsolved: 0 })
  } catch (error: any) {
    errorMessage.value = error?.message || '反馈数据读取失败，请稍后重试'
  } finally {
    loading.value = false
  }
}

async function loadOptions(type: 'agents' | 'knowledge_bases', query = '') {
  const isAgents = type === 'agents'
  const sequence = isAgents ? ++agentSearchSequence : ++knowledgeBaseSearchSequence
  if (isAgents) agentOptionsLoading.value = true
  else knowledgeBaseOptionsLoading.value = true
  try {
    const response = await listFeedbackAnalyticsOptions(type, query)
    if ((isAgents ? agentSearchSequence : knowledgeBaseSearchSequence) !== sequence) return
    const next = (response.data || []).map((option: FeedbackAnalyticsResourceOption) => ({
      value: option.key,
      label: option.name + (option.is_deleted ? '（已删除）' : ''),
    }))
    if (isAgents) agentOptions.value = mergeOptions(agentOptions.value, next)
    else knowledgeBaseOptions.value = mergeOptions(knowledgeBaseOptions.value, next)
  } catch {
    // 选项读取失败不阻断明细数据。
  } finally {
    if (isAgents) agentOptionsLoading.value = false
    else knowledgeBaseOptionsLoading.value = false
  }
}

function mergeOptions(current: SelectOption[], next: SelectOption[]) {
  const map = new Map(current.map(option => [option.value, option]))
  next.forEach(option => map.set(option.value, option))
  return Array.from(map.values())
}

function searchAgents(query: string) { loadOptions('agents', query) }
function searchKnowledgeBases(query: string) { loadOptions('knowledge_bases', query) }

function loadAll() {
  loadData()
  loadOptions('agents')
  loadOptions('knowledge_bases')
}

function applyFilters() {
  page.value = 1
  loadData()
}

function clearFilters() {
  userQuery.value = ''
  selectedAgentKeys.value = []
  selectedKnowledgeBaseKeys.value = []
  feedbackFilter.value = ''
  channelFilter.value = ''
  page.value = 1
  loadData()
}

function onPageChange(nextPage: number, nextPageSize?: number) {
  page.value = nextPage
  if (nextPageSize && nextPageSize !== pageSize.value) {
    pageSize.value = nextPageSize
    page.value = 1
  }
  loadData()
}

async function openDetail(row: FeedbackAnalyticsItem) {
  detailVisible.value = true
  detail.value = null
  questionExpanded.value = false
  showAnswer.value = false
  showConversation.value = false
  conversationItems.value = []
  conversationTotal.value = 0
  conversationPage.value = 0
  try {
    const response = await getFeedbackAnalyticsDetail(row.id, 1, CONVERSATION_PAGE_SIZE, false)
    detail.value = response.data
  } catch (error: any) {
    detailVisible.value = false
    await MessagePlugin.error(error?.message || '问题详情读取失败')
  }
}

function handleSourceCitationClick(event: MouseEvent) {
  const target = event.target as HTMLElement | null
  const sourceEl = target?.closest?.('.citation-source') as HTMLElement | null
  if (!sourceEl) return

  event.preventDefault()
  event.stopPropagation()
  const isAnswerCitation = Boolean(sourceEl.closest('.progressive-step--answer'))
  if (isAnswerCitation && sourceReferenceHub.value?.activateByElement?.(sourceEl)) return

  const type = sourceEl.getAttribute('data-source-type') || ''
  if (type === 'web') {
    const url = sourceEl.getAttribute('data-url') || ''
    if (url) window.open(url, '_blank', 'noopener,noreferrer')
    return
  }
  if (type === 'wiki') {
    const kbId = sourceEl.getAttribute('data-kb-id') || ''
    const slug = sourceEl.getAttribute('data-slug') || ''
    if (kbId && slug) {
      openRouteInNewTab({
        path: '/platform/knowledge-bases/' + kbId,
        query: { tab: 'graph', slug },
      })
    }
    return
  }
  if (type === 'knowledge') {
    const kbId = sourceEl.getAttribute('data-kb-id') || ''
    const knowledgeId = sourceEl.getAttribute('data-knowledge-id') || ''
    const chunkId = sourceEl.getAttribute('data-chunk-id') || ''
    if (kbId) {
      openRouteInNewTab({
        path: '/platform/knowledge-bases/' + kbId,
        query: knowledgeId
          ? { knowledge_id: knowledgeId, chunk_id: chunkId || undefined }
          : {},
      })
    }
  }
}

function openRouteInNewTab(location: Parameters<typeof router.resolve>[0]) {
  const href = router.resolve(location).href
  window.open(new URL(href, window.location.origin).toString(), '_blank', 'noopener,noreferrer')
}

async function refreshDetailMarkdown() {
  await nextTick()
  const root = detailMarkdownRoot.value
  if (!root) return
  rebindCitations()
  refreshMarkdownEnhancements(root)
  await hydrateProtectedFileImages(root)
  await enhanceMarkdownContainer(root)
}

watch(
  [renderedAnswerHtml, detailReferences, showAnswer, showConversation],
  () => { void refreshDetailMarkdown() },
  { flush: 'post' },
)

async function expandConversation() {
  showConversation.value = true
  await nextTick()
  await loadConversationChunk(1)
}

async function loadEarlier() {
  if (!hasEarlier.value || conversationLoading.value) return
  await loadConversationChunk(conversationPage.value + 1)
}

async function loadConversationChunk(targetPage: number) {
  if (!detail.value || conversationLoading.value) return
  conversationLoading.value = true
  const scrollElement = conversationScroll.value
  const oldHeight = scrollElement?.scrollHeight || 0
  const oldTop = scrollElement?.scrollTop || 0
  try {
    const response = await getFeedbackAnalyticsDetail(detail.value.feedback.id, targetPage, CONVERSATION_PAGE_SIZE, true)
    const chunk = response.data.conversation
    conversationTotal.value = chunk.total
    conversationPage.value = chunk.page
    if (targetPage === 1) {
      conversationItems.value = chunk.items || []
    } else {
      const existing = new Set(conversationItems.value.map(message => message.id))
      conversationItems.value = [...(chunk.items || []).filter(message => !existing.has(message.id)), ...conversationItems.value]
    }
    await nextTick()
    if (targetPage > 1 && scrollElement) {
      scrollElement.scrollTop = scrollElement.scrollHeight - oldHeight + oldTop
    } else if (targetPage === 1 && scrollElement) {
      scrollElement.scrollTop = scrollElement.scrollHeight
    }
  } catch (error: any) {
    await MessagePlugin.error(error?.message || '会话消息读取失败')
  } finally {
    conversationLoading.value = false
  }
}

const hasEarlier = computed(() => conversationPage.value > 0 && conversationPage.value * CONVERSATION_PAGE_SIZE < conversationTotal.value)

function handleConversationScroll(event: Event) {
  const element = event.target as HTMLElement
  if (element.scrollTop <= 24) loadEarlier()
}

function questionPreview(value: string, limit = 120) {
  if (!value) return ''
  return value.length > limit ? value.slice(0, limit) + '…' : value
}

function feedbackLabel(value: string) { return feedbackOptions.find(option => option.value === value)?.label || value || '未标记' }
function feedbackTheme(value: string) {
  if (value === 'solved') return 'success'
  if (value === 'off_topic') return 'warning'
  if (value === 'inaccurate') return 'danger'
  return 'default'
}
function channelLabel(value?: string) { return channelOptions.find(option => option.value === value)?.label || value || '未知' }
function roleLabel(value: string) {
  if (value === 'user') return '用户'
  if (value === 'assistant') return '智能体'
  if (value === 'system') return '系统'
  return value
}
function messageClass(value: string) { return 'conversation-message--' + value }
function resourceNames(resources?: FeedbackAnalyticsResource[]) {
  return resources?.length ? resources.map(resource => resource.name).join('、') : '未记录'
}
function formatDate(value?: string) {
  if (!value) return '—'
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return value
  return date.toLocaleString('zh-CN', { hour12: false })
}

onMounted(loadAll)
onMounted(() => { void refreshDetailMarkdown() })
</script>

<style scoped lang="less">
@import '../../../components/css/chat-markdown.less';
@import '../../../components/css/chat-message-shared.less';
@import '../../../components/css/chat-citations.less';

.feedback-analytics { color: var(--td-text-color-primary); }
.section-header { display: flex; align-items: flex-start; justify-content: space-between; gap: 16px; margin-bottom: 22px; }
.section-header h2 { margin: 0 0 8px; font-size: 21px; line-height: 1.35; }
.section-description { margin: 0; color: var(--td-text-color-secondary); font-size: 13px; line-height: 1.6; }
.summary-grid { display: grid; grid-template-columns: repeat(5, minmax(100px, 1fr)); gap: 10px; margin-bottom: 16px; }
.summary-card { min-height: 76px; padding: 14px 16px; border: 1px solid var(--td-component-stroke); border-radius: 8px; background: var(--td-bg-color-container); box-sizing: border-box; }
.summary-card span { display: block; color: var(--td-text-color-secondary); font-size: 12px; }
.summary-card strong { display: block; margin-top: 5px; font-size: 24px; font-weight: 650; line-height: 1; }
.summary-card--solved strong { color: var(--td-success-color); }
.summary-card--off-topic strong { color: var(--td-warning-color); }
.summary-card--inaccurate strong { color: var(--td-error-color); }
.summary-card--unsolved strong { color: var(--td-text-color-secondary); }
.filter-panel { padding: 14px; border: 1px solid var(--td-component-stroke); border-radius: 8px; background: var(--td-bg-color-secondarycontainer); margin-bottom: 18px; }
.filter-row { display: flex; align-items: center; gap: 10px; flex-wrap: wrap; }
.filter-row + .filter-row { margin-top: 10px; }
.filter-control { min-width: 180px; flex: 1 1 180px; }
.filter-control--user { flex-basis: 250px; }
.filter-control--resource { flex-basis: 210px; }
.filter-control--feedback, .filter-control--channel { flex: 0 1 128px; }
.filter-actions { display: flex; align-items: center; gap: 4px; margin-left: auto; }
.analytics-alert { margin-bottom: 16px; }
.feedback-list-card { min-width: 0; border: 1px solid var(--td-component-stroke); border-radius: 8px; background: var(--td-bg-color-container); }
.feedback-list-card__header { display: flex; align-items: center; justify-content: space-between; gap: 12px; padding: 14px 16px 12px; border-bottom: 1px solid var(--td-component-stroke); }
.feedback-list-card__header h3 { margin: 0 0 4px; font-size: 15px; }
.feedback-list-card__header span { color: var(--td-text-color-secondary); font-size: 12px; }
.group-empty, .empty-state { padding: 28px 16px; color: var(--td-text-color-placeholder); font-size: 13px; text-align: center; }
.list-loading { display: flex; justify-content: center; padding: 28px 16px; }
.data-table-shell__pager { display: flex; justify-content: flex-end; padding: 12px 16px 14px; border-top: 1px solid var(--td-component-stroke); }
.feedback-list { min-width: 0; }
.feedback-row { padding: 15px 16px 14px; border-bottom: 1px solid var(--td-component-stroke); transition: background-color 0.15s ease; }
.feedback-row:last-child { border-bottom: 0; }
.feedback-row:hover { background: var(--td-bg-color-secondarycontainer); }
.feedback-row__top { display: flex; align-items: center; gap: 9px; min-width: 0; }
.feedback-row__time { color: var(--td-text-color-secondary); font-size: 12px; font-variant-numeric: tabular-nums; white-space: nowrap; }
.feedback-row__channel { color: var(--td-text-color-placeholder); font-size: 12px; }
.feedback-row__top .t-button { margin-left: auto; }
.feedback-row__meta { display: grid; grid-template-columns: minmax(150px, 1.1fr) minmax(130px, 0.9fr) minmax(160px, 1.2fr); gap: 16px; margin-top: 13px; }
.feedback-row__field { display: flex; min-width: 0; flex-direction: column; gap: 4px; }
.feedback-row__field strong { overflow: hidden; font-size: 13px; font-weight: 500; text-overflow: ellipsis; white-space: nowrap; }
.feedback-row__field small, .resource-id, .muted, .deleted-mark { overflow: hidden; color: var(--td-text-color-placeholder); font-size: 11px; text-overflow: ellipsis; white-space: nowrap; }
.field-label { color: var(--td-text-color-placeholder); font-size: 11px; line-height: 1.2; }
.resource-list { display: flex; flex-wrap: wrap; gap: 4px; }
.resource-name { overflow: hidden; font-weight: 500; text-overflow: ellipsis; white-space: nowrap; }
.resource-chip { max-width: 180px; overflow: hidden; padding: 2px 6px; border-radius: 4px; background: var(--td-bg-color-secondarycontainer); color: var(--td-text-color-secondary); font-size: 11px; text-overflow: ellipsis; white-space: nowrap; }
.resource-chip em { color: var(--td-text-color-placeholder); font-style: normal; }
.deleted-mark { display: block; color: var(--td-error-color); }
.feedback-row__question { display: flex; align-items: baseline; gap: 10px; min-width: 0; margin-top: 12px; padding-top: 11px; border-top: 1px solid var(--td-bg-color-secondarycontainer); color: var(--td-text-color-secondary); font-size: 13px; line-height: 1.55; }
.feedback-row__question > span:last-child { display: -webkit-box; min-width: 0; overflow: hidden; -webkit-box-orient: vertical; -webkit-line-clamp: 2; }
.detail-content { max-height: 70vh; overflow-y: auto; padding-right: 2px; }
.detail-meta, .detail-resource-line { display: flex; align-items: center; gap: 10px; flex-wrap: wrap; color: var(--td-text-color-secondary); font-size: 12px; }
.detail-resource-line { margin-top: 12px; padding-bottom: 14px; border-bottom: 1px solid var(--td-component-stroke); }
.progressive-step { margin-top: 14px; padding: 12px 14px; border-radius: 7px; background: var(--td-bg-color-secondarycontainer); }
.progressive-step--answer { background: color-mix(in srgb, var(--td-brand-color) 5%, var(--td-bg-color-secondarycontainer)); }
.progressive-step__label { margin-bottom: 7px; color: var(--td-text-color-secondary); font-size: 12px; font-weight: 600; }
.progressive-step__content { max-height: 7.2em; overflow: hidden; white-space: pre-wrap; word-break: break-word; font-size: 13px; line-height: 1.65; }
.progressive-step__content.is-expanded { max-height: none; }
.detail-render-surface { min-width: 0; }
.detail-markdown { .chat-markdown-typography(); .chat-citation-pills(); min-width: 0; }
.progressive-step__content--answer { max-height: 430px; overflow-y: auto; white-space: normal; word-break: normal; }
.progressive-actions { display: flex; justify-content: center; margin-top: 13px; }
.progressive-actions--left { justify-content: flex-start; }
.conversation-block { margin-top: 20px; }
.conversation-block__header { display: flex; align-items: baseline; justify-content: space-between; gap: 12px; margin-bottom: 10px; }
.conversation-block__header h4 { margin: 0 0 4px; font-size: 14px; }
.conversation-block__header span { color: var(--td-text-color-placeholder); font-size: 12px; }
.conversation-list { display: flex; max-height: 390px; overflow-y: auto; flex-direction: column; gap: 8px; padding: 4px; border-radius: 6px; background: var(--td-bg-color-secondarycontainer); }
.conversation-message { padding: 9px 11px; border-left: 3px solid var(--td-component-stroke); border-radius: 5px; background: var(--td-bg-color-container); }
.conversation-message--user { border-left-color: var(--td-brand-color); background: color-mix(in srgb, var(--td-brand-color) 4%, var(--td-bg-color-container)); }
.conversation-message--assistant { border-left-color: var(--td-success-color); background: color-mix(in srgb, var(--td-success-color) 7%, var(--td-bg-color-container)); }
.conversation-message--system { border-left-color: var(--td-warning-color); background: color-mix(in srgb, var(--td-warning-color) 5%, var(--td-bg-color-container)); }
.conversation-message__head { display: flex; justify-content: space-between; margin-bottom: 5px; color: var(--td-text-color-placeholder); font-size: 11px; }
.conversation-message__body { min-width: 0; font-size: 13px; line-height: 1.6; }
.conversation-message__body.detail-markdown { font-size: 14px; }
.load-earlier { flex: 0 0 auto; min-height: 30px; border: 0; border-radius: 5px; background: var(--td-bg-color-container); color: var(--td-brand-color); cursor: pointer; font-size: 12px; }
.load-earlier:hover { background: var(--td-bg-color-container-hover); }
.conversation-loading { display: flex; justify-content: center; padding: 8px; }
.detail-loading { display: flex; justify-content: center; padding: 48px 0; }
@media (max-width: 860px) {
  .summary-grid { grid-template-columns: repeat(3, minmax(90px, 1fr)); }
}
@media (max-width: 560px) {
  .summary-grid { grid-template-columns: repeat(2, minmax(90px, 1fr)); }
  .section-header { align-items: stretch; flex-direction: column; }
  .filter-control, .filter-control--feedback, .filter-control--channel { flex-basis: 100%; }
  .filter-actions { width: 100%; margin-left: 0; }
  .feedback-row__top { align-items: flex-start; flex-wrap: wrap; }
  .feedback-row__time { width: 100%; }
  .feedback-row__top .t-button { margin-left: 0; }
  .feedback-row__meta { grid-template-columns: 1fr; gap: 10px; }
}
</style>
