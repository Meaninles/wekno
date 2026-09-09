import {
  ref,
  reactive,
  watch,
  nextTick,
  onMounted,
  onUnmounted,
  type Ref,
} from 'vue'
import { useStream } from '@/api/chat/streame'
import {
  getEmbedMessageList,
  postEmbedMessageSent,
  postEmbedMessageReceived,
  relayEmbedWebhookEvent,
  stopEmbedSession,
} from '@/api/embed'
import { embedToast } from '@/utils/embedToast'
import { buildQueryWithHostContext } from '@/utils/embedContext'
import { useChatUploads, chatImagePlaceholder } from '@/custom/modules/chatuploads/uploads'
import { useI18n } from 'vue-i18n'
import { useChatStreamHandler } from '@/composables/useChatStreamHandler'
import { useStickyBottomOnResize } from '@/composables/useStickyBottomOnResize'
import { isAgentStreamAgentId } from '@/utils/agent-mode'
import { synchronizeSessionTitle } from '@/custom/modules/sessiontitle/client'
import { saveSessionDraftState } from '@/custom/modules/sessionState/draftState'
import { embedDraftScope } from '@/custom/modules/sessionState/storage'

type EmbedChatImage = { url?: string; data?: string }
type EmbedChatAttachment = { file_name: string; file_size?: number }
type EmbedChatMessage = Record<string, unknown> & {
  id?: string
  role?: string
  content?: string
  created_at?: string
  mentioned_items?: unknown[]
  images?: EmbedChatImage[]
  attachments?: EmbedChatAttachment[]
  isRagMode?: boolean
  isAgentMode?: boolean
  showThink?: boolean
  hideContent?: boolean
  is_completed?: boolean
  agentEventStream?: Array<Record<string, unknown>>
  knowledge_references?: Array<{ chunk_type?: string; knowledge_id?: string; knowledge_title?: string }>
}

export function useEmbedChatSession(options: {
  sessionId: Ref<string>
  sessionSig: Ref<string>
  visitorId: Ref<string>
  channelId: string
  token: string
  agentId: string
  kbIds: string[]
  allowWebSearch?: boolean
  allowFileUpload?: boolean
  hostContext?: Ref<Record<string, unknown>>
  onMessagesChange?: (has: boolean) => void
  onSessionTitle?: (title: string) => void
}) {
  const { rows: uploadRows, preparing: uploadsPreparing, prepare: prepareUploads, retry: retryUpload, cancel: cancelUploads, detach: detachUploads } = useChatUploads()
  const { t } = useI18n()
  const { onChunk, error, startStream, stopStream } = useStream()

  const isAgentStreamSession = () =>
    isAgentStreamAgentId(options.agentId, true)

  const limit = ref(20)
  const messagesList = reactive<EmbedChatMessage[]>([])
  watch(
    () => messagesList.length,
    (len) => options.onMessagesChange?.(len > 0),
    { immediate: true },
  )

  const isReplying = ref(false)
  const currentAssistantMessageId = ref('')
  const isFirstEnter = ref(true)
  const loading = ref(false)
  const historyLoading = ref(true)
  const historyLoadingMore = ref(false)
  const hasMoreHistory = ref(true)
  const created_at = ref('')
  const fullContent = ref('')
  const scrollContainer = ref<HTMLElement | null>(null)
  const userHasScrolledUp = ref(false)
  const SCROLL_BOTTOM_THRESHOLD = 80

  const isNearBottom = () => {
    if (!scrollContainer.value) return true
    const { scrollTop, scrollHeight, clientHeight } = scrollContainer.value
    return scrollHeight - scrollTop - clientHeight < SCROLL_BOTTOM_THRESHOLD
  }

  const getUserQuery = (index: number) => {
    if (index <= 0) return ''
    const previous = messagesList[index - 1]
    if (previous && previous.role === 'user') {
      return String(previous.content || '')
    }
    return ''
  }

  const scrollToBottom = (force = false) => {
    if (!force && userHasScrolledUp.value) return
    nextTick(() => {
      if (scrollContainer.value) {
        scrollContainer.value.scrollTop = scrollContainer.value.scrollHeight
      }
    })
  }

  const onClickScrollToBottom = () => {
    userHasScrolledUp.value = false
    scrollToBottom(true)
  }

  useStickyBottomOnResize(scrollContainer, userHasScrolledUp, scrollToBottom)

  const debounce = <T extends (...args: never[]) => void>(fn: T, delay: number) => {
    let timer: ReturnType<typeof setTimeout>
    return (...args: Parameters<T>) => {
      clearTimeout(timer)
      timer = setTimeout(() => fn(...args), delay)
    }
  }

  const notifyEmbedReceived = (content: string) => {
    if (!content?.trim()) return
    postEmbedMessageReceived(options.channelId, options.sessionId.value, content)
    relayEmbedWebhookEvent(
      options.channelId,
      options.token,
      options.sessionId.value,
      options.sessionSig.value,
      { type: 'message_received', content },
    )
  }

  const {
    shouldRenderAssistantMessage,
    shouldShowGlobalTypingIndicator,
    handleMsgList,
    processStreamChunk,
    prepareForNewOutgoingMessage,
    markInFlightAssistantStopped,
  } = useChatStreamHandler({
    messagesList,
    loading,
    isReplying,
    currentAssistantMessageId,
    fullContent,
    isAgentStreamSession,
    scrollToBottom,
    onReplyComplete: notifyEmbedReceived,
    onError: embedToast,
    isFirstEnter,
    scrollContainer,
  })

  const onChatScrollTop = () => {
    if (historyLoadingMore.value || !hasMoreHistory.value) return
    if (!scrollContainer.value) return
    const { scrollTop, scrollHeight } = scrollContainer.value
    isFirstEnter.value = false
    if (scrollTop <= 0) {
      getmsgList(
        {
          session_id: options.sessionId.value,
          created_at: created_at.value,
          limit: limit.value,
        },
        true,
        scrollHeight,
      )
    }
  }

  const debouncedScrollTop = debounce(onChatScrollTop, 500)

  let lastScrollTop = 0
  const handleScroll = () => {
    const el = scrollContainer.value
    if (el) {
      const currentTop = el.scrollTop
      // Only an actual upward scroll detaches from the live edge. Content that
      // grows after a chunk (images, diagrams) keeps scrollTop fixed and would
      // otherwise fire a stale scroll event that falsely marks the user as
      // scrolled up, killing the auto-follow during streaming.
      if (currentTop < lastScrollTop - 1) {
        userHasScrolledUp.value = !isNearBottom()
      } else if (isNearBottom()) {
        userHasScrolledUp.value = false
      }
      lastScrollTop = currentTop
    }
    debouncedScrollTop()
  }

  const getmsgList = (
    data: { session_id: string; created_at?: string; limit: number },
    isScrollType = false,
    scrollHeight?: number,
  ) => {
    if (isScrollType) {
      if (historyLoadingMore.value || !hasMoreHistory.value) return
      historyLoadingMore.value = true
    }

    getEmbedMessageList(
      options.channelId,
      options.token,
      data.session_id,
      data.limit,
      data.created_at || undefined,
      options.sessionSig.value,
    )
      .then(async (res) => {
        const batch = res?.data as Record<string, unknown>[] | undefined
        if (!batch?.length) {
          // No (more) server history. Crucially this also covers the initial
          // load of a brand-new session: leaving hasMoreHistory true here would
          // let a later scroll-to-top re-fetch with an empty cursor (= "latest"),
          // pulling back the just-sent messages and duplicating them.
          hasMoreHistory.value = false
          return
        }
        const nextCursor = String(batch[0].created_at)
        if (isScrollType && created_at.value && nextCursor === created_at.value) {
          hasMoreHistory.value = false
          return
        }
        if (batch.length < limit.value) hasMoreHistory.value = false
        created_at.value = nextCursor
        await handleMsgList(batch, isScrollType, scrollHeight)
        if (!isScrollType && batch.at(-1)?.is_completed) void synchronizeSessionTitle(data.session_id, {
          prefix: `/api/v1/embed/${options.channelId}`, embedToken: options.token,
          sessionSig: options.sessionSig.value, visitorId: options.visitorId.value,
          active: () => options.sessionId.value === data.session_id,
          onTitle: value => options.onSessionTitle?.(value.title),
        })
      })
      .catch((err) => {
        console.error('Failed to load messages:', err)
        if (isScrollType) hasMoreHistory.value = false
      })
      .finally(() => {
        historyLoading.value = false
        historyLoadingMore.value = false
      })
  }

  const handleStopGeneration = () => {
    void cancelUploads()
    stopStream()
    markInFlightAssistantStopped(currentAssistantMessageId.value)
    const messageId = currentAssistantMessageId.value
    if (messageId) {
      stopEmbedSession(
        options.channelId,
        options.token,
        options.sessionId.value,
        messageId,
        options.sessionSig.value,
      ).catch((err) => console.error('Failed to stop embed generation:', err))
    }
    loading.value = false
    isReplying.value = false
  }

  const sendMsg = async (
    value: string,
    opts: { webSearchEnabled?: boolean; imageFiles?: File[]; attachmentFiles?: File[] } = {},
  ) => {
    stopStream()
    prepareForNewOutgoingMessage()
    const outboundQuery = buildQueryWithHostContext(value, options.hostContext?.value)
    const visitorWebSearchEnabled = opts.webSearchEnabled ?? false
    const imageFiles = (options.allowFileUpload ? opts.imageFiles : undefined) || []
    const attachmentFiles = (options.allowFileUpload ? opts.attachmentFiles : undefined) || []
    isReplying.value = true
    loading.value = true

    const requestSessionId = options.sessionId.value
    const requestSessionSig = options.sessionSig.value
    const requestVisitorId = options.visitorId.value
    const draftScope = embedDraftScope(options.channelId, requestVisitorId)
    const draftAttachments = attachmentFiles.map(file => ({ file, id: crypto.randomUUID(), name: file.name, size: file.size, type: file.type }))
    saveSessionDraftState(requestSessionId, {}, draftAttachments, imageFiles, value, draftScope)
    let uploadIds: string[]
    let inputFileIds: string[]
    try {
      const prepared = await prepareUploads([...imageFiles, ...attachmentFiles], {
        sessionId: requestSessionId, agentId: options.agentId, channelId: options.channelId,
        token: options.token, sessionSig: requestSessionSig, visitorId: requestVisitorId,
        directInput: isAgentStreamSession(),
      })
      uploadIds = prepared.uploadIds
      inputFileIds = prepared.inputFileIds
    } catch (err: any) {
      if (requestSessionId !== options.sessionId.value) return
      isReplying.value = false
      loading.value = false
      if (err?.name !== 'AbortError') embedToast(err?.message || '文件处理失败')
      throw err
    }
    if (requestSessionId !== options.sessionId.value) return
    const displayImages = imageFiles.map(file => ({ url: chatImagePlaceholder(), name: file.name }))
    const displayAttachments = attachmentFiles.map(file => ({ file_name: file.name, file_size: file.size }))

    messagesList.push({
      content: value,
      role: 'user',
      mentioned_items: [],
      images: displayImages,
      attachments: displayAttachments,
      channel: 'embed',
    })
    postEmbedMessageSent(options.channelId, options.sessionId.value, value)
    relayEmbedWebhookEvent(
      options.channelId,
      options.token,
      options.sessionId.value,
      options.sessionSig.value,
      { type: 'message_sent', query: value },
    )
    userHasScrolledUp.value = false
    scrollToBottom(true)

    const agentEnabled = isAgentStreamSession()
    const endpoint = agentEnabled
      ? `/api/v1/embed/${options.channelId}/agent-chat`
      : `/api/v1/embed/${options.channelId}/knowledge-chat`

    await startStream({
      session_id: options.sessionId.value,
      knowledge_base_ids: options.kbIds,
      knowledge_ids: [],
      agent_enabled: agentEnabled,
      agent_id: options.agentId,
      web_search_enabled: (options.allowWebSearch ?? false) && visitorWebSearchEnabled,
      enable_memory: false,
      summary_model_id: '',
      mcp_service_ids: [],
      mentioned_items: [],
      upload_ids: uploadIds,
      input_file_ids: inputFileIds,
      query: outboundQuery,
      method: 'POST',
      url: endpoint,
      embed_token: options.token,
      embed_session_sig: options.sessionSig.value,
      embed_visitor_id: options.visitorId.value,
    })
    saveSessionDraftState(requestSessionId, {}, draftAttachments, imageFiles, '', draftScope)
  }

  watch(error, (newError) => {
    if (newError) {
      embedToast(newError)
      isReplying.value = false
      loading.value = false
      currentAssistantMessageId.value = ''
    }
  })

  onChunk((data) => {
    if (data.response_type === 'session_title') {
	  if (data.data?.session_id && data.data.session_id !== options.sessionId.value) return
      const title = String(data.content || (data.data as { title?: string })?.title || '').trim()
      if (title) {
        options.onSessionTitle?.(title)
      }
      return
    }
    processStreamChunk(data)
  })

  const resetAndLoad = (sid: string) => {
    detachUploads()
    messagesList.splice(0)
    historyLoading.value = true
    historyLoadingMore.value = false
    hasMoreHistory.value = true
    created_at.value = ''
    loading.value = false
    isReplying.value = false
    currentAssistantMessageId.value = ''
    userHasScrolledUp.value = false
    isFirstEnter.value = true
    fullContent.value = ''
    if (!sid) {
      historyLoading.value = false
      return
    }
    getmsgList({ session_id: sid, created_at: '', limit: limit.value })
  }

  watch(
    () => options.sessionId.value,
    (sid) => resetAndLoad(sid),
    { immediate: true },
  )

  onMounted(() => {
    loading.value = false
    isReplying.value = false
  })

  onUnmounted(() => {
    stopStream()
    fullContent.value = ''
  })

  return {
    uploadRows, uploadsPreparing, prepareUploads, retryUpload, cancelUploads,
    messagesList,
    loading,
    isReplying,
    historyLoading,
    scrollContainer,
    userHasScrolledUp,
    isFirstEnter,
    shouldRenderAssistantMessage,
    shouldShowGlobalTypingIndicator,
    getUserQuery,
    handleScroll,
    scrollToBottom,
    onClickScrollToBottom,
    sendMsg,
    handleStopGeneration,
  }
}
