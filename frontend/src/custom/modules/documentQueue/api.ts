import { post } from '@/utils/request'
import type { DocumentQueueSnapshot, DocumentQueueStatusEnvelope } from './types'
import { normalizeKnowledgeIds } from './ids'

const EMPTY_SNAPSHOT: DocumentQueueSnapshot = { waiting_total: 0, items: {} }

/**
 * Fetch one queue snapshot for the document cards currently rendered by the
 * knowledge-base page. The queue remains system-wide; knowledge_ids only
 * limits the per-document position map returned alongside the global count.
 */
export async function getDocumentQueueStatus(
  knowledgeIds: readonly string[],
): Promise<DocumentQueueSnapshot> {
  const ids = normalizeKnowledgeIds(knowledgeIds)
  if (ids.length === 0) return EMPTY_SNAPSHOT

  // Queue ranks are a point-in-time global snapshot. Sequential GET batches
  // can straddle document claims and assign the same merged rank to two cards.
  // Send all visible IDs in one request body so the backend ranks them from one
  // database statement without hitting proxy request-line limits.
  const response = await post('/api/v1/custom/document-queue/status', {
    knowledge_ids: ids,
  }) as unknown as DocumentQueueStatusEnvelope | DocumentQueueSnapshot
  const envelope = response as DocumentQueueStatusEnvelope
  const payload = (envelope.data ?? response) as Partial<DocumentQueueSnapshot>
  const waitingTotal = Number(payload.waiting_total)
  const snapshot: DocumentQueueSnapshot = {
    waiting_total: Number.isFinite(waitingTotal) ? Math.max(0, Math.trunc(waitingTotal)) : 0,
    items: {},
  }
  if (payload.items && typeof payload.items === 'object') {
    Object.assign(snapshot.items, payload.items)
  }
  return snapshot
}
