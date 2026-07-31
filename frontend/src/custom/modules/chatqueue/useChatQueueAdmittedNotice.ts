import { computed, onBeforeUnmount, ref, type Ref } from 'vue'
import type { ChatQueueStatus } from './types'

const ADMITTED_NOTICE_MS = 3000

type AdmittedNotice = {
  sessionId: string
  status: ChatQueueStatus
}

export function useChatQueueAdmittedNotice(sessionId: Ref<string>) {
  const notice = ref<AdmittedNotice | null>(null)
  let hideTimer: ReturnType<typeof setTimeout> | null = null

  const clear = () => {
    if (hideTimer) {
      clearTimeout(hideTimer)
      hideTimer = null
    }
    notice.value = null
  }

  const capture = (payload?: {
    response_type?: string
    data?: ChatQueueStatus
  } | null) => {
    if (
      payload?.response_type !== 'queue_status' ||
      payload.data?.state !== 'admitted'
    ) {
      return
    }

    clear()
    notice.value = {
      sessionId: String(sessionId.value || ''),
      status: { ...payload.data },
    }
    hideTimer = setTimeout(clear, ADMITTED_NOTICE_MS)
  }

  const admittedStatus = computed(() => {
    if (notice.value?.sessionId !== String(sessionId.value || '')) return null
    return notice.value.status
  })

  onBeforeUnmount(clear)

  return {
    admittedStatus,
    capture,
    clear,
  }
}
