/** Shared citation tag preprocessing for chat markdown (QA + agent). */
import {
  buildCitedSourceReferenceItems,
  buildSourceReferenceHref,
  findSourceReferenceItem,
  getSourceReferenceKind,
  type SourceReference,
  type SourceReferenceItem,
} from './sourceReferences.ts'

/** The only citation tag accepted from model output. */
export const SOURCE_CITATION_TAG_RE = /<src id="S[1-9][0-9]*" \/>/g
const SRC_TAG_ATTR_RE = /<src id="(S[1-9][0-9]*)" \/>/g
const SOURCE_CITATION_TAG_EXACT_RE = /^<src id="S[1-9][0-9]*" \/>$/
const CITATION_LIKE_TAG_RE = /<\/?(?:src|source|citation|doc|document|kb|wiki|web)\b[^>]*>/gi
const MARKDOWN_CODE_RE = /```[\s\S]*?```|~~~[\s\S]*?~~~|`[^`\n]*`/g
const WIKI_HANDLE_RE = /\[\[([^\]|\n]+)(?:\|([^\]\n]+))?\]\]/g
const ADJACENT_SAME_CITATION_RE = /(<src id="(S[1-9][0-9]*)" \/>)(\s*)<src id="\2" \/>/g

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

  const partial = tail.match(/^<\/?([a-z]*)(?:\s[\s\S]*)?$/i)
  const partialName = partial?.[1]?.toLowerCase() || ''
  const isCitationPrefix = tail === '<' || tail === '</' || (Boolean(partialName) && [
    'src', 'source', 'citation', 'doc', 'kb', 'wiki', 'web',
  ].some((name) => name.startsWith(partialName)))

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

function escapeHtml(text: string): string {
  return String(text)
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
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
  if (!list.length) return ''

  const kbId = String(attrs.kbId || '').trim()
  const exact = list.find((ref) => {
    if (kbId && ref.knowledge_base_id && ref.knowledge_base_id !== kbId) return false
    return String(ref.id || '').trim() === raw
      || String(ref.metadata?.chunk_id || '').trim() === raw
  })
  return exact?.metadata?.chunk_id || exact?.id || ''
}

/**
 * Filter unsupported citation syntax during streaming. Final responses are
 * filtered by the backend authority as well; this keeps an invalid completed
 * tag from flashing before that final event arrives. Code examples remain
 * literal text and are never treated as citations.
 */
export function stripUnsupportedCitationTags(content: string): string {
  if (!content) return content

  const filterSegment = (segment: string): string => {
    let filtered = segment.replace(CITATION_LIKE_TAG_RE, (tag) => (
      SOURCE_CITATION_TAG_EXACT_RE.test(tag) ? tag : ''
    ))
    .replace(WIKI_HANDLE_RE, (_value, slug: string, label?: string) => (
      String(label || slug || '').trim()
    ))
    let previous = ''
    while (filtered !== previous) {
      previous = filtered
      filtered = filtered.replace(ADJACENT_SAME_CITATION_RE, '$1')
    }
    return filtered
  }

  let output = ''
  let start = 0
  MARKDOWN_CODE_RE.lastIndex = 0
  let match: RegExpExecArray | null
  while ((match = MARKDOWN_CODE_RE.exec(content)) !== null) {
    output += filterSegment(content.slice(start, match.index))
    output += match[0]
    start = match.index + match[0].length
  }
  output += filterSegment(content.slice(start))
  return output
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
  const safeHref = escapeHtml(buildSourceReferenceHref(item))
  const attributes = `class="citation citation-source citation-source--${safeType}" data-source-id="${safeSourceId}" data-citation-number="${safeNumber}" data-source-type="${safeType}" data-title="${safeTitle}" data-source-label="${safeSourceLabel}" data-kb-id="${safeKbId}" data-knowledge-id="${safeKnowledgeId}" data-chunk-id="${safeChunkId}" data-chunk-index="${safeChunkIndex}" data-slug="${safeSlug}" data-url="${safeUrl}" data-data-source-id="${safeDataSourceId}" tabindex="0" aria-label="引用 ${safeNumber}：${safeTitle}" title="${safeTitle}"`
  if (safeHref) {
    return `<a ${attributes} href="${safeHref}"><span class="citation-number">${safeNumber}</span></a>`
  }
  return `<span ${attributes} role="button"><span class="citation-number">${safeNumber}</span></span>`
}

/** Convert canonical source handles into inline citation HTML. */
export function preprocessCitationTags(
  contentStr: string,
  refs?: CitationKnowledgeRef[] | null,
  sourceCitationNumberById?: Map<string, number>,
): string {
  if (!contentStr.trim()) return ''

  const replaceSourceTag = (_match: string, sourceId: string): string => {
    const item = findSourceReferenceItem(refs, sourceId)
    if (!item) return ''
    return renderSourceCitation(item, sourceId, sourceCitationNumberById)
  }

  return contentStr.replace(SRC_TAG_ATTR_RE, replaceSourceTag)
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

  const content = contentStr.replace(
    SOURCE_CITATION_TAG_RE,
    (match) => storeHtml(preprocessCitationTags(match, refs, sourceCitationNumberById)),
  )

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
      /(<src\b[^>]*?\s*\/?>)\s*\n+\s*(<src\b)/gi,
      '$1 $2',
    )
  }

  // Blank lines before citations: join to the previous content. Fenced-code
  // delimiters are the only exception because ``` / ~~~ must stay on their own line.
  result = result.replace(/\n[ \t]*\n+([ \t]*<src\b)/gi, (match, srcStart, offset, full) => {
    const before = full.slice(0, offset)
    const lastLine = before.split('\n').filter((line: string) => line.trim()).pop() || ''
    if (isFencedCodeDelimiterLine(lastLine)) {
      return `\n\n${srcStart}`
    }
    return ` ${srcStart.trimStart()}`
  })

  // Single newline before citation when it follows text or another citation (not after a blank line)
  result = result.replace(
    /(?<!\n)(<src\b[^>]*?\s*\/?>|[ \t]*\S[^\n]*?)\n([ \t]*<src\b)/g,
    (match, beforePart: string, srcStart: string, offset: number, full: string) => {
      // Resolve the full preceding line: lazy capture + lookbehind can grab only a
      // partial line (e.g. ``` captured as ``), which would skip the fence check.
      const lineStart = full.lastIndexOf('\n', offset - 1) + 1
      const fullPrevLine = full.slice(lineStart, offset + beforePart.length)
      if (isFencedCodeDelimiterLine(fullPrevLine)) {
        return match
      }
      return `${beforePart} ${srcStart.trimStart()}`
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

/** Preserve canonical source handles before markdown transformations. */
export function preserveCitationTags(contentStr: string): { text: string; tags: string[] } {
  const tags: string[] = []
  const text = contentStr.replace(SOURCE_CITATION_TAG_RE, (match) => {
    const idx = tags.length
    tags.push(match)
    return `\x00TAG${idx}\x00`
  })
  return { text, tags }
}

export function restoreCitationTags(text: string, tags: string[]): string {
  return text.replace(/\x00TAG(\d+)\x00/g, (_, idx) => tags[Number(idx)] || '')
}
