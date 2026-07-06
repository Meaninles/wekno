export type SourceReferenceKind = 'knowledge' | 'wiki' | 'web' | 'data_source'

export type SourceReference = {
  id?: string
  content?: string
  knowledge_id?: string
  knowledge_title?: string
  knowledge_filename?: string
  knowledge_base_id?: string
  chunk_type?: string
  metadata?: Record<string, string>
}

export type SourceReferenceItem = {
  key: string
  number: number
  citationId: string
  type: SourceReferenceKind
  title: string
  sourceLabel: string
  snippet: string
  count: number
  icon: string
  url: string
  knowledgeBaseId: string
  knowledgeId: string
  slug: string
  sourceId: string
  clickable: boolean
}

const CITATION_ID_RE = /^S(\d+)$/i
const SRC_TAG_RE = /<src\b([^>]*?)\s*\/?>/gi
const SRC_ID_ATTR_RE = /\b(?:id|source_id|sourceId)\s*=\s*"([^"]*)"/i

export function getSourceReferenceKind(ref: SourceReference): SourceReferenceKind {
  const metadataType = ref.metadata?.source_type
  if (metadataType === 'wiki') return 'wiki'
  if (metadataType === 'web') return 'web'
  if (metadataType === 'data_source') return 'data_source'
  if (ref.chunk_type === 'wiki_page') return 'wiki'
  if (ref.chunk_type === 'web_search') return 'web'
  if (ref.chunk_type === 'data_source') return 'data_source'
  return 'knowledge'
}

export function buildSourceReferenceItems(
  refs?: SourceReference[] | null,
): SourceReferenceItem[] {
  const grouped = new Map<string, SourceReferenceItem>()
  const firstSeen = new Map<string, number>()
  let seenIndex = 0

  for (const ref of refs || []) {
    if (!ref) continue
    const type = getSourceReferenceKind(ref)
    const metadata = ref.metadata || {}
    const citationId = metadata.citation_id || ''
    const key = citationId || fallbackSourceKey(ref, type)
    if (!key) continue

    if (!firstSeen.has(key)) {
      firstSeen.set(key, seenIndex++)
    }

    const existing = grouped.get(key)
    if (existing) {
      existing.count += 1
      if (!existing.snippet) existing.snippet = summarizeContent(ref.content || '')
      continue
    }

    const url = metadata.url || (isHttpUrl(ref.id) ? ref.id || '' : '')
    const slug = metadata.slug || stripWikiID(ref.id || '')
    const knowledgeBaseId = ref.knowledge_base_id || metadata.knowledge_base_id || ''
    const knowledgeId = ref.knowledge_id || metadata.knowledge_id || ''
    const sourceId = metadata.source_id || stripDataSourceID(ref.id || '')
    const title = normalizeDisplayText(sourceTitle(ref, type, url, slug, sourceId))
    const sourceLabel = normalizeDisplayText(sourceLabelFor(ref, type, url))

    grouped.set(key, {
      key,
      number: 0,
      citationId,
      type,
      title,
      sourceLabel,
      snippet: summarizeContent(ref.content || ''),
      count: 1,
      icon: iconForSourceType(type),
      url,
      knowledgeBaseId,
      knowledgeId,
      slug,
      sourceId,
      clickable: isClickable(type, {
        url,
        knowledgeBaseId,
        knowledgeId,
        slug,
        sourceId,
      }),
    })
  }

  return Array.from(grouped.values())
    .sort((a, b) => compareSourceItems(a, b, firstSeen))
    .map((item, index) => ({ ...item, number: index + 1 }))
}

export function findSourceReferenceItem(
  refs: SourceReference[] | null | undefined,
  citationId: string,
): SourceReferenceItem | undefined {
  const id = String(citationId || '').trim()
  if (!id) return undefined
  return buildSourceReferenceItems(refs).find((item) => item.citationId === id)
}

export function buildCitedSourceReferenceItems(
  refs: SourceReference[] | null | undefined,
  content: string,
  includeFallback: boolean,
): SourceReferenceItem[] {
  const allItems = buildSourceReferenceItems(refs)
  if (!allItems.length) return []
  if (!String(content || '').trim()) return []

  const byCitationId = new Map<string, SourceReferenceItem>()
  for (const item of allItems) {
    if (item.citationId) byCitationId.set(item.citationId, item)
  }

  const citedItems: SourceReferenceItem[] = []
  const seen = new Set<string>()
  for (const citationId of extractSourceCitationIds(content)) {
    const item = byCitationId.get(citationId)
    if (!item || seen.has(item.key)) continue
    seen.add(item.key)
    citedItems.push(item)
  }
  if (citedItems.length > 0) return renumberSourceItems(citedItems)

  if (!includeFallback || hasLegacyInlineCitation(content)) return []
  return allItems
    .filter((item) => item.type !== 'data_source' && item.citationId)
    .slice(0, 6)
    .map((item, index) => ({ ...item, number: index + 1 }))
}

export function extractSourceCitationIds(content: string): string[] {
  const ids: string[] = []
  const seen = new Set<string>()
  SRC_TAG_RE.lastIndex = 0
  let match: RegExpExecArray | null
  while ((match = SRC_TAG_RE.exec(String(content || ''))) !== null) {
    const attrMatch = (match[1] || '').match(SRC_ID_ATTR_RE)
    const id = attrMatch?.[1]?.trim()
    if (!id || seen.has(id)) continue
    seen.add(id)
    ids.push(id)
  }
  return ids
}

export function sourceTypeLabel(type: SourceReferenceKind): string {
  if (type === 'web') return '网页'
  if (type === 'wiki') return 'Wiki'
  if (type === 'data_source') return '数据源'
  return '知识库文档'
}

export function hostFromUrl(value?: string): string {
  if (!value) return ''
  try {
    return new URL(value).hostname
  } catch {
    return ''
  }
}

export function isHttpUrl(value?: string): boolean {
  return Boolean(value && (value.startsWith('http://') || value.startsWith('https://')))
}

function compareSourceItems(
  a: SourceReferenceItem,
  b: SourceReferenceItem,
  firstSeen: Map<string, number>,
): number {
  const aNum = citationNumber(a.citationId)
  const bNum = citationNumber(b.citationId)
  if (aNum !== null && bNum !== null && aNum !== bNum) return aNum - bNum
  if (aNum !== null && bNum === null) return -1
  if (aNum === null && bNum !== null) return 1
  return (firstSeen.get(a.key) ?? 0) - (firstSeen.get(b.key) ?? 0)
}

function citationNumber(citationId: string): number | null {
  const match = citationId.match(CITATION_ID_RE)
  if (!match) return null
  return Number(match[1])
}

function fallbackSourceKey(ref: SourceReference, type: SourceReferenceKind): string {
  const metadata = ref.metadata || {}
  if (type === 'web') {
    const url = metadata.url || (isHttpUrl(ref.id) ? ref.id : '')
    return `web:${url || metadata.title || ref.knowledge_title || ref.id || ''}`
  }
  if (type === 'wiki') {
    const slug = metadata.slug || stripWikiID(ref.id || '')
    const kbId = ref.knowledge_base_id || metadata.knowledge_base_id || ''
    return `wiki:${kbId}:${slug || ref.knowledge_title || ref.id || ''}`
  }
  if (type === 'data_source') {
    return `data_source:${metadata.source_id || stripDataSourceID(ref.id || '') || metadata.source_name || ref.id || ''}`
  }
  return `knowledge:${ref.knowledge_base_id || metadata.knowledge_base_id || ''}:${ref.knowledge_id || metadata.knowledge_id || ref.knowledge_title || ref.knowledge_filename || ref.id || ''}`
}

function sourceTitle(ref: SourceReference, type: SourceReferenceKind, url: string, slug: string, sourceId: string): string {
  const metadata = ref.metadata || {}
  return metadata.citation_title
    || metadata.source_name
    || metadata.title
    || ref.knowledge_title
    || ref.knowledge_filename
    || (type === 'wiki' ? slug : '')
    || (type === 'web' ? hostFromUrl(url) || url : '')
    || (type === 'data_source' ? sourceId : '')
    || ref.id
    || sourceTypeLabel(type)
}

function sourceLabelFor(ref: SourceReference, type: SourceReferenceKind, url: string): string {
  const metadata = ref.metadata || {}
  if (type === 'web') return hostFromUrl(url) || metadata.source || '网页'
  if (type === 'wiki') return metadata.knowledge_base_name || 'Wiki'
  if (type === 'data_source') return metadata.database_type || metadata.source_name || '数据源'
  return metadata.knowledge_base_name || metadata.source_name || '知识库文档'
}

function iconForSourceType(type: SourceReferenceKind): string {
  if (type === 'web') return 'internet'
  if (type === 'wiki') return 'browse'
  if (type === 'data_source') return 'server'
  return 'file'
}

function isClickable(
  type: SourceReferenceKind,
  info: {
    url: string
    knowledgeBaseId: string
    knowledgeId: string
    slug: string
    sourceId: string
  },
): boolean {
  if (type === 'web') return Boolean(info.url)
  if (type === 'wiki') return Boolean(info.knowledgeBaseId && info.slug)
  if (type === 'data_source') return Boolean(info.sourceId)
  return Boolean(info.knowledgeBaseId)
}

function stripWikiID(value: string): string {
  return value.replace(/^wiki:[^:]*:/, '')
}

function stripDataSourceID(value: string): string {
  return value.replace(/^data_source:/, '')
}

function hasLegacyInlineCitation(content: string): boolean {
  const text = String(content || '')
  return /<kb\b([^>]*?)\s*\/?>/i.test(text)
    || /<web\b([^>]*?)\s*\/?>/i.test(text)
    || /\[\[([^\]]+)\]\]/.test(text)
}

function renumberSourceItems(items: SourceReferenceItem[]): SourceReferenceItem[] {
  return items.map((item, index) => ({ ...item, number: index + 1 }))
}

function summarizeContent(content: string): string {
  const text = String(content || '')
    .replace(/<[^>]+>/g, ' ')
  const normalized = normalizeDisplayText(text)
    .replace(/\s+/g, ' ')
  if (normalized.length <= 120) return normalized
  return `${normalized.slice(0, 120)}...`
}

function normalizeDisplayText(value: string): string {
  return String(value || '')
    .replace(/\uFFFD/g, '')
    .replace(/[\u0000-\u0008\u000B\u000C\u000E-\u001F\u007F]/g, '')
    .replace(/\s+/g, ' ')
    .trim()
}
