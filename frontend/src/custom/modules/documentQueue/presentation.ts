import type { DocumentQueueItemStatus } from './types'

export interface DocumentQueueBadgeView {
  visible: boolean
  position: number
  total: number | null
}

const positiveInteger = (value: unknown): number | null => {
  const parsed = Number(value)
  if (!Number.isFinite(parsed) || parsed <= 0) return null
  return Math.trunc(parsed)
}

export function documentQueueBadgeView(
  status: DocumentQueueItemStatus | null | undefined,
  waitingTotal: unknown,
): DocumentQueueBadgeView {
  if (status?.state !== 'waiting') {
    return { visible: false, position: 0, total: null }
  }

  const position = positiveInteger(status.position)
  if (!position) return { visible: false, position: 0, total: null }

  const total = positiveInteger(waitingTotal)
  return {
    visible: true,
    position,
    // Never render an internally contradictory "position 8 / total 3"
    // while independently refreshed queue counters briefly cross in flight.
    total: total === null ? null : Math.max(total, position),
  }
}
