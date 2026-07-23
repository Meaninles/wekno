export type DocumentQueueItemState = 'waiting' | 'active' | 'none'

export interface DocumentQueueItemStatus {
  position: number
  state: DocumentQueueItemState
}

export interface DocumentQueueSnapshot {
  waiting_total: number
  items: Record<string, DocumentQueueItemStatus>
}

export interface DocumentQueueStatusEnvelope {
  success?: boolean
  data?: DocumentQueueSnapshot
}
