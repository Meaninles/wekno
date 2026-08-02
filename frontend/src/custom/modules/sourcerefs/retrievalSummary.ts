export type RetrievalStats = {
  attempted: boolean
  documents: number
  wiki: number
  web: number
  dataSources: number
  total: number
  unit: 'documents' | 'data_sources'
}

const EVIDENCE_RETRIEVAL_TOOLS = [
  'knowledge_search',
  'search_knowledge',
  'grep_chunks',
  'list_knowledge_chunks',
  'wiki_read_page',
  'wiki_read_source_doc',
  'web_search',
  'web_fetch',
  'data_analysis',
  'table_analysis',
  'db_query',
] as const

export function retrievalStatsFromMessage(message: Record<string, any> | undefined): RetrievalStats | null {
  const raw = message?.retrieval_stats
  if (!raw || typeof raw !== 'object') return null
  return {
    attempted: raw.attempted === true,
    documents: Number(raw.documents) || 0,
    wiki: Number(raw.wiki) || 0,
    web: Number(raw.web) || 0,
    dataSources: Number(raw.data_sources) || 0,
    total: Number(raw.total) || 0,
    unit: raw.unit === 'data_sources' ? 'data_sources' : 'documents',
  }
}

/** Structured table/database evidence is a data source, never a document. */
export function usesDataSourceRetrievalUnit(stats: RetrievalStats | null | undefined): boolean {
  return Boolean(stats && (stats.unit === 'data_sources' || stats.dataSources > 0))
}

export function isEvidenceRetrievalToolName(toolName: unknown): boolean {
  const name = typeof toolName === 'string' ? toolName.trim().toLowerCase() : ''
  return EVIDENCE_RETRIEVAL_TOOLS.some((canonical) =>
    name === canonical || name.endsWith(`__${canonical}`) || name.endsWith(`_${canonical}`),
  )
}

/** Completed summaries use the backend authority so live and reloaded views agree. */
export function agentToolCountFromMessage(message: Record<string, any> | undefined): number {
  const authoritative = Number(message?.agent_tool_count)
  if (Number.isFinite(authoritative) && authoritative >= 0) {
    return Math.trunc(authoritative)
  }
  const stream = Array.isArray(message?.agentEventStream) ? message.agentEventStream : []
  return stream.filter((event: any) =>
    event?.type === 'tool_call' && !isEvidenceRetrievalToolName(event.tool_name)
  ).length
}

/** A direct text response has no citation/retrieval/tool telemetry to explain. */
export function isSimpleCompletedConversation(message: Record<string, any> | undefined): boolean {
  if (message?.is_completed !== true || !String(message?.content || '').trim()) return false
  // A ReAct answer remains an agent run even when it needed no retrieval or
  // non-retrieval tools. Keep its authoritative zero-state telemetry visible.
  if (message?.agent_mode === true) return false
  const stats = retrievalStatsFromMessage(message)
  if (!stats || stats.attempted || stats.total > 0) return false
  if (agentToolCountFromMessage(message) > 0) return false
  return !Array.isArray(message?.knowledge_references) || message.knowledge_references.length === 0
}

/**
 * Format a completed answer's authoritative backend duration without decimal
 * units. Keeping this beside retrieval summary construction gives Web, embed,
 * and mobile one shared presentation rule while preserving millisecond
 * precision in persisted data and diagnostics.
 */
export function formatCompletedRunDuration(ms: number | undefined | null): string {
  const milliseconds = Number(ms) || 0
  if (milliseconds <= 0) return ''
  if (milliseconds < 1000) return `${Math.max(0, Math.round(milliseconds))}ms`
  const seconds = Math.round(milliseconds / 1000)
  if (seconds < 60) return `${seconds}s`
  const minutes = Math.floor(seconds / 60)
  const remainingSeconds = seconds % 60
  return remainingSeconds > 0 ? `${minutes}m ${remainingSeconds}s` : `${minutes}m`
}
