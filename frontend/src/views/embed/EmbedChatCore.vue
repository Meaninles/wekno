<template>
  <div class="embed-chat">
    <div ref="scrollContainer" class="embed-chat__scroll" @scroll="handleScroll">
      <div class="embed-chat__messages">
        <div v-if="historyLoading && messagesList.length === 0 && !hasWelcomeText" class="msg-skeleton-list">
          <div class="msg-skeleton msg-skeleton-user"><div class="sk-line sk-line--short" /></div>
          <div class="msg-skeleton msg-skeleton-bot">
            <div class="sk-line" />
            <div class="sk-line" />
            <div class="sk-line sk-line--medium" />
          </div>
        </div>

        <div
          v-if="showWelcome"
          class="embed-welcome-bubble"
        >
          <p class="embed-welcome-bubble__text">{{ welcomeText }}</p>
        </div>

        <div
          v-for="(session, index) in messagesList"
          :key="(session.id as string) || `${session.role}-${session.created_at}-${index}`"
          class="msg-item-wrapper"
        >
          <div v-if="session.role === 'user'">
            <EmbedUserMessage
              :content="String(session.content || '')"
              :mentioned_items="asUnknownArray(session.mentioned_items)"
              :images="asEmbedImages(session.images)"
              :attachments="asEmbedAttachments(session.attachments)"
              :embeddedMode="true"
              :embed-channel-id="channelId"
              :embed-token="token"
            />
          </div>
          <div v-if="session.role === 'assistant' && shouldRenderAssistantMessage(session)">
            <EmbedBotMessage
              :content="String(session.content || '')"
              :session="session"
              :session-id="sessionId"
              :user-query="getUserQuery(index)"
              :embedded-mode="true"
              :embed-channel-id="channelId"
              :embed-token="token"
              :embed-session-sig="sessionSig"
            />
          </div>
        </div>

        <div v-if="showGlobalTypingIndicator" class="embed-chat__typing">
          <div class="loading-typing">
            <span></span>
            <span></span>
            <span></span>
          </div>
        </div>
      </div>
    </div>

    <transition name="scroll-btn-fade">
      <div v-show="userHasScrolledUp" class="scroll-to-bottom-btn" @click="onClickScrollToBottom" aria-label="scroll to bottom">
        <svg width="20" height="20" viewBox="0 0 24 24" fill="none" aria-hidden="true">
          <path d="M6 9l6 6 6-6" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" />
        </svg>
      </div>
    </transition>

    <div class="embed-chat__input">
      <EmbedInputField
        :isReplying="isReplying"
        :show-web-search-toggle="showWebSearchToggle"
        v-model:web-search-enabled="webSearchEnabled"
        :show-file-upload-toggle="showFileUploadToggle"
        @send-msg="onSendMsg"
        @stop-generation="handleStopGeneration"
      />
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, toRef, watch } from 'vue'
import { onEmbedHostOpenWithQuery } from '@/api/embed'
import EmbedInputField from '@/components/EmbedInputField.vue'
import EmbedBotMessage from '@/views/embed/EmbedBotMessage.vue'
import EmbedUserMessage from '@/views/embed/EmbedUserMessage.vue'
import { useEmbedChatSession } from '@/composables/useEmbedChatSession'

type EmbedImage = { url?: string; data?: string }
type EmbedAttachment = { file_name: string; file_size?: number }

const props = defineProps<{
  sessionId: string
  sessionSig: string
  visitorId: string
  channelId: string
  token: string
  agentId: string
  kbIds: string[]
  welcomeMessage?: string
  showSuggestedQuestions?: boolean
  allowWebSearch?: boolean
  agentWebSearchEnabled?: boolean
  allowFileUpload?: boolean
  agentImageUploadEnabled?: boolean
  useSessionHeaderTitle?: boolean
  hostContext?: Record<string, unknown>
}>()

const emit = defineEmits<{
  (e: 'session-title', title: string): void
  (e: 'messages-state', hasMessages: boolean): void
}>()

const sessionIdRef = toRef(props, 'sessionId')
const sessionSigRef = toRef(props, 'sessionSig')
const visitorIdRef = toRef(props, 'visitorId')
const hostContextRef = ref<Record<string, unknown>>(props.hostContext || {})

function asUnknownArray(value: unknown): unknown[] | undefined {
  return Array.isArray(value) ? value : undefined
}

function asEmbedImages(value: unknown): EmbedImage[] | undefined {
  return Array.isArray(value) ? value as EmbedImage[] : undefined
}

function asEmbedAttachments(value: unknown): EmbedAttachment[] | undefined {
  return Array.isArray(value) ? value as EmbedAttachment[] : undefined
}

const embedWebSearchStorageKey = () => `weknora-embed-web-search:${props.channelId}`

const readStoredWebSearchEnabled = () => {
  if (typeof localStorage === 'undefined') return false
  return localStorage.getItem(embedWebSearchStorageKey()) === '1'
}

const webSearchEnabled = ref(readStoredWebSearchEnabled())

const showWebSearchToggle = computed(
  () => props.allowWebSearch === true && props.agentWebSearchEnabled === true,
)
const showFileUploadToggle = computed(
  () => props.allowFileUpload === true && props.agentImageUploadEnabled === true,
)

watch(webSearchEnabled, (enabled) => {
  if (!showWebSearchToggle.value) return
  if (typeof localStorage === 'undefined') return
  localStorage.setItem(embedWebSearchStorageKey(), enabled ? '1' : '0')
})

watch(showWebSearchToggle, (visible) => {
  if (!visible) {
    webSearchEnabled.value = false
  }
})

watch(() => props.hostContext, (ctx) => {
  hostContextRef.value = ctx || {}
}, { deep: true })

const {
  messagesList,
  loading,
  isReplying,
  historyLoading,
  scrollContainer,
  userHasScrolledUp,
  shouldRenderAssistantMessage,
  shouldShowGlobalTypingIndicator,
  getUserQuery,
  handleScroll,
  scrollToBottom,
  onClickScrollToBottom,
  sendMsg,
  handleStopGeneration,
} = useEmbedChatSession({
  sessionId: sessionIdRef,
  sessionSig: sessionSigRef,
  visitorId: visitorIdRef,
  channelId: props.channelId,
  token: props.token,
  agentId: props.agentId,
  kbIds: props.kbIds,
  allowWebSearch: props.allowWebSearch,
  allowFileUpload: props.allowFileUpload,
  hostContext: hostContextRef,
  onMessagesChange: (has) => emit('messages-state', has),
  onSessionTitle: (title) => {
    if (props.useSessionHeaderTitle) {
      emit('session-title', title)
    }
  },
})

const welcomeText = computed(() => props.welcomeMessage?.trim() || '')
const hasWelcomeText = computed(() => welcomeText.value.length > 0)

const hasUserMessage = computed(() =>
  messagesList.some((m) => m.role === 'user'))

const showGlobalTypingIndicator = computed(() =>
  shouldShowGlobalTypingIndicator(messagesList, loading.value),
)

/** 访客未发言前始终展示欢迎语（含历史加载中），发送后隐藏 */
const showWelcome = computed(() => hasWelcomeText.value && !hasUserMessage.value)

const onSendMsg = (query: string, imageFiles: File[] = [], attachmentFiles: File[] = []) => {
  void sendMsg(query, {
    webSearchEnabled: webSearchEnabled.value,
    imageFiles,
    attachmentFiles,
  })
}

let removeOpenQueryListener: (() => void) | null = null

onMounted(() => {
  removeOpenQueryListener = onEmbedHostOpenWithQuery((query) => {
    if (isReplying.value) return
    void sendMsg(query, { webSearchEnabled: webSearchEnabled.value })
  })
})

onUnmounted(() => {
  removeOpenQueryListener?.()
})
</script>

<style scoped lang="less">
.embed-chat {
  display: flex;
  flex-direction: column;
  flex: 1;
  min-height: 0;
  width: 100%;
  position: relative;
}

.embed-chat__scroll {
  flex: 1;
  min-height: 0;
  width: 100%;
  overflow-y: auto;
}

.embed-chat__messages {
  display: flex;
  flex-direction: column;
  gap: 16px;
  max-width: 800px;
  margin: 0 auto;
  width: 100%;
  padding: 12px 16px 0;
  box-sizing: border-box;
}

.embed-welcome-bubble {
  display: flex;
  justify-content: flex-start;
  width: 100%;
  animation: welcome-in 0.28s ease both;

  &__text {
    margin: 0;
    max-width: min(88%, 520px);
    padding: 10px 14px;
    font-size: 14px;
    line-height: 1.55;
    color: var(--td-text-color-primary);
    white-space: pre-wrap;
    word-break: break-word;
    background: color-mix(
      in srgb,
      var(--embed-primary, var(--td-brand-color)) 7%,
      var(--td-bg-color-container, #fff)
    );
    border: 1px solid color-mix(
      in srgb,
      var(--embed-primary, var(--td-brand-color)) 14%,
      var(--td-component-stroke, #e7e7e7)
    );
    border-radius: 4px 14px 14px 14px;
    box-shadow: 0 1px 2px rgba(15, 23, 42, 0.04);
  }
}

@keyframes welcome-in {
  from {
    opacity: 0;
    transform: translateY(6px);
  }
  to {
    opacity: 1;
    transform: translateY(0);
  }
}

.embed-chat__typing {
  height: 41px;
  display: flex;
  align-items: center;
  padding-left: 4px;
}

.embed-chat__input {
  flex-shrink: 0;
  padding: 8px 16px 16px;
  box-sizing: border-box;
}

.msg-skeleton-list {
  display: flex;
  flex-direction: column;
  gap: 20px;
  padding: 8px 0;
}

.msg-skeleton-user {
  display: flex;
  justify-content: flex-end;
}

.msg-skeleton-bot {
  display: flex;
  flex-direction: column;
  gap: 8px;
  padding-left: 4px;
}

.sk-line {
  height: 14px;
  border-radius: 6px;
  background: linear-gradient(90deg, #f0f0f0 25%, #e6e6e6 50%, #f0f0f0 75%);
  background-size: 200% 100%;
  animation: sk-shimmer 1.2s ease-in-out infinite;
}

.sk-line--short { width: 45%; height: 36px; }
.sk-line--medium { width: 60%; }

@keyframes sk-shimmer {
  0% { background-position: 200% 0; }
  100% { background-position: -200% 0; }
}

.loading-typing {
  display: flex;
  align-items: center;
  gap: 4px;

  span {
    width: 6px;
    height: 6px;
    border-radius: 50%;
    background: var(--td-text-color-placeholder);
    animation: typingBounce 1.4s ease-in-out infinite;

    &:nth-child(1) { animation-delay: 0s; }
    &:nth-child(2) { animation-delay: 0.2s; }
    &:nth-child(3) { animation-delay: 0.4s; }
  }
}

@keyframes typingBounce {
  0%, 60%, 100% { transform: translateY(0); }
  30% { transform: translateY(-8px); }
}

.scroll-to-bottom-btn {
  position: absolute;
  left: 50%;
  transform: translateX(-50%);
  bottom: 100px;
  z-index: 10;
  width: 36px;
  height: 36px;
  border-radius: 50%;
  background: var(--td-bg-color-container);
  border: 1px solid var(--td-component-stroke);
  box-shadow: 0 2px 8px rgba(0, 0, 0, 0.1);
  display: flex;
  align-items: center;
  justify-content: center;
  cursor: pointer;
  color: var(--td-text-color-secondary);
}

.scroll-btn-fade-enter-active,
.scroll-btn-fade-leave-active {
  transition: opacity 0.2s ease, transform 0.2s ease;
}

.scroll-btn-fade-enter-from,
.scroll-btn-fade-leave-to {
  opacity: 0;
  transform: translateX(-50%) translateY(8px);
}
</style>
