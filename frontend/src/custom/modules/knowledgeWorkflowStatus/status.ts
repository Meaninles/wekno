export type KnowledgePollStatus = {
  parse_status?: string
  summary_status?: string
  enrichment_status?: string
  wiki_status?: string
}

export type KnowledgeWorkflowStatus =
  | 'pending'
  | 'processing'
  | 'cancelling'
  | 'deleting'
  | 'completed'
  | 'failed'
  | 'cancelled'
  | 'draft'
  | 'unknown'

export type KnowledgeDetailStatus =
  | KnowledgeWorkflowStatus
  | 'not_started'
  | 'disabled'
  | 'not_applicable'
  | 'skipped'
  | 'blocked'

export type KnowledgeWorkflowFeatures = {
  embedding?: boolean
  multimodal?: boolean
  summary?: boolean
  question?: boolean
  graph?: boolean
  wiki?: boolean
}

export type KnowledgeWorkflowFeatureSource = {
  summary_model_id?: string
  question_generation_config?: { enabled?: boolean } | null
  extract_config?: { enabled?: boolean } | null
  vlm_config?: { enabled?: boolean; model_id?: string } | null
  asr_config?: { enabled?: boolean; model_id?: string } | null
  indexing_strategy?: {
    vector_enabled?: boolean
    keyword_enabled?: boolean
    graph_enabled?: boolean
    wiki_enabled?: boolean
  } | null
}

type KnowledgeFeatureItem = {
  metadata?: unknown
}

const DERIVATIVE_IN_FLIGHT = new Set(['pending', 'processing'])
const DERIVATIVE_SUCCESS = new Set(['', 'none', 'completed', 'done', 'skipped'])

function normalize(status?: string): string {
  return String(status ?? '').trim().toLowerCase()
}

function asRecord(value: unknown): Record<string, unknown> | undefined {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return undefined
  return value as Record<string, unknown>
}

function metadataRecord(value: unknown): Record<string, unknown> | undefined {
  if (typeof value === 'string') {
    try {
      return asRecord(JSON.parse(value))
    } catch {
      return undefined
    }
  }
  return asRecord(value)
}

function optionalBoolean(value: unknown): boolean | undefined {
  return typeof value === 'boolean' ? value : undefined
}

function configuredModel(config: unknown): boolean {
  const record = asRecord(config)
  return record?.enabled === true && String(record.model_id ?? '').trim() !== ''
}

// Resolve the same per-document feature switches as the backend's
// ResolveProcessConfig. The metadata override matters: two documents in the
// same KB may intentionally enable different question/graph/multimodal work.
export function resolveKnowledgeWorkflowFeatures(
  source: KnowledgeWorkflowFeatureSource | null | undefined,
  item?: KnowledgeFeatureItem,
): KnowledgeWorkflowFeatures {
  if (!source) return {}

  const strategy = source.indexing_strategy || {}
  const metadata = metadataRecord(item?.metadata)
  const overrides = asRecord(metadata?.process_overrides)
  const questionOverride = asRecord(overrides?.question_generation_config)
  const extractOverride = asRecord(overrides?.extract_config)
  const vlmOverride = asRecord(overrides?.vlm_config)
  const asrOverride = asRecord(overrides?.asr_config)

  const baseQuestion = source.question_generation_config?.enabled === true
  const question = optionalBoolean(questionOverride?.enabled) ?? baseQuestion

  const baseExtract = source.extract_config?.enabled === true
  let graph = strategy.graph_enabled === true && baseExtract
  graph = optionalBoolean(overrides?.graph_enabled) ?? graph
  const effectiveExtract = optionalBoolean(extractOverride?.enabled) ?? baseExtract
  graph = graph && effectiveExtract

  const baseVLM = configuredModel(source.vlm_config)
  const effectiveVLM = vlmOverride
    ? configuredModel(vlmOverride)
    : baseVLM
  const multimodalEnabled = optionalBoolean(overrides?.enable_multimodel) ?? baseVLM
  const effectiveASR = asrOverride
    ? configuredModel(asrOverride)
    : configuredModel(source.asr_config)

  return {
    // Historical/native KB rows can omit indexing_strategy; backend upload
    // validation treats that shape as the default vector+keyword strategy.
    // Keep the status UI aligned instead of incorrectly rendering "N/A".
    embedding: (strategy.vector_enabled ?? true) || (strategy.keyword_enabled ?? true),
    multimodal: (multimodalEnabled && effectiveVLM) || effectiveASR,
    summary: String(source.summary_model_id ?? '').trim() !== '',
    question,
    graph,
    wiki: strategy.wiki_enabled === true,
  }
}

// Embedding/index persistence is part of the core document parse contract, not
// a derivative with a separate status column. Once parse_status is completed,
// an enabled index has completed as well; if persistence failed the core parse
// cannot legitimately reach completed.
export function resolveCoreIndexDetailStatus(
  parseStatus: string | undefined,
  enabled?: boolean,
): KnowledgeDetailStatus {
  if (enabled === false) return 'disabled'
  switch (normalize(parseStatus)) {
    case 'pending':
    case 'draft':
      return 'not_started'
    case 'processing':
    case 'finalizing':
      return 'processing'
    case 'completed':
      return 'completed'
    case 'failed':
      return 'blocked'
    case 'cancelled':
    case 'cancelling':
    case 'deleting':
      return 'cancelled'
    default:
      return 'unknown'
  }
}

// The persisted derivative columns deliberately use "none" both before a
// task is created and when no task is applicable. Feature configuration and
// the core lifecycle are therefore required to render it honestly.
export function resolveKnowledgeDetailStatus(
  status: string | undefined,
  parseStatus: string | undefined,
  enabled?: boolean,
): KnowledgeDetailStatus {
  switch (normalize(status)) {
    case 'skipped':
      return 'skipped'
    case 'pending':
      return 'pending'
    case 'processing':
    case 'running':
    case 'finalizing':
      return 'processing'
    case 'completed':
    case 'done':
      return 'completed'
    case 'failed':
    case 'degraded':
      return 'failed'
    case 'cancelled':
      return 'cancelled'
    case '':
    case 'none':
      break
    default:
      return 'unknown'
  }

  if (enabled === false) return 'disabled'
  switch (normalize(parseStatus)) {
    case 'pending':
    case 'processing':
    case 'finalizing':
    case 'cancelling':
    case 'draft':
      return 'not_started'
    case 'failed':
      return 'blocked'
    case 'cancelled':
    case 'deleting':
      return 'cancelled'
    case 'completed':
      return 'not_applicable'
    default:
      return 'unknown'
  }
}

function derivativeStatuses(item: KnowledgePollStatus): string[] {
  return [
    item.summary_status,
    item.enrichment_status,
    item.wiki_status,
  ].map(normalize)
}

export function isKnowledgeParseInFlight(status?: string): boolean {
  return status === 'pending' || status === 'processing' || status === 'finalizing'
}

// A document remains visibly active while any enabled derivative is active.
// This deliberately wins over a simultaneous derivative failure: the durable
// workflow may still recover the failed branch before all branches quiesce.
export function knowledgeNeedsStatusPolling(item: KnowledgePollStatus): boolean {
  if (isKnowledgeParseInFlight(item.parse_status)) return true
  return item.parse_status === 'completed' &&
    derivativeStatuses(item).some((status) => DERIVATIVE_IN_FLIGHT.has(status))
}

export function knowledgeHasDerivativeFailure(item: KnowledgePollStatus): boolean {
  return item.parse_status === 'completed' &&
    !derivativeStatuses(item).some((status) => DERIVATIVE_IN_FLIGHT.has(status)) &&
    !derivativeStatuses(item).every((status) => DERIVATIVE_SUCCESS.has(status))
}

export function knowledgeIsFullyComplete(item: KnowledgePollStatus): boolean {
  return item.parse_status === 'completed' &&
    derivativeStatuses(item).every((status) => DERIVATIVE_SUCCESS.has(status))
}

export function resolveKnowledgeWorkflowStatus(
  item: KnowledgePollStatus,
): KnowledgeWorkflowStatus {
  const parseStatus = normalize(item.parse_status)
  if (parseStatus === 'draft') return 'draft'
  if (parseStatus === 'deleting') return 'deleting'
  if (parseStatus === 'cancelling') return 'cancelling'
  if (parseStatus === 'cancelled') return 'cancelled'
  if (parseStatus === 'failed') return 'failed'
  if (parseStatus === 'pending') return 'pending'
  if (knowledgeNeedsStatusPolling(item)) return 'processing'
  if (knowledgeHasDerivativeFailure(item)) return 'failed'
  if (knowledgeIsFullyComplete(item)) return 'completed'
  return 'unknown'
}

// Keep client-side optimistic updates and status polling consistent with the
// backend workflow_status projection. A row that changes bucket must
// disappear from the currently selected bucket immediately; otherwise a
// "failed" list can briefly contain pending rows after reparse.
export function knowledgeMatchesWorkflowFilter(
  item: KnowledgePollStatus,
  workflowStatus?: string,
): boolean {
  const expected = normalize(workflowStatus)
  return expected === '' || resolveKnowledgeWorkflowStatus(item) === expected
}

export function shouldRefreshWikiStatusAfterKnowledgePoll(
  before: KnowledgePollStatus,
  after: KnowledgePollStatus,
): boolean {
  return knowledgeNeedsStatusPolling(before) && !knowledgeNeedsStatusPolling(after)
}

export type SpanNode = {
  name?: string
  status?: string
  output?: Record<string, unknown>
  started_at?: string | null
  finished_at?: string | null
  children?: SpanNode[]
}

const SPAN_STATUS: Record<string, KnowledgeWorkflowStatus> = {
  pending: 'pending',
  running: 'processing',
  processing: 'processing',
  finalizing: 'processing',
  done: 'completed',
  completed: 'completed',
  skipped: 'completed',
  failed: 'failed',
  degraded: 'failed',
  cancelled: 'cancelled',
}

const SPAN_DETAIL_STATUS: Record<string, KnowledgeDetailStatus> = {
  pending: 'pending',
  running: 'processing',
  processing: 'processing',
  finalizing: 'processing',
  done: 'completed',
  completed: 'completed',
  skipped: 'skipped',
  failed: 'failed',
  degraded: 'failed',
  cancelled: 'cancelled',
}

function nodeTime(node: SpanNode): number {
  const raw = node.finished_at || node.started_at
  const parsed = raw ? Date.parse(raw) : Number.NaN
  return Number.isNaN(parsed) ? 0 : parsed
}

function latestSpanNodes(
  root: SpanNode | undefined,
  matches: (name: string) => boolean,
): SpanNode[] {
  if (!root) return []
  const latest = new Map<string, SpanNode>()
  let sequence = 0
  const order = new Map<SpanNode, number>()
  const walk = (node: SpanNode) => {
    order.set(node, sequence++)
    const name = String(node.name ?? '')
    if (name && matches(name)) {
      const previous = latest.get(name)
      if (
        !previous ||
        nodeTime(node) > nodeTime(previous) ||
        (nodeTime(node) === nodeTime(previous) &&
          (order.get(node) ?? 0) > (order.get(previous) ?? 0))
      ) {
        latest.set(name, node)
      }
    }
    for (const child of node.children || []) walk(child)
  }
  walk(root)
  return [...latest.values()]
}

// Select only the newest retry for each logical span name, then aggregate.
// Old failed/cancelled attempts must not make a later successful retry look
// failed in the compact hover card.
export function latestSpanGroupStatus(
  root: SpanNode | undefined,
  matches: (name: string) => boolean,
): KnowledgeWorkflowStatus | undefined {
  const statuses = latestSpanNodes(root, matches)
    .map((node) => SPAN_STATUS[normalize(node.status)])
    .filter(Boolean)
  if (statuses.length === 0) return undefined
  if (statuses.some((status) => status === 'processing' || status === 'pending')) {
    return 'processing'
  }
  if (statuses.some((status) => status === 'failed')) return 'failed'
  if (statuses.some((status) => status === 'completed')) return 'completed'
  if (statuses.length > 0 && statuses.every((status) => status === 'cancelled')) return 'cancelled'
  return undefined
}

// Detail rendering preserves an intentional skip instead of folding it into
// "completed". A done span whose structured output contains a skip reason is
// also an intentional no-op (for example no text, no extracted image content,
// or a stale generation fenced after reparse/delete).
export function latestSpanGroupDetailStatus(
  root: SpanNode | undefined,
  matches: (name: string) => boolean,
): KnowledgeDetailStatus | undefined {
  const statuses = latestSpanNodes(root, matches).map((node) => {
    if (
      normalize(node.status) === 'skipped' ||
      String(node.output?.skipped ?? '').trim() !== ''
    ) {
      return 'skipped' as const
    }
    return SPAN_DETAIL_STATUS[normalize(node.status)]
  }).filter(Boolean)

  if (statuses.length === 0) return undefined
  if (statuses.some((status) => status === 'processing' || status === 'pending')) {
    return 'processing'
  }
  if (statuses.some((status) => status === 'failed')) return 'failed'
  if (statuses.some((status) => status === 'completed')) return 'completed'
  if (statuses.every((status) => status === 'skipped')) return 'skipped'
  if (statuses.every((status) => status === 'cancelled')) return 'cancelled'
  if (statuses.some((status) => status === 'cancelled')) return 'cancelled'
  return undefined
}
