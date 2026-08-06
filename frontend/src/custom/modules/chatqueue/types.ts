export const CHAT_QUEUE_FULL = 'CHAT_QUEUE_FULL'
export const CHAT_QUEUE_USER_LIMIT = 'CHAT_QUEUE_USER_LIMIT'
export const CHAT_QUEUE_UNAVAILABLE = 'CHAT_QUEUE_UNAVAILABLE'
export const CHAT_QUEUE_MODEL_UNAVAILABLE = 'CHAT_QUEUE_MODEL_UNAVAILABLE'

export interface ChatQueueRejection {
  code: string
  message: string
  model_id?: string
  resource_pool_id?: string
  waiting?: number
  active?: number
  max_concurrent?: number
  max_waiting?: number
  user_waiting?: number
  user_max_waiting?: number
}

export interface ChatQueueStatus {
  state: 'waiting' | 'admitted'
  model_id?: string
  resource_pool_id?: string
  position?: number
  waiting?: number
  active?: number
  max_concurrent?: number
  max_waiting?: number
  queued_at_unix?: number
}

export class ChatQueueHTTPError extends Error {
  rejection: ChatQueueRejection

  constructor(rejection: ChatQueueRejection) {
    super(rejection.message)
    this.name = 'ChatQueueHTTPError'
    this.rejection = rejection
  }
}

export async function readChatQueueRejection(
  response: Response,
): Promise<ChatQueueRejection | null> {
  if (response.status !== 429 && response.status !== 503) return null
  try {
    const payload = await response.clone().json()
    const code = String(payload?.code || payload?.data?.code || '')
    if (!code.startsWith('CHAT_QUEUE_')) return null
    return {
      ...(payload?.data || {}),
      code,
      message: String(
        payload?.message ||
        payload?.error ||
        payload?.data?.message ||
        '当前请求暂时无法进入聊天队列',
      ),
    }
  } catch {
    return null
  }
}

export function isPersonalQueueLimit(rejection?: ChatQueueRejection | null) {
  return rejection?.code === CHAT_QUEUE_USER_LIMIT
}
