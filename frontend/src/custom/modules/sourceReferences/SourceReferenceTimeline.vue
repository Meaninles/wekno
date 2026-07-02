<template>
  <div v-if="totalCount > 0" class="source-reference-timeline" :class="{ 'is-embedded': embeddedMode }">
    <div class="tree-children">
      <div class="tree-child tree-child-last source-ref-step">
        <div class="tree-branch" />
        <div class="tree-child-content">
          <div class="tool-event">
            <div class="action-card">
              <div class="action-header" @click="toggleExpanded">
                <div class="action-title">
                  <t-icon class="action-title-icon" name="file-search" />
                  <span class="action-name">{{ headerText }}</span>
                  <span class="action-show-icon">
                    <t-icon :name="expanded ? 'chevron-down' : 'chevron-right'" />
                  </span>
                </div>
              </div>

              <div v-show="expanded" class="source-ref-list">
                <button
                  v-for="(item, index) in sourceRows"
                  :key="item.key"
                  type="button"
                  class="source-ref-item"
                  :class="{ 'is-clickable': item.clickable }"
                  @click.stop="activateItem(item)"
                >
                  <span class="source-ref-index">{{ index + 1 }}.</span>
                  <t-icon class="source-ref-icon" :name="item.icon" />
                  <span class="source-ref-title" :title="item.title">{{ item.title }}</span>
                  <span v-if="item.meta" class="source-ref-meta">{{ item.meta }}</span>
                  <t-icon v-if="item.clickable" class="source-ref-jump" name="jump" />
                </button>
              </div>
            </div>
          </div>
        </div>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, ref } from 'vue'
import { useRouter } from 'vue-router'

type SourceKind = 'knowledge' | 'wiki' | 'web' | 'data_source'

type SourceReference = {
  id?: string
  content?: string
  knowledge_id?: string
  knowledge_title?: string
  knowledge_filename?: string
  knowledge_base_id?: string
  chunk_type?: string
  metadata?: Record<string, string>
}

type SourceRow = {
  key: string
  type: SourceKind
  title: string
  meta: string
  icon: string
  clickable: boolean
  href?: string
  knowledgeId?: string
  knowledgeBaseId?: string
  slug?: string
  sourceId?: string
}

const props = defineProps<{
  session?: {
    knowledge_references?: SourceReference[]
  }
  embeddedMode?: boolean
}>()

const router = useRouter()
const expanded = ref(true)

const references = computed(() => props.session?.knowledge_references ?? [])

const knowledgeRows = computed<SourceRow[]>(() => {
  const groups = new Map<string, SourceRow & { count: number }>()
  for (const ref of references.value) {
    if (sourceKind(ref) !== 'knowledge') continue
    const title = ref.knowledge_title || ref.knowledge_filename || ref.knowledge_id || ref.id || '未命名文档'
    const key = ref.knowledge_id || ref.id || title
    if (!groups.has(key)) {
      groups.set(key, {
        key: `knowledge:${key}`,
        type: 'knowledge',
        title,
        meta: '',
        icon: 'file',
        clickable: Boolean(ref.knowledge_base_id),
        knowledgeId: ref.knowledge_id,
        knowledgeBaseId: ref.knowledge_base_id,
        count: 0,
      })
    }
    groups.get(key)!.count += 1
  }
  return Array.from(groups.values()).map((row) => ({
    ...row,
    meta: `${row.count}个片段`,
  }))
})

const wikiRows = computed<SourceRow[]>(() => {
  const rows: SourceRow[] = []
  const seen = new Set<string>()
  for (const ref of references.value) {
    if (sourceKind(ref) !== 'wiki') continue
    const metadata = ref.metadata || {}
    const slug = metadata.slug || ref.id?.replace(/^wiki:[^:]*:/, '') || ''
    const kbId = ref.knowledge_base_id || metadata.knowledge_base_id || ''
    const title = ref.knowledge_title || slug || 'Wiki 页面'
    const key = `wiki:${kbId}:${slug || title}`
    if (seen.has(key)) continue
    seen.add(key)
    rows.push({
      key,
      type: 'wiki',
      title,
      meta: 'Wiki',
      icon: 'browse',
      clickable: Boolean(kbId && slug),
      knowledgeBaseId: kbId,
      slug,
    })
  }
  return rows
})

const webRows = computed<SourceRow[]>(() => {
  const rows: SourceRow[] = []
  const seen = new Set<string>()
  for (const ref of references.value) {
    if (sourceKind(ref) !== 'web') continue
    const metadata = ref.metadata || {}
    const href = metadata.url || (isHttpUrl(ref.id) ? ref.id : '')
    const title = ref.knowledge_title || metadata.title || hostFromUrl(href) || href || '网页'
    const key = `web:${href || title}`
    if (seen.has(key)) continue
    seen.add(key)
    rows.push({
      key,
      type: 'web',
      title,
      meta: hostFromUrl(href),
      icon: 'internet',
      clickable: Boolean(href),
      href,
    })
  }
  return rows
})

const dataSourceRows = computed<SourceRow[]>(() => {
  const rows: SourceRow[] = []
  const seen = new Set<string>()
  for (const ref of references.value) {
    if (sourceKind(ref) !== 'data_source') continue
    const metadata = ref.metadata || {}
    const sourceId = metadata.source_id || ref.id?.replace(/^data_source:/, '') || ''
    const title = metadata.source_name || ref.knowledge_title || sourceId || '数据源'
    const key = `data_source:${sourceId || title}`
    if (seen.has(key)) continue
    seen.add(key)
    rows.push({
      key,
      type: 'data_source',
      title,
      meta: metadata.database_type || '数据源',
      icon: 'server',
      clickable: Boolean(sourceId),
      sourceId,
    })
  }
  return rows
})

const sourceRows = computed(() => [
  ...knowledgeRows.value,
  ...wikiRows.value,
  ...webRows.value,
  ...dataSourceRows.value,
])

const totalCount = computed(() =>
  knowledgeRows.value.length + wikiRows.value.length + webRows.value.length + dataSourceRows.value.length,
)

const headerText = computed(() => {
  const parts: string[] = []
  if (knowledgeRows.value.length) parts.push(`${knowledgeRows.value.length}篇文档`)
  if (wikiRows.value.length) parts.push(`${wikiRows.value.length}个Wiki页面`)
  if (webRows.value.length) parts.push(`${webRows.value.length}条网页`)
  if (dataSourceRows.value.length) parts.push(`${dataSourceRows.value.length}个数据源`)
  return parts.length > 0 ? `引用了${joinChinese(parts)}` : `引用了${totalCount.value}个来源`
})

function sourceKind(ref: SourceReference): SourceKind {
  const metadataType = ref.metadata?.source_type
  if (metadataType === 'wiki') return 'wiki'
  if (metadataType === 'web') return 'web'
  if (metadataType === 'data_source') return 'data_source'
  if (ref.chunk_type === 'wiki_page') return 'wiki'
  if (ref.chunk_type === 'web_search') return 'web'
  if (ref.chunk_type === 'data_source') return 'data_source'
  return 'knowledge'
}

function joinChinese(parts: string[]) {
  if (parts.length <= 1) return parts[0] || ''
  if (parts.length === 2) return `${parts[0]}和${parts[1]}`
  return `${parts.slice(0, -1).join('、')}和${parts[parts.length - 1]}`
}

function toggleExpanded() {
  expanded.value = !expanded.value
}

function activateItem(item: SourceRow) {
  if (!item.clickable) return
  if (item.type === 'web' && item.href) {
    window.open(item.href, '_blank', 'noopener,noreferrer')
    return
  }
  if (item.type === 'knowledge' && item.knowledgeBaseId) {
    router.push({
      path: `/platform/knowledge-bases/${item.knowledgeBaseId}`,
      query: item.knowledgeId ? { knowledge_id: item.knowledgeId } : {},
    })
    return
  }
  if (item.type === 'wiki' && item.knowledgeBaseId && item.slug) {
    router.push({
      path: `/platform/knowledge-bases/${item.knowledgeBaseId}`,
      query: { tab: 'graph', slug: item.slug },
    })
    return
  }
  if (item.type === 'data_source' && item.sourceId) {
    router.push({
      path: '/platform/data-sources',
      query: { source_id: item.sourceId },
    })
  }
}

function isHttpUrl(value?: string) {
  return Boolean(value && (value.startsWith('http://') || value.startsWith('https://')))
}

function hostFromUrl(value?: string) {
  if (!value) return ''
  try {
    return new URL(value).hostname
  } catch {
    return ''
  }
}
</script>

<style scoped lang="less">
.source-reference-timeline {
  --source-ref-line-color: color-mix(in srgb, var(--td-text-color-primary) 16%, transparent);
  --source-ref-icon-color: var(--td-text-color-placeholder);

  margin: 0;
}

.tree-children {
  position: relative;
  padding-left: 0;
  margin-top: 0;
  margin-left: 10px;
}

.tree-child {
  position: relative;
  padding-left: 42px;
  margin-bottom: 18px;

  &::before {
    content: '';
    position: absolute;
    left: 9px;
    top: 22px;
    bottom: -18px;
    width: 0;
    border-left: 1px solid var(--source-ref-line-color);
  }

  .tree-branch {
    display: none;
  }

  &.tree-child-last {
    margin-bottom: 0;

    &::before {
      content: none;
    }
  }
}

.tool-event {
  .action-card {
    position: relative;
    background: transparent;
    border: 0;
    box-shadow: none;
  }

  .action-header {
    display: flex;
    align-items: center;
    min-height: 24px;
    padding: 0;
    cursor: pointer;
    user-select: none;
  }

  .action-title {
    display: flex;
    align-items: center;
    gap: 8px;
    position: relative;
    flex: 0 1 auto;
    min-width: 0;
  }

  .action-title-icon {
    position: absolute;
    left: -42px;
    top: 3px;
    width: 18px;
    height: 18px;
    flex-shrink: 0;
    color: var(--source-ref-icon-color);
  }

  .action-name {
    font-size: 14px;
    line-height: 1.55;
    font-weight: 400;
    color: var(--td-text-color-secondary);
    word-break: break-word;
    max-width: min(680px, 100%);
  }

  .action-show-icon {
    color: var(--td-text-color-placeholder);
    font-size: 14px;
    flex-shrink: 0;
  }
}

.source-ref-list {
  display: flex;
  flex-direction: column;
  gap: 6px;
  padding-top: 4px;
}

.source-ref-item {
  width: 100%;
  min-height: 22px;
  display: grid;
  grid-template-columns: auto 14px minmax(0, 1fr) auto auto;
  align-items: center;
  justify-content: start;
  gap: 6px;
  padding: 0;
  border: 0;
  background: transparent;
  color: var(--td-text-color-secondary);
  font: inherit;
  text-align: left;
  cursor: default;

  &.is-clickable {
    cursor: pointer;
  }

  &.is-clickable:hover .source-ref-title {
    color: var(--td-brand-color);
  }
}

.source-ref-index {
  font-size: 13px;
  line-height: 1.5;
  color: var(--td-text-color-secondary);
}

.source-ref-icon,
.source-ref-jump {
  color: var(--td-text-color-placeholder);
  flex-shrink: 0;
}

.source-ref-title {
  min-width: 0;
  max-width: min(560px, 100%);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  font-size: 13px;
  line-height: 1.5;
  color: var(--td-text-color-secondary);
}

.source-ref-meta {
  font-size: 12px;
  line-height: 1.4;
  color: var(--td-text-color-placeholder);
  white-space: nowrap;
}

.is-embedded {
  .source-ref-title {
    max-width: min(420px, 100%);
  }
}

@media (max-width: 640px) {
  .tree-child {
    padding-left: 34px;

    &::before {
      left: 8px;
    }
  }

  .tool-event .action-title-icon {
    left: -34px;
  }

  .source-ref-item {
    grid-template-columns: auto 14px minmax(0, 1fr) auto;
  }

  .source-ref-jump {
    display: none;
  }

  .source-ref-title {
    max-width: 100%;
  }
}
</style>
