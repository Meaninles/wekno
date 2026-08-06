import type { ComposerTranslation } from 'vue-i18n'

function normalizedToolName(toolName: unknown): string {
  return typeof toolName === 'string' ? toolName.trim().toLowerCase() : ''
}

/**
 * Sidecar runtimes may preserve the MCP namespace in a tool event while the
 * native agent emits the canonical short name. Treat both as the same
 * capability so every agent type gets the same user-facing retrieval UI.
 */
export function isKnowledgeSearchToolName(toolName: unknown): boolean {
  const name = normalizedToolName(toolName)
  return name === 'knowledge_search' ||
    name === 'search_knowledge' ||
    name.endsWith('__knowledge_search') ||
    name.endsWith('__search_knowledge') ||
    name.endsWith('_knowledge_search') ||
    name.endsWith('_search_knowledge')
}

export function isWebSearchToolName(toolName: unknown): boolean {
  const name = normalizedToolName(toolName)
  return name === 'web_search' ||
    name.endsWith('__web_search') ||
    name.endsWith('_web_search')
}

function collectQueryStrings(value: unknown): string[] {
  if (value == null) return []

  if (typeof value === 'string') {
    const trimmed = value.trim()
    if (!trimmed) return []
    if (trimmed.startsWith('[')) {
      try {
        const parsed = JSON.parse(trimmed)
        if (Array.isArray(parsed)) {
          return parsed.filter((q): q is string => typeof q === 'string' && Boolean(q.trim()))
        }
      } catch {
        // fall through to treat as a single query string
      }
    }
    return [trimmed]
  }

  if (Array.isArray(value)) {
    return value.filter((q): q is string => typeof q === 'string' && Boolean(q.trim()))
  }

  return []
}

export function getQueryText(args: unknown): string {
  if (!args) return ''

  let parsedArgs = args
  if (typeof parsedArgs === 'string') {
    try {
      parsedArgs = JSON.parse(parsedArgs)
    } catch {
      return ''
    }
  }

  if (!parsedArgs || typeof parsedArgs !== 'object') return ''

  const queries: string[] = []
  const record = parsedArgs as Record<string, unknown>

  queries.push(...collectQueryStrings(record.query))
  queries.push(...collectQueryStrings(record.queries))

  return Array.from(new Set(queries)).join('，')
}

export function getWikiPageText(args: unknown): string {
  if (!args) return ''

  let parsedArgs = args
  if (typeof parsedArgs === 'string') {
    try {
      parsedArgs = JSON.parse(parsedArgs)
    } catch {
      return ''
    }
  }

  if (!parsedArgs || typeof parsedArgs !== 'object') return ''

  const record = parsedArgs as Record<string, unknown>
  const slugs = [
    ...collectQueryStrings(record.slug),
    ...collectQueryStrings(record.slugs),
  ]
  return Array.from(new Set(slugs)).join('、')
}

export function getKnowledgeSearchSummaryHtml(
  _t: ComposerTranslation,
  _toolData: Record<string, unknown> | null | undefined,
): string {
  // Retrieval counts are diagnostic details rather than useful answer
  // content. Keep every successful knowledge lookup neutral and stable: the
  // step title alone says that retrieval completed, regardless of hit count.
  return ''
}

export function getWebSearchSummaryHtml(
  t: ComposerTranslation,
  toolData: Record<string, unknown> | null | undefined,
): string {
  if (!toolData) return ''

  const results = toolData.results
  const count = (Array.isArray(results) ? results.length : 0) || Number(toolData.count) || 0
  if (count === 0) return ''

  return t('agentStream.search.webResults', { count: `<strong>${count}</strong>` })
}

type RagPipelineEvent = {
  tool_name?: string
  pending?: boolean
  success?: boolean
  arguments?: unknown
  tool_data?: Record<string, unknown> | null
}

export function getRagPipelineStepTitle(t: ComposerTranslation, event: RagPipelineEvent): string {
  const toolName = String(event.tool_name || '')
  const pending = event.pending === true
  const query =
    getQueryText(event.arguments) ||
    getQueryText(event.tool_data)

  if (toolName === 'query_understand') {
    return pending
      ? t('agentStream.toolStatus.queryUnderstanding')
      : t('agentStream.toolStatus.queryUnderstandDone')
  }

  if (isKnowledgeSearchToolName(toolName)) {
    if (pending) {
      return query
        ? t('agentStream.ragPipeline.searchingWithQuery', { query })
        : t('agentStream.ragPipeline.searching')
    }

    if (event.success !== false) {
      return t('agentStream.ragPipeline.searchDone')
    }

    const baseTitle = t('agentStream.toolStatus.searchKbFailed')
    return query ? `${baseTitle}：「${query}」` : baseTitle
  }

  if (isWebSearchToolName(toolName)) {
    const baseTitle = event.success === false
      ? t('agentStream.toolStatus.webSearchFailed')
      : t('agentStream.toolStatus.webSearch')
    return query ? `${baseTitle}：「${query}」` : baseTitle
  }

  return ''
}
