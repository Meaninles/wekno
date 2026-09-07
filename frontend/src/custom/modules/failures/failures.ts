import catalog from '../../../../../internal/custom/modules/usererrors/catalog.json' with { type: 'json' }

export type PublicFailure = { code: string; message: string }

function detail(value: unknown): string {
  if (typeof value === 'string') return value.trim()
  if (value instanceof Error) return value.message
  if (value && typeof value === 'object') {
    const row = value as Record<string, unknown>
    return detail(row.message || row.error || row.content)
  }
  return ''
}

// This vocabulary is also embedded by Go, so live, persisted and IM messages
// share categories and wording. Never render arbitrary provider diagnostics.
export function publicFailure(value: unknown, code?: unknown): PublicFailure {
  const text = detail(value)
  const known = catalog.find(row => row.code === code)
  if (known) return { code: known.code, message: known.message }
  const lower = text.toLowerCase()
  const item = catalog.find(row => row.message === text || row.patterns.some(pattern => lower.includes(pattern)))
    || catalog[catalog.length - 1]!
  return { code: item.code, message: item.message }
}

export function messageFailure(message: Record<string, any>): PublicFailure | null {
  if (message.role === 'user') return null
  if (message.error_code) return publicFailure(message.content, message.error_code)
  const events: Record<string, any>[] = message.agentEventStream || []
  const failure = events.find(event => event.type === 'error' && !event.tool_name && event.done !== false)
  if (failure) return publicFailure(failure.content || failure.error, failure.error_code)
  if (!message.is_completed) return null
  if (events.some(event => event.type === 'stop') || message.content === '已停止生成。') return null
  // Successful file-only or structured-result messages are not empty replies.
  const visible = String(message.content || '').trim()
    || events.some(event => event.type === 'answer' && !event.superseded && String(event.content || '').trim())
    || message.artifacts?.length
    || message.agent_steps?.some((step: any) => step.tool_calls?.some((call: any) =>
      call.result?.data?.files?.length || call.result?.data?.artifacts?.length))
  if (!visible) return publicFailure('', 'empty_response')
  return null
}
