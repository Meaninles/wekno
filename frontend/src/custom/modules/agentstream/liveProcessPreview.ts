export const MAX_LIVE_PROCESS_PREVIEWS = 3
export const MAX_LIVE_PROCESS_TEXT_LENGTH = 180

export type LiveProcessState = 'running' | 'success' | 'error'

export interface LiveProcessPreviewItem {
  key: string
  text: string
  state: LiveProcessState
  updatedAt: number
}

export interface LiveAgentProjection {
  previews: LiveProcessPreviewItem[]
  interactiveEvents: Record<string, unknown>[]
  activeAnswer: Record<string, unknown> | null
  answerEverStarted: boolean
  terminalEvent: Record<string, unknown> | null
}

type AgentMessage = Record<string, unknown>

const PROJECTION_KEY = '_agentLiveProjection'

function normalizePreviewText(value: unknown): string {
  const text = String(value || '')
    .replace(/\s+/g, ' ')
    .trim()
  if (!text) return ''
  const chars = Array.from(text)
  if (chars.length <= MAX_LIVE_PROCESS_TEXT_LENGTH) return text
  return `${chars.slice(0, MAX_LIVE_PROCESS_TEXT_LENGTH - 1).join('')}…`
}

export function ensureLiveAgentProjection(message: AgentMessage): LiveAgentProjection {
  const existing = message[PROJECTION_KEY]
  if (existing && typeof existing === 'object') {
    return existing as LiveAgentProjection
  }
  const projection: LiveAgentProjection = {
    previews: [],
    interactiveEvents: [],
    activeAnswer: null,
    answerEverStarted: false,
    terminalEvent: null,
  }
  message[PROJECTION_KEY] = projection
  return projection
}

export function readLiveAgentProjection(message?: AgentMessage | null): LiveAgentProjection | null {
  const value = message?.[PROJECTION_KEY]
  return value && typeof value === 'object' ? (value as LiveAgentProjection) : null
}

export function upsertLiveProcessPreview(
  message: AgentMessage,
  key: unknown,
  text: unknown,
  state: LiveProcessState = 'running',
): void {
  const normalized = normalizePreviewText(text)
  if (!normalized) return

  const projection = ensureLiveAgentProjection(message)
  const stableKey = String(key || 'agent-process')
  const existingIndex = projection.previews.findIndex((item) => item.key === stableKey)
  if (existingIndex >= 0) projection.previews.splice(existingIndex, 1)
  projection.previews.push({
    key: stableKey,
    text: normalized,
    state,
    updatedAt: Date.now(),
  })
  if (projection.previews.length > MAX_LIVE_PROCESS_PREVIEWS) {
    projection.previews.splice(
      0,
      projection.previews.length - MAX_LIVE_PROCESS_PREVIEWS,
    )
  }
}

export function setLiveActiveAnswer(
  message: AgentMessage,
  answerEvent: Record<string, unknown> | null,
): void {
  const projection = ensureLiveAgentProjection(message)
  projection.activeAnswer = answerEvent
  if (answerEvent && String(answerEvent.content || '').trim()) {
    projection.answerEverStarted = true
  }
}

export function clearLiveActiveAnswer(message: AgentMessage): void {
  ensureLiveAgentProjection(message).activeAnswer = null
}

function interactiveIdentity(event: Record<string, unknown>): string {
  return String(
    event.pending_id ||
      event.tool_call_id ||
      `${String(event.type || 'interactive')}:${String(event.requested_at || '')}`,
  )
}

export function addLiveInteractiveEvent(
  message: AgentMessage,
  event: Record<string, unknown>,
): void {
  const projection = ensureLiveAgentProjection(message)
  const identity = interactiveIdentity(event)
  const index = projection.interactiveEvents.findIndex(
    (item) => interactiveIdentity(item) === identity,
  )
  if (index >= 0) projection.interactiveEvents[index] = event
  else projection.interactiveEvents.push(event)
}

export function resolveLiveInteractiveEvent(
  message: AgentMessage,
  pendingID: unknown,
  serviceID?: unknown,
  authorized = false,
): void {
  const projection = ensureLiveAgentProjection(message)
  projection.interactiveEvents = projection.interactiveEvents.filter((event) => {
    if (pendingID && event.pending_id === pendingID) return false
    return !(authorized && serviceID && event.service_id === serviceID)
  })
}

export function setLiveTerminalEvent(
  message: AgentMessage,
  event: Record<string, unknown> | null,
): void {
  ensureLiveAgentProjection(message).terminalEvent = event
}

