<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from "vue";
import { useRoute } from "vue-router";
import { MessagePlugin } from "tdesign-vue-next";
import botmsg from "@/views/chat/components/botmsg.vue";
import usermsg from "@/views/chat/components/usermsg.vue";
import GeneralAgentArtifactsResult from "@/views/chat/components/tool-results/GeneralAgentArtifactsResult.vue";
import ChatQuestionLocator from "@/custom/modules/chatQuestionLocator/ChatQuestionLocator.vue";
import MobileChatMessage from "@/custom/modules/mobile/components/MobileChatMessage.vue";
import { getChatShare, type ChatShareView } from "../api";
import { normalizeMessageArtifacts } from "../media";
import { artifactDataFor, normalizeChatShareMessage, userQueryFor } from "../message";

const route = useRoute();
const share = ref<ChatShareView | null>(null);
const loading = ref(true);
const errorMessage = ref("");
const isMobileLayout = ref(false);
const scrollContainer = ref<HTMLElement | null>(null);
let mediaQuery: MediaQueryList | null = null;

const token = computed(() => String(route.params.token || "").trim());
const title = computed(() => share.value?.title || "分享对话");
const messages = computed(() => (share.value?.messages || []).map(normalizeChatShareMessage));

async function loadShare() {
  if (!token.value) {
    errorMessage.value = "分享链接无效";
    loading.value = false;
    return;
  }
  loading.value = true;
  errorMessage.value = "";
  try {
    const resp: any = await getChatShare(token.value);
    if (!resp?.success || !resp?.data) {
      throw new Error(resp?.message || "分享内容不存在");
    }
    share.value = resp.data;
    document.title = `${title.value} - WeKnora`;
  } catch (error: any) {
    errorMessage.value = error?.message || "分享内容加载失败";
    MessagePlugin.error(errorMessage.value);
  } finally {
    loading.value = false;
  }
}

function syncLayout(query: MediaQueryList | MediaQueryListEvent) {
  isMobileLayout.value = query.matches;
}

onMounted(() => {
  if (typeof window !== "undefined") {
    mediaQuery = window.matchMedia("(max-width: 768px)");
    syncLayout(mediaQuery);
    mediaQuery.addEventListener?.("change", syncLayout);
  }
  void loadShare();
});

onBeforeUnmount(() => {
  mediaQuery?.removeEventListener?.("change", syncLayout);
});
</script>

<template>
  <main class="chat-share" :class="{ 'is-mobile': isMobileLayout }">
    <div v-if="loading" class="share-state">
      <t-loading size="small" />
      <span>正在加载</span>
    </div>

    <div v-else-if="errorMessage" class="share-state share-state--error">
      <strong>无法打开分享</strong>
      <span>{{ errorMessage }}</span>
    </div>

    <template v-else>
      <section v-if="!isMobileLayout" class="share-desktop">
        <header class="share-desktop__header">
          <h1>{{ title }}</h1>
        </header>
        <div ref="scrollContainer" class="share-desktop__scroll">
          <div class="share-desktop__list msg_list">
            <div
              v-for="(message, index) in messages"
              :key="message.id || `${message.role}-${index}`"
              class="share-message"
              :class="`share-message--${message.role}`"
              :data-chat-message-index="index"
              :data-chat-message-role="message.role"
              :data-chat-question-index="message.role === 'user' ? index : null"
            >
              <usermsg
                v-if="message.role === 'user'"
                :content="message.content"
                :mentioned_items="message.mentioned_items"
                :images="message.images"
                :attachments="message.attachments"
                :share-mode="true"
                :share-token="token"
              />
              <botmsg
                v-else-if="message.role === 'assistant'"
                :content="message.content"
                :session="message"
                :session-id="share?.session_id || ''"
                :user-query="userQueryFor(messages, index)"
                :share-mode="true"
                :share-token="token"
              />
              <GeneralAgentArtifactsResult
                v-if="message.role === 'assistant' && normalizeMessageArtifacts(message.artifacts).length"
                class="share-artifacts"
                :data="artifactDataFor(message)"
                :share-mode="true"
              />
            </div>
            <div v-if="messages.length === 0" class="share-empty">暂无对话内容</div>
          </div>
        </div>
        <ChatQuestionLocator :messages="messages" :scroll-container="scrollContainer" />
      </section>

      <section v-else class="share-mobile">
        <header class="share-mobile__header">
          <span aria-hidden="true"></span>
          <h1>{{ title }}</h1>
          <span aria-hidden="true"></span>
        </header>
        <div class="share-mobile__scroll">
          <MobileChatMessage
            v-for="(message, index) in messages"
            :key="message.id || `${message.role}-${index}`"
            class="share-mobile__message"
            :message="message"
            :share-mode="true"
            :share-token="token"
          />
          <div v-if="messages.length === 0" class="share-empty">暂无对话内容</div>
        </div>
      </section>
    </template>
  </main>
</template>

<style scoped>
.chat-share {
  min-height: 100vh;
  background: #f6f8f7;
  color: #17211c;
}

.chat-share.is-mobile {
  --mobile-font-family: -apple-system, BlinkMacSystemFont, "SF Pro Text", "PingFang SC", "HarmonyOS Sans SC", "MiSans", "Noto Sans SC", "Source Han Sans SC", "Microsoft YaHei UI", "Microsoft YaHei", "Segoe UI", Roboto, "Helvetica Neue", Arial, sans-serif;
  --mobile-font-family-mono: ui-monospace, SFMono-Regular, "SF Mono", Menlo, Monaco, Consolas, "Liberation Mono", "Courier New", monospace;
  --mobile-base-font-size: 17px;
  --mobile-reading-font-size: 18.5px;
  --mobile-reading-line-height: 2.22;
  min-height: 100dvh;
  overflow: hidden;
  background: #f5f7f8;
  color: #15211d;
  font-family: var(--mobile-font-family);
  font-size: var(--mobile-base-font-size);
  font-weight: 400;
  font-feature-settings: "kern" 1;
  font-kerning: normal;
  text-rendering: optimizeLegibility;
  -webkit-text-size-adjust: 100%;
  -webkit-font-smoothing: antialiased;
  -moz-osx-font-smoothing: grayscale;
  overscroll-behavior: none;
}

.chat-share.is-mobile :deep(*) {
  box-sizing: border-box;
}

.chat-share.is-mobile :deep(button),
.chat-share.is-mobile :deep(input),
.chat-share.is-mobile :deep(textarea) {
  font-family: var(--mobile-font-family);
  font-size: inherit;
}

.chat-share.is-mobile :deep(pre),
.chat-share.is-mobile :deep(code),
.chat-share.is-mobile :deep(kbd),
.chat-share.is-mobile :deep(samp) {
  font-family: var(--mobile-font-family-mono);
}

.share-state {
  min-height: 100vh;
  display: flex;
  align-items: center;
  justify-content: center;
  gap: 10px;
  color: #53615a;
  font-size: 14px;
}

.share-state--error {
  flex-direction: column;
}

.share-state--error strong {
  color: #17211c;
  font-size: 18px;
}

.share-desktop {
  position: relative;
  height: 100vh;
  max-height: 100vh;
  display: grid;
  grid-template-rows: auto minmax(0, 1fr);
  overflow: hidden;
}

.share-desktop__header {
  display: flex;
  align-items: center;
  min-height: 64px;
  padding: 0 40px;
  border-bottom: 1px solid #dfe6e2;
  background: rgba(255, 255, 255, 0.92);
}

.share-desktop__header h1,
.share-mobile__header h1 {
  min-width: 0;
  margin: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.share-desktop__header h1 {
  font-size: 18px;
  font-weight: 650;
}

.share-desktop__scroll {
  min-height: 0;
  overflow-y: auto;
}

.share-desktop__list {
  width: min(920px, calc(100% - 112px));
  margin: 0 auto;
  padding: 28px 0 52px;
}

.share-message {
  width: 100%;
  margin: 0 0 18px;
}

.share-message--assistant {
  padding-right: 12%;
}

.share-message--user {
  padding-left: 12%;
}

.share-mobile {
  display: grid;
  width: 100%;
  min-width: 0;
  max-width: 100vw;
  height: 100dvh;
  grid-template-rows: auto minmax(0, 1fr);
  overflow: hidden;
  background: #f5f7f8;
}

.share-mobile__header {
  display: grid;
  width: 100%;
  min-width: 0;
  grid-template-columns: 42px minmax(0, 1fr) 42px;
  align-items: center;
  gap: 4px;
  padding: calc(env(safe-area-inset-top) + 8px) 12px 8px;
  border-bottom: 1px solid #e3ebe6;
  background: rgba(245, 247, 248, 0.96);
}

.share-mobile__header h1 {
  justify-self: center;
  max-width: 100%;
  font-size: 16px;
  font-weight: 650;
  line-height: 1.25;
}

.share-mobile__scroll {
  display: flex;
  width: 100%;
  min-height: 0;
  min-width: 0;
  flex-direction: column;
  align-items: stretch;
  gap: 18px;
  overflow-y: auto;
  overflow-x: hidden;
  padding: 16px 16px 12px;
  scrollbar-width: none;
  -webkit-overflow-scrolling: touch;
}

.share-mobile__scroll::-webkit-scrollbar {
  display: none;
}

.share-mobile__message {
  width: 100%;
  min-width: 0;
  align-self: stretch;
}

.share-empty {
  padding: 32px 0;
  color: #718078;
  text-align: center;
  font-size: 14px;
}

@media (max-width: 768px) {
  .chat-share {
    min-height: 100dvh;
  }
}
</style>
