/** Shared citation tag preprocessing for chat markdown (QA + agent). */
import {
  buildCitedSourceReferenceItems,
  findSourceReferenceItem,
  getSourceReferenceKind,
  type SourceReference,
  type SourceReferenceItem,
} from './sourceReferences.ts'

/**
 * Citation tags accepted from model output.
 *
 * `<src/>` is the canonical protocol. `<kb/>` and `<web/>` remain supported by
 * their existing renderers; model output must never invent a `<doc/>` tag.
 */
export const KB_WEB_TAG_RE = /<(?:kb|web|src)\b[^>]*?\s*\/?>/gi
const KB_TAG_ATTR_RE = /<kb\b([^>]*?)\s*\/?>/g
const WEB_TAG_ATTR_RE = /<web\b([^>]*?)\s*\/?>/g
const SRC_TAG_ATTR_RE = /<src\b([^>]*?)\s*\/?>/g

const ATTRIBUTE_REGEX = /([\w-]+)\s*=\s*"([^"]*)"/g
const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i

/**
 * Hide a citation tag while the typewriter has only emitted part of it.
 *
 * Without this guard, Markdown renders the leading `<` as ordinary text until
 * the closing `>` arrives. Only the unfinished tail is removed; a complete tag
 * continues through the normal citation pipeline.
 */
export function stripIncompleteCitationTag(content: string): string {
  if (!content) return content

  const start = content.lastIndexOf('<')
  if (start < 0) return content

  const tail = content.slice(start)
  if (tail.includes('>')) return content

  const isCitationPrefix = tail === '<'
    || /^<k(?:b(?:\s[\s\S]*)?)?$/i.test(tail)
    || /^<w(?:e(?:b(?:\s[\s\S]*)?)?)?$/i.test(tail)
    || /^<s(?:r(?:c(?:\s[\s\S]*)?)?)?$/i.test(tail)

  return isCitationPrefix ? content.slice(0, start) : content
}

export type CitationKnowledgeRef = SourceReference & {
  id?: string
  knowledge_id?: string
  knowledge_title?: string
  knowledge_filename?: string
  chunk_index?: number
  chunk_type?: string
  knowledge_base_id?: string
  metadata?: Record<string, string>
}

function parseTagAttributes(attrString: string): Record<string, string> {
  const attributes: Record<string, string> = {}
  if (!attrString) return attributes
  ATTRIBUTE_REGEX.lastIndex = 0
  let match: RegExpExecArray | null
  while ((match = ATTRIBUTE_REGEX.exec(attrString)) !== null) {
    attributes[match[1]] = match[2]
  }
  return attributes
}

function escapeHtml(text: string): string {
  return String(text)
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
}

function truncateMiddle(text: string, maxLength = 13): string {
  if (!text) return ''
  if (text.length <= maxLength) return text
  const half = Math.floor((maxLength - 3) / 2)
  const start = text.slice(0, half + ((maxLength - 3) % 2))
  const end = text.slice(-half)
  return `${start}...${end}`
}

function sourceType(ref: CitationKnowledgeRef): string {
  return getSourceReferenceKind(ref)
}

function findSourceRef(sourceId: string, refs?: CitationKnowledgeRef[] | null): CitationKnowledgeRef | undefined {
  const id = sourceId.trim()
  if (!id) return undefined
  return (refs || []).find((ref) => ref?.metadata?.citation_id === id)
}

/** Resolve only an exact, message-bound chunk identifier. */
export function resolveCitationChunkId(
  rawChunkId: string,
  attrs: { doc?: string; kbId?: string },
  refs?: CitationKnowledgeRef[] | null,
): string {
  const raw = String(rawChunkId || '').trim()
  if (!raw) return ''

  const list = (refs || []).filter((r) => r && r.chunk_type !== 'web_search')
  if (!list.length) return UUID_RE.test(raw) ? raw : ''

  const kbId = String(attrs.kbId || '').trim()
  const exact = list.find((ref) => {
    if (kbId && ref.knowledge_base_id && ref.knowledge_base_id !== kbId) return false
    return String(ref.id || '').trim() === raw
      || String(ref.metadata?.chunk_id || '').trim() === raw
  })
  return exact?.metadata?.chunk_id || exact?.id || ''
}

function renderSourceCitation(
  item: SourceReferenceItem,
  sourceId: string,
  sourceCitationNumberById?: Map<string, number>,
): string {
  const safeSourceId = escapeHtml(item.citationId || sourceId)
  const displayNumber = sourceCitationNumberById?.get(item.citationId) || item.number
  const safeNumber = escapeHtml(String(displayNumber))
  const safeType = escapeHtml(item.type)
  const safeTitle = escapeHtml(item.title)
  const safeSourceLabel = escapeHtml(item.sourceLabel)
  const safeKbId = escapeHtml(item.knowledgeBaseId)
  const safeKnowledgeId = escapeHtml(item.knowledgeId)
  const safeChunkId = escapeHtml(item.chunkId)
  const safeChunkIndex = item.chunkIndex === null ? '' : escapeHtml(String(item.chunkIndex))
  const safeSlug = escapeHtml(item.slug)
  const safeUrl = escapeHtml(item.url)
  const safeDataSourceId = escapeHtml(item.sourceId)
  return `<span class="citation citation-source citation-source--${safeType}" data-source-id="${safeSourceId}" data-citation-number="${safeNumber}" data-source-type="${safeType}" data-title="${safeTitle}" data-source-label="${safeSourceLabel}" data-kb-id="${safeKbId}" data-knowledge-id="${safeKnowledgeId}" data-chunk-id="${safeChunkId}" data-chunk-index="${safeChunkIndex}" data-slug="${safeSlug}" data-url="${safeUrl}" data-data-source-id="${safeDataSourceId}" role="button" tabindex="0" aria-label="引用 ${safeNumber}：${safeTitle}" title="${safeTitle}"><span class="citation-number">${safeNumber}</span></span>`
}

/** Convert <web/> / <kb/> / [[wiki]] tags into inline citation HTML. */
export function preprocessCitationTags(
  contentStr: string,
  refs?: CitationKnowledgeRef[] | null,
  sourceCitationNumberById?: Map<string, number>,
): string {
  if (!contentStr.trim()) return ''

  const replaceSourceTag = (_match: string, attrString: string): string => {
      const attrs = parseTagAttributes(attrString)
      const sourceId = attrs.id || attrs.source_id || attrs.sourceId || ''
      const item = findSourceReferenceItem(refs, sourceId)
      if (!item) return ''
      return renderSourceCitation(item, sourceId, sourceCitationNumberById)
  }

  return contentStr
    .replace(SRC_TAG_ATTR_RE, replaceSourceTag)
    .replace(WEB_TAG_ATTR_RE, (_m, attrString: string) => {
      const attrs = parseTagAttributes(attrString)
      const url = attrs.url || ''
      const title = attrs.title || ''
      if (!url) return ''

      let domain = url
      try {
        const u = new URL(url)
        const host = u.hostname || ''
        const parts = host.split('.')
        domain = parts.length >= 2 ? parts.slice(-2).join('.') : host || url
      } catch {
        // keep original
      }
      const safeTitle = escapeHtml(title)
      const safeUrl = escapeHtml(url)
      return `<a class="citation citation-web" data-url="${safeUrl}" href="${safeUrl}" target="_blank" rel="noopener noreferrer"><span class="citation-icon citation-icon--web" aria-hidden="true"></span><span class="citation-domain">${domain}</span><span class="citation-tip"><span class="tip-title">${safeTitle}</span><span class="tip-url">${safeUrl}</span></span></a>`
    })
    .replace(KB_TAG_ATTR_RE, (_m, attrString: string) => {
      const attrs = parseTagAttributes(attrString)
      const doc = attrs.doc || ''
      const rawChunkId = attrs.chunk_id || attrs.chunkId || ''
      const kbId = attrs.kb_id || attrs.kbId || ''
      const chunkId = resolveCitationChunkId(rawChunkId, { doc, kbId }, refs)
      if (!doc || !chunkId) return ''

      const safeDoc = escapeHtml(doc)
      const safeKbId = escapeHtml(kbId)
      const safeChunkId = escapeHtml(chunkId)
      const displayDoc = escapeHtml(truncateMiddle(doc))
      return `<span class="citation citation-kb" data-kb-id="${safeKbId}" data-chunk-id="${safeChunkId}" data-doc="${safeDoc}" role="button" tabindex="0"><span class="citation-icon citation-icon--book" aria-hidden="true"></span><span class="citation-text">${displayDoc}</span><span class="citation-tip"><span class="tip-loading">…</span></span></span>`
    })
    .replace(/\[\[([^\]]+)\]\]/g, (match, inner: string) => {
      const pipeIdx = inner.indexOf('|')
      const slug = pipeIdx > 0 ? inner.substring(0, pipeIdx).trim() : inner.trim()
      if (!slug) return match
      let display = slug
      if (pipeIdx > 0) {
        display = inner.substring(pipeIdx + 1).trim()
      } else {
        const parts = slug.split('/')
        display = parts.length > 1 ? parts.slice(1).join('/') : slug
      }
      return `<a href="#" class="wiki-content-link citation-wiki" data-slug="${escapeHtml(slug)}">${escapeHtml(display)}</a>`
    })
}

const HTML_PLACEHOLDER_RE = /@@WEKNORA_HTML_PLACEHOLDER_(\d+)@@/g

/** Protect citation HTML from markdown parser; restore after marked.parse. */
export function extractCitationHtmlPlaceholders(
  contentStr: string,
  refs?: CitationKnowledgeRef[] | null,
): { content: string; htmlSnippets: string[] } {
  const htmlSnippets: string[] = []
  const sourceCitationNumberById = new Map(
    buildCitedSourceReferenceItems(refs, contentStr)
      .map((item) => [item.citationId, item.number] as const),
  )
  const storeHtml = (html: string): string => {
    const idx = htmlSnippets.length
    htmlSnippets.push(html)
    return `@@WEKNORA_HTML_PLACEHOLDER_${idx}@@`
  }

  const content = contentStr
    .replace(KB_WEB_TAG_RE, (match) => storeHtml(preprocessCitationTags(match, refs, sourceCitationNumberById)))
    .replace(/\[\[([^\]]+)\]\]/g, (match) => storeHtml(preprocessCitationTags(match, refs, sourceCitationNumberById)))

  return { content, htmlSnippets }
}

export function restoreCitationHtmlPlaceholders(html: string, htmlSnippets: string[]): string {
  if (!htmlSnippets.length) return html
  return html.replace(HTML_PLACEHOLDER_RE, (_match, idx) => htmlSnippets[Number(idx)] || '')
}

/** Opening/closing fence for GFM fenced code blocks (up to 3 spaces indent). */
const FENCED_CODE_DELIMITER_RE = /^ {0,3}(`{3,}|~{3,})(\s*\S.*)?\s*$/

function isFencedCodeDelimiterLine(line: string): boolean {
  return FENCED_CODE_DELIMITER_RE.test(line)
}

/** Collapse newlines around citation tags so marked keeps citations inline. */
export function joinCitationTagsToPreviousLine(content: string): string {
  if (!content) return content

  let result = content

  // Newlines between consecutive citation tags
  let prev = ''
  while (result !== prev) {
    prev = result
    result = result.replace(
      /(<(?:kb|web|src)\b[^>]*?\s*\/?>)\s*\n+\s*(<(?:kb|web|src)\b)/gi,
      '$1 $2',
    )
  }

  // Blank lines before citations: join to the previous content. Fenced-code
  // delimiters are the only exception because ``` / ~~~ must stay on their own line.
  result = result.replace(/\n[ \t]*\n+([ \t]*<(?:kb|web|src)\b)/gi, (match, kbStart, offset, full) => {
    const before = full.slice(0, offset)
    const lastLine = before.split('\n').filter((line: string) => line.trim()).pop() || ''
    if (isFencedCodeDelimiterLine(lastLine)) {
      return `\n\n${kbStart}`
    }
    return ` ${kbStart.trimStart()}`
  })

  // Single newline before citation when it follows text or another citation (not after a blank line)
  result = result.replace(
    /(?<!\n)(<(?:kb|web|src)\b[^>]*?\s*\/?>|[ \t]*\S[^\n]*?)\n([ \t]*<(?:kb|web|src)\b)/g,
    (match, beforePart: string, kbStart: string, offset: number, full: string) => {
      // Resolve the full preceding line: lazy capture + lookbehind can grab only a
      // partial line (e.g. ``` captured as ``), which would skip the fence check.
      const lineStart = full.lastIndexOf('\n', offset - 1) + 1
      const fullPrevLine = full.slice(lineStart, offset + beforePart.length)
      if (isFencedCodeDelimiterLine(fullPrevLine)) {
        return match
      }
      return `${beforePart} ${kbStart.trimStart()}`
    },
  )

  return result
}

const CITATION_HTML_FRAGMENT =
  '(?:<span class="citation\\b[^]*?</span>|<a class="citation\\b[^]*?</a>)'

/** Merge citation-only <p> blocks into the preceding paragraph (marked splits on newlines). */
export function collapseStandaloneCitationParagraphs(html: string): string {
  if (!html || !html.includes('citation')) return html

  const mergePattern = new RegExp(
    `(<\\/(?:p|li)>)\\s*(?:<p>\\s*<\\/p>\\s*)*<p>\\s*(${CITATION_HTML_FRAGMENT})\\s*<\\/p>`,
    'g',
  )

  let result = html
  let prev = ''
  while (result !== prev) {
    prev = result
    result = result.replace(mergePattern, (_match, closeTag: string, citation: string) => {
      return ` ${citation}${closeTag}`
    })
  }

  return result
}

/** Preserve raw <kb>/<web> tags before sanitizers that would strip them. */
export function preserveCitationTags(contentStr: string): { text: string; tags: string[] } {
  const tags: string[] = []
  const text = contentStr.replace(KB_WEB_TAG_RE, (match) => {
    const idx = tags.length
    tags.push(match)
    return `\x00TAG${idx}\x00`
  })
  return { text, tags }
}

export function restoreCitationTags(text: string, tags: string[]): string {
  return text.replace(/\x00TAG(\d+)\x00/g, (_, idx) => tags[Number(idx)] || '')
}
