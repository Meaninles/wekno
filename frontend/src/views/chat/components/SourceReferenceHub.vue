<template>
  <div v-if="items.length" ref="rootElement" class="source-reference-hub" :class="{ 'is-embedded': embeddedMode }">
    <button type="button" class="source-reference-trigger" @click.stop="togglePanel">
      <t-icon class="source-reference-trigger__icon" name="file-search" />
      <span class="source-reference-trigger__text">引用 {{ items.length }} 条参考资料</span>
      <t-icon class="source-reference-trigger__arrow" name="chevron-right" />
    </button>

    <Teleport to="body">
      <div v-if="panelVisible" ref="panelElement" class="source-reference-panel" role="dialog" aria-modal="false"
        aria-label="全部参考资料">
        <div class="source-reference-panel__header">
          <span>全部参考资料</span>
          <button type="button" class="source-reference-panel__close" aria-label="关闭" @click="panelVisible = false">
            <t-icon name="close" />
          </button>
        </div>
        <div class="source-reference-panel__list">
          <div v-for="item in items" :key="item.key" class="source-reference-card"
            :class="{ 'is-clickable': item.clickable }" :role="item.clickable ? 'button' : undefined"
            :tabindex="item.clickable ? 0 : undefined" @click="activateItem(item)"
            @keydown.enter.prevent="activateItem(item)" @keydown.space.prevent="activateItem(item)">
            <div class="source-reference-card__title">
              <span class="source-reference-card__number">{{ item.number }}.</span>
              <span class="source-reference-card__name" :title="item.title">{{ item.title }}</span>
            </div>
            <div class="source-reference-card__source">
              <t-icon class="source-reference-card__source-icon" :name="item.icon" />
              <span class="source-reference-card__type">{{ sourceTypeText(item.type) }}</span>
              <span v-if="sourceDetailLabel(item)" class="source-reference-card__source-name"
                :title="sourceDetailLabel(item)">
                {{ sourceDetailLabel(item) }}
              </span>
            </div>
            <div v-if="item.snippet" class="source-reference-card__snippet">{{ item.snippet }}</div>
          </div>
        </div>
      </div>
    </Teleport>

  </div>

  <t-drawer v-if="wikiDrawerVisible" v-model:visible="wikiDrawerVisible" :header="wikiDrawerPage?.title || ''"
    size="480px" :footer="false" placement="right" attach="body" :show-overlay="true" :close-btn="true"
    :close-on-overlay-click="true" :destroy-on-close="true" class="source-reference-wiki-drawer">
    <template v-if="wikiDrawerPage">
      <div class="wiki-reader-meta">
        <t-tag v-if="wikiDrawerPage.page_type" size="small" :theme="getTypeTheme(wikiDrawerPage.page_type)"
          variant="light-outline">
          {{ getTypeLabel(wikiDrawerPage.page_type) }}
        </t-tag>
        <span v-if="wikiDrawerPage.version" class="wiki-reader-meta-text">
          {{ t('knowledgeEditor.wikiBrowser.version', { ver: wikiDrawerPage.version }) }}
        </span>
        <t-button size="small" variant="text" @click="openWikiGraphInNewTab">
          {{ t('knowledgeEditor.wikiBrowser.viewInGraph') }}
        </t-button>
      </div>
      <div ref="wikiDrawerBodyRef" class="wiki-reader-body" v-html="wikiDrawerContent"
        @click="handleWikiDrawerClick" />
    </template>
  </t-drawer>

  <t-drawer v-if="knowledgeDrawerVisible" v-model:visible="knowledgeDrawerVisible"
    :header="knowledgeDrawer.title || '知识库文档'" size="480px" :footer="false" placement="right" attach="body"
    :show-overlay="true" :close-btn="true" :close-on-overlay-click="true" :destroy-on-close="true"
    class="source-reference-wiki-drawer">
    <div class="wiki-reader-meta">
      <t-tag size="small" theme="primary" variant="light-outline">文档片段</t-tag>
      <span v-if="knowledgeDrawerMetaText" class="wiki-reader-meta-text">{{ knowledgeDrawerMetaText }}</span>
      <t-button v-if="canOpenKnowledgeDocument" size="small" variant="text" @click="openKnowledgeDocumentInNewTab">
        打开文档
      </t-button>
    </div>
    <div v-if="knowledgeDrawer.loading" class="wiki-reader-body source-reference-reader-state">
      正在加载文档片段
    </div>
    <div v-else-if="knowledgeDrawer.error" class="wiki-reader-body source-reference-reader-state is-error">
      {{ knowledgeDrawer.error }}
    </div>
    <div v-else ref="knowledgeDrawerBodyRef" class="wiki-reader-body" v-html="knowledgeDrawerContent" />
  </t-drawer>
</template>

<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRouter } from 'vue-router'
import { marked } from 'marked'
import { MessagePlugin } from 'tdesign-vue-next'
import { getChunkByIdOnly } from '@/api/knowledge-base'
import { getWikiPage, type WikiPage } from '@/api/wiki'
import { useChatResourcesStore } from '@/stores/chatResources'
import { hydrateProtectedFileImages, sanitizeMarkdownHTML } from '@/utils/security'
import { wrapChatMarkdownTables } from '@/utils/chatMarkdownRenderer'
import {
  buildCitedSourceReferenceItems,
  buildSourceReferenceItems,
  sourceTypeLabel,
  type SourceReference,
  type SourceReferenceItem,
  type SourceReferenceKind,
} from '@/utils/sourceReferences'

type SessionWithReferences = {
  content?: string
  is_completed?: boolean
  agentEventStream?: Array<Record<string, unknown>>
  knowledge_references?: SourceReference[]
}

type KnowledgeDrawerState = {
  title: string
  content: string
  knowledgeBaseId: string
  knowledgeId: string
  chunkId: string
  chunkIndex: number | null
  loading: boolean
  error: string
}

const props = defineProps<{
  session?: SessionWithReferences
  content?: string
  embeddedMode?: boolean
}>()

const { t } = useI18n()
const router = useRouter()
const chatResources = useChatResourcesStore()

const rootElement = ref<HTMLElement | null>(null)
const panelElement = ref<HTMLElement | null>(null)
const panelVisible = ref(false)
const wikiDrawerVisible = ref(false)
const wikiDrawerPage = ref<WikiPage | null>(null)
const wikiDrawerBodyRef = ref<HTMLElement | null>(null)
const currentWikiKbId = ref('')
const knowledgeDrawerVisible = ref(false)
const knowledgeDrawerBodyRef = ref<HTMLElement | null>(null)
const knowledgeDrawer = ref<KnowledgeDrawerState>({
  title: '',
  content: '',
  knowledgeBaseId: '',
  knowledgeId: '',
  chunkId: '',
  chunkIndex: null,
  loading: false,
  error: '',
})
const panelOwnerId = `source-reference-${Math.random().toString(36).slice(2)}`

const referenceContent = computed(() => {
  const explicit = String(props.content || '')
  if (explicit.trim()) return explicit

  const direct = String(props.session?.content || '')
  if (direct.trim()) return direct

  const stream = props.session?.agentEventStream
  if (!Array.isArray(stream)) return ''
  return stream
    .filter((event) => event?.type === 'answer' && event?.superseded !== true)
    .map((event) => String(event?.content || ''))
    .filter((content) => content.trim())
    .join('\n\n')
})

const items = computed(() => buildCitedSourceReferenceItems(
  props.session?.knowledge_references,
  referenceContent.value,
  Boolean(props.session?.is_completed),
))

const allItems = computed(() => buildSourceReferenceItems(props.session?.knowledge_references))

const wikiDrawerContent = computed(() => {
  if (!wikiDrawerPage.value) return ''
  const preprocessed = String(wikiDrawerPage.value.content || '').replace(
    /\[\[([^\]]+)\]\]/g,
    (_, inner: string) => {
      const pipeIdx = inner.indexOf('|')
      const slug = pipeIdx > 0 ? inner.substring(0, pipeIdx).trim() : inner.trim()
      const display = pipeIdx > 0
        ? inner.substring(pipeIdx + 1).trim()
        : slug.split('/').slice(-1)[0] || slug
      return `<a href="#" class="wiki-content-link citation-wiki" data-slug="${escapeHtml(slug)}">${escapeHtml(display)}</a>`
    },
  )
  const html = marked.parse(preprocessed, { breaks: true, async: false }) as string
  return sanitizeMarkdownHTML(wrapChatMarkdownTables(html))
})

const knowledgeDrawerContent = computed(() => {
  const html = marked.parse(knowledgeDrawer.value.content || '', { breaks: true, async: false }) as string
  return sanitizeMarkdownHTML(wrapChatMarkdownTables(html))
})

const knowledgeDrawerMetaText = computed(() => {
  const index = knowledgeDrawer.value.chunkIndex
  return index === null ? '' : `第 ${index + 1} 个文档片段`
})

const canOpenKnowledgeDocument = computed(() =>
  Boolean(knowledgeDrawer.value.knowledgeBaseId && knowledgeDrawer.value.knowledgeId),
)

watch(wikiDrawerContent, async () => {
  await nextTick()
  if (wikiDrawerBodyRef.value) {
    await hydrateProtectedFileImages(wikiDrawerBodyRef.value)
  }
})

watch(knowledgeDrawerContent, async () => {
  await nextTick()
  if (knowledgeDrawerBodyRef.value) {
    await hydrateProtectedFileImages(knowledgeDrawerBodyRef.value)
  }
})

watch(panelVisible, (visible) => {
  if (visible) {
    claimReferencePanelLayout()
    window.dispatchEvent(new CustomEvent('weknora:source-reference-panel-open', {
      detail: { ownerId: panelOwnerId },
    }))
    window.addEventListener('keydown', handleGlobalKeydown, true)
    window.addEventListener('click', handleGlobalClick, true)
  } else {
    releaseReferencePanelLayout()
    window.removeEventListener('keydown', handleGlobalKeydown, true)
    window.removeEventListener('click', handleGlobalClick, true)
  }
})

window.addEventListener('weknora:source-reference-panel-open', handleOtherReferencePanelOpen as EventListener)
window.addEventListener('weknora:wiki-drawer-open', handleWikiDrawerOpenElsewhere as EventListener)

onBeforeUnmount(() => {
  releaseReferencePanelLayout()
  window.removeEventListener('keydown', handleGlobalKeydown, true)
  window.removeEventListener('click', handleGlobalClick, true)
  window.removeEventListener('weknora:source-reference-panel-open', handleOtherReferencePanelOpen as EventListener)
  window.removeEventListener('weknora:wiki-drawer-open', handleWikiDrawerOpenElsewhere as EventListener)
})

function togglePanel() {
  panelVisible.value = !panelVisible.value
}

function activateByElement(el: HTMLElement): boolean {
  const citationId = el.getAttribute('data-source-id') || ''
  const matched = items.value.find((item) => item.citationId === citationId)
  if (matched) {
    activateItem(matched)
    return true
  }

  const fallback = itemFromElement(el)
  if (!fallback) return false
  activateItem(fallback)
  return true
}

function activateItem(item: SourceReferenceItem) {
  if (!item.clickable) return
  if (item.type === 'web' && item.url) {
    window.open(item.url, '_blank', 'noopener,noreferrer')
    return
  }
  if (item.type === 'knowledge') {
    void openKnowledgeDrawer(item)
    return
  }
  if (item.type === 'wiki' && item.slug) {
    void openWikiDrawer(item.knowledgeBaseId, item.slug)
    return
  }
  if (item.type === 'data_source' && item.sourceId) {
    openRouteInNewTab({
      path: '/platform/data-sources',
      query: { source_id: item.sourceId },
    })
  }
}

function itemFromElement(el: HTMLElement): SourceReferenceItem | null {
  const type = (el.getAttribute('data-source-type') || 'knowledge') as SourceReferenceKind
  const title = el.getAttribute('data-title') || sourceTypeLabel(type)
  const url = el.getAttribute('data-url') || ''
  const knowledgeBaseId = el.getAttribute('data-kb-id') || ''
  const knowledgeId = el.getAttribute('data-knowledge-id') || ''
  const chunkId = el.getAttribute('data-chunk-id') || ''
  const chunkIndexAttr = el.getAttribute('data-chunk-index') || ''
  const chunkIndex = chunkIndexAttr === '' ? NaN : Number(chunkIndexAttr)
  const slug = el.getAttribute('data-slug') || ''
  const citationId = el.getAttribute('data-source-id') || ''
  return {
    key: citationId || `${type}:${title}`,
    number: Number(el.getAttribute('data-citation-number') || '0') || 0,
    citationId,
    type,
    title,
    sourceLabel: el.getAttribute('data-source-label') || sourceTypeLabel(type),
    snippet: '',
    count: 1,
    icon: type === 'web' ? 'internet' : type === 'wiki' ? 'browse' : type === 'data_source' ? 'server' : 'file',
    url,
    knowledgeBaseId,
    knowledgeId,
    chunkId,
    chunkIndex: Number.isFinite(chunkIndex) ? chunkIndex : null,
    startAt: null,
    endAt: null,
    slug,
    sourceId: el.getAttribute('data-data-source-id') || '',
    clickable: type === 'web'
      ? Boolean(url)
      : type === 'wiki'
        ? Boolean(slug)
        : Boolean(chunkId || knowledgeId || knowledgeBaseId),
  }
}

async function openKnowledgeDrawer(item: SourceReferenceItem) {
  panelVisible.value = false
  wikiDrawerVisible.value = false
  knowledgeDrawer.value = {
    title: item.title || '知识库文档',
    content: item.snippet || '',
    knowledgeBaseId: item.knowledgeBaseId,
    knowledgeId: item.knowledgeId,
    chunkId: item.chunkId,
    chunkIndex: item.chunkIndex,
    loading: Boolean(item.chunkId),
    error: '',
  }
  knowledgeDrawerVisible.value = true

  if (!item.chunkId) {
    if (!item.snippet) {
      knowledgeDrawer.value.error = '这个引用没有关联到可打开的文档片段。'
    }
    knowledgeDrawer.value.loading = false
    return
  }

  try {
    const res: any = await getChunkByIdOnly(item.chunkId)
    const data = res?.data || res || {}
    knowledgeDrawer.value = {
      ...knowledgeDrawer.value,
      content: String(data.content || item.snippet || '').trim(),
      knowledgeId: item.knowledgeId || data.knowledge_id || '',
      knowledgeBaseId: item.knowledgeBaseId || data.knowledge_base_id || '',
      chunkIndex: item.chunkIndex ?? (Number.isFinite(Number(data.chunk_index)) ? Number(data.chunk_index) : null),
      loading: false,
      error: data.content || item.snippet ? '' : '没有找到这个文档片段的正文内容。',
    }
  } catch (e) {
    console.error(`Failed to load knowledge fragment ${item.chunkId}:`, e)
    knowledgeDrawer.value = {
      ...knowledgeDrawer.value,
      loading: false,
      error: '文档片段加载失败，可以稍后重试。',
    }
  }
}

function sameSlug(left: string, right: string) {
  if (left === right) return true
  try {
    return decodeURIComponent(left) === right || left === decodeURIComponent(right)
  } catch {
    return false
  }
}

function isWikiKb(kb: any) {
  return kb?.indexing_strategy?.wiki_enabled === true || kb?.wiki_config?.wiki_enabled === true
}

async function resolveWikiPage(kbId: string, slug: string): Promise<WikiPage> {
  if (kbId) {
    const res = await getWikiPage(kbId, slug)
    return ((res as any).data || res) as WikiPage
  }

  const referenced = allItems.value.find((item) =>
    item.type === 'wiki' && sameSlug(item.slug, slug) && item.knowledgeBaseId,
  )
  if (referenced?.knowledgeBaseId) {
    const res = await getWikiPage(referenced.knowledgeBaseId, slug)
    return ((res as any).data || res) as WikiPage
  }

  await chatResources.ensureKnowledgeBases().catch(() => undefined)
  const candidates = (chatResources.rawKnowledgeBases || [])
    .filter(isWikiKb)
    .map((kb: any) => String(kb.id || ''))
    .filter(Boolean)

  for (const candidateKbId of candidates) {
    try {
      const res = await getWikiPage(candidateKbId, slug)
      const page = ((res as any).data || res) as WikiPage
      if (page?.slug || page?.title || page?.content) return page
    } catch {
      // Try the next accessible wiki knowledge base.
    }
  }

  throw new Error('Wiki page not found')
}

async function openWikiDrawer(kbId: string, slug: string) {
  if (!slug) return
  try {
    panelVisible.value = false
    knowledgeDrawerVisible.value = false
    window.dispatchEvent(new CustomEvent('weknora:wiki-drawer-open'))
    const page = await resolveWikiPage(kbId, slug)
    currentWikiKbId.value = page.knowledge_base_id || kbId
    wikiDrawerPage.value = page
    wikiDrawerVisible.value = true
  } catch (e) {
    console.error(`Failed to load wiki page ${slug}:`, e)
    MessagePlugin.warning(t('agentStream.citation.loadFailed'))
  }
}

function openKnowledgeDocumentInNewTab() {
  if (!knowledgeDrawer.value.knowledgeBaseId || !knowledgeDrawer.value.knowledgeId) return
  router.push({
    path: `/platform/knowledge-bases/${knowledgeDrawer.value.knowledgeBaseId}`,
    query: { knowledge_id: knowledgeDrawer.value.knowledgeId },
  })
}

function openWikiGraphInNewTab() {
  if (!currentWikiKbId.value || !wikiDrawerPage.value?.slug) return
  openRouteInNewTab({
    path: `/platform/knowledge-bases/${currentWikiKbId.value}`,
    query: {
      tab: 'graph',
      slug: wikiDrawerPage.value.slug,
    },
  })
}

function openRouteInNewTab(location: Parameters<typeof router.resolve>[0]) {
  const href = router.resolve(location).href
  window.open(new URL(href, window.location.origin).toString(), '_blank', 'noopener,noreferrer')
}

function handleWikiDrawerClick(e: MouseEvent) {
  const target = e.target as HTMLElement
  const wikiEl = target.closest?.('.citation-wiki') as HTMLElement | null
  if (wikiEl) {
    e.preventDefault()
    e.stopPropagation()
    const slug = wikiEl.getAttribute('data-slug') || ''
    if (slug) void openWikiDrawer(currentWikiKbId.value, slug)
    return
  }

  const aEl = target.closest?.('a') as HTMLAnchorElement | null
  if (aEl?.href && (aEl.href.startsWith('http://') || aEl.href.startsWith('https://'))) {
    e.preventDefault()
    window.open(aEl.href, '_blank', 'noopener,noreferrer')
  }
}

function handleGlobalKeydown(event: KeyboardEvent) {
  if (event.key === 'Escape') panelVisible.value = false
}

function handleGlobalClick(event: MouseEvent) {
  const target = event.target as Node | null
  if (!target) return
  if (panelElement.value?.contains(target) || rootElement.value?.contains(target)) return
  panelVisible.value = false
}

function handleOtherReferencePanelOpen(event: CustomEvent<{ ownerId?: string }>) {
  if (event.detail?.ownerId !== panelOwnerId) panelVisible.value = false
}

function handleWikiDrawerOpenElsewhere() {
  panelVisible.value = false
}

function claimReferencePanelLayout() {
  document.documentElement.dataset.sourceReferencePanelOwner = panelOwnerId
}

function releaseReferencePanelLayout() {
  const root = document.documentElement
  if (root.dataset.sourceReferencePanelOwner !== panelOwnerId) return
  delete root.dataset.sourceReferencePanelOwner
}

function sourceTypeText(type: SourceReferenceKind): string {
  return sourceTypeLabel(type)
}

function sourceDetailLabel(item: SourceReferenceItem): string {
  const typeText = sourceTypeText(item.type)
  const label = item.sourceLabel || ''
  return label && label !== typeText ? label : ''
}

function getTypeTheme(type?: string): string {
  const map: Record<string, string> = {
    summary: 'primary',
    entity: 'success',
    concept: 'warning',
    synthesis: 'primary',
    comparison: 'danger',
    index: 'default',
    log: 'default',
  }
  return type ? map[type] || 'default' : 'default'
}

function getTypeLabel(type?: string): string {
  if (!type) return ''
  const map: Record<string, string> = {
    summary: t('knowledgeEditor.wikiBrowser.filterSummary'),
    entity: t('knowledgeEditor.wikiBrowser.filterEntity'),
    concept: t('knowledgeEditor.wikiBrowser.filterConcept'),
    synthesis: t('knowledgeEditor.wikiBrowser.filterSynthesis'),
    comparison: t('knowledgeEditor.wikiBrowser.filterComparison'),
    index: 'Index',
    log: 'Log',
  }
  return map[type] || type
}

function escapeHtml(text: string): string {
  return String(text)
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
}

defineExpose({
  activateByElement,
  openPanel: () => {
    panelVisible.value = true
  },
})
</script>

<style scoped lang="less">
.source-reference-hub {
  display: inline-flex;
  align-items: center;
  align-self: flex-start;
  width: fit-content;
  max-width: 100%;
  margin: 0 0 8px;
}

.source-reference-trigger {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  max-width: 100%;
  min-height: 22px;
  padding: 0;
  border: 0;
  border-radius: 0;
  background: transparent;
  color: var(--td-text-color-secondary);
  font: inherit;
  font-size: 13px;
  line-height: 1.5;
  cursor: pointer;
  transition: background 0.15s ease, color 0.15s ease;

  &:hover {
    background: transparent;
    color: var(--td-text-color-primary);
  }

  &:focus-visible {
    outline: 2px solid var(--td-component-border);
    outline-offset: 2px;
  }
}

.source-reference-trigger__text {
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.source-reference-trigger__icon,
.source-reference-trigger__arrow {
  flex-shrink: 0;
  color: var(--td-text-color-placeholder);
}

</style>

<style lang="less">
:root {
  --source-reference-panel-width: clamp(320px, 23vw, 420px);
  --source-reference-panel-right: clamp(16px, 3vw, 42px);
}

.source-reference-panel {
  position: fixed;
  top: clamp(72px, 9vh, 96px);
  right: var(--source-reference-panel-right);
  z-index: 6000;
  width: var(--source-reference-panel-width);
  max-height: min(78vh, 680px);
  display: flex;
  flex-direction: column;
  padding: 14px;
  border-radius: 16px;
  background: var(--td-bg-color-container);
  border: 1px solid var(--td-component-stroke);
  box-shadow: 0 18px 48px rgba(0, 0, 0, 0.16);
  box-sizing: border-box;
}

.source-reference-panel__header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  min-height: 28px;
  margin-bottom: 12px;
  font-size: 14px;
  font-weight: 500;
  color: var(--td-text-color-primary);
}

.source-reference-panel__close {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 26px;
  height: 26px;
  padding: 0;
  border: 0;
  border-radius: 50%;
  background: transparent;
  color: var(--td-text-color-secondary);
  cursor: pointer;

  &:hover {
    background: var(--td-bg-color-secondarycontainer);
    color: var(--td-text-color-primary);
  }
}

.source-reference-panel__list {
  min-height: 0;
  overflow-y: auto;
  display: flex;
  flex-direction: column;
  gap: 8px;
  padding-right: 2px;
}

.source-reference-card {
  flex: 0 0 auto;
  width: 100%;
  min-height: 0;
  display: flex;
  flex-direction: column;
  padding: 10px 12px;
  border-radius: 8px;
  border: 1px solid var(--td-component-stroke);
  background: var(--td-bg-color-container);
  color: var(--td-text-color-primary);
  text-align: left;
  font: inherit;
  cursor: default;
  box-sizing: border-box;
  transition: border-color 0.15s ease, background 0.15s ease, transform 0.15s ease;

  &.is-clickable {
    cursor: pointer;
  }

  &.is-clickable:hover {
    border-color: color-mix(in srgb, var(--td-brand-color) 45%, var(--td-component-stroke));
    background: color-mix(in srgb, var(--td-brand-color) 3%, var(--td-bg-color-container));
  }

  &:focus-visible {
    outline: 2px solid var(--td-component-border);
    outline-offset: 2px;
  }
}

.source-reference-card__title {
  flex: 0 0 auto;
  display: grid;
  grid-template-columns: auto minmax(0, 1fr);
  gap: 4px;
  font-size: 14px;
  line-height: 20px;
  font-weight: 500;
}

.source-reference-card__number {
  color: var(--td-text-color-secondary);
}

.source-reference-card__name {
  min-width: 0;
  display: -webkit-box;
  -webkit-line-clamp: 2;
  line-clamp: 2;
  -webkit-box-orient: vertical;
  overflow: hidden;
  word-break: break-word;
}

.source-reference-card__source {
  flex: 0 0 auto;
  display: flex;
  align-items: center;
  gap: 5px;
  min-width: 0;
  margin-top: 7px;
  font-size: 12px;
  line-height: 1.4;
  color: var(--td-text-color-placeholder);
  white-space: nowrap;
  overflow: hidden;
}

.source-reference-card__source-icon {
  flex-shrink: 0;
}

.source-reference-card__type {
  flex-shrink: 0;
}

.source-reference-card__source-name {
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.source-reference-card__snippet {
  flex: 0 0 auto;
  margin-top: 7px;
  color: var(--td-text-color-secondary);
  font-size: 12px;
  line-height: 18px;
  height: 36px;
  overflow: hidden;
  word-break: break-word;
}

.source-reference-wiki-drawer {
  box-shadow: -4px 0 16px rgba(0, 0, 0, 0.08);

  .wiki-reader-meta {
    display: flex;
    align-items: center;
    gap: 12px;
    margin-bottom: 14px;

    .t-button {
      margin-left: auto;
    }
  }

  .wiki-reader-meta-text {
    font-size: 13px;
    color: var(--td-text-color-placeholder);
  }

  .wiki-reader-body {
    line-height: 1.6;
    font-size: 14px;
    color: var(--td-text-color-primary);

    h1 {
      font-size: 24px;
      margin: 28px 0 16px;
      font-weight: 600;
      line-height: 1.4;
    }

    h2 {
      font-size: 18px;
      margin: 24px 0 12px;
      font-weight: 600;
      line-height: 1.4;
    }

    h3 {
      font-size: 16px;
      margin: 20px 0 10px;
      font-weight: 600;
      line-height: 1.5;
    }

    h4,
    h5,
    h6 {
      font-size: 14px;
      margin: 16px 0 8px;
      font-weight: 600;
      line-height: 1.5;
    }

    p {
      margin: 0 0 14px;
    }

    ul,
    ol {
      margin: 0 0 14px;
      padding-left: 24px;
    }

    li {
      margin-bottom: 6px;
      line-height: 1.6;
    }

    blockquote {
      margin: 0 0 14px;
      padding: 10px 16px;
      background: var(--td-bg-color-secondarycontainer);
      border-left: 4px solid var(--td-component-border);
      border-radius: 0 4px 4px 0;
      color: var(--td-text-color-secondary);
    }

    code {
      font-family: var(--app-font-family-mono);
      font-size: 13px;
      padding: 2px 4px;
      background: var(--td-bg-color-secondarycontainer);
      border-radius: 4px;
      color: var(--td-brand-color);
    }

    pre {
      margin: 0 0 14px;
      padding: 12px 16px;
      background: var(--td-bg-color-secondarycontainer);
      border-radius: 6px;
      overflow-x: auto;

      code {
        padding: 0;
        background: transparent;
        color: inherit;
      }
    }

    a.wiki-content-link {
      color: var(--td-brand-color);
      text-decoration: none;
      border-bottom: 1px dashed var(--td-brand-color);
      cursor: pointer;
      font-weight: 500;

      &:hover {
        border-bottom-style: solid;
        text-decoration: none !important;
      }
    }

    .chat-markdown-table {
      width: fit-content;
      max-width: 100%;
      overflow-x: auto;
      margin: 0 0 16px;
      background: var(--td-bg-color-container);
      border: 1px solid var(--td-component-stroke);
      border-radius: 6px;
      -webkit-overflow-scrolling: touch;
    }

    table {
      display: table;
      width: max-content;
      min-width: 0;
      border-collapse: separate;
      border-spacing: 0;
      font-size: 13px;
      line-height: 1.55;
    }

    th,
    td {
      padding: 8px 10px;
      border-right: 1px solid var(--td-component-stroke);
      border-bottom: 1px solid var(--td-component-stroke);
      text-align: left;
      vertical-align: top;
    }
  }

  .source-reference-reader-state {
    padding: 18px;
    border-radius: 8px;
    background: var(--td-bg-color-secondarycontainer);
    color: var(--td-text-color-secondary);
    text-align: center;

    &.is-error {
      color: var(--td-error-color);
      background: var(--td-error-color-1);
    }
  }
}

@media (max-width: 720px) {
  .source-reference-panel {
    top: 72px;
    right: 12px;
    width: calc(100vw - 24px);
    max-height: min(76vh, 620px);
  }
}
</style>
