<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from "vue";
import { useRoute, useRouter } from "vue-router";
import { MessagePlugin } from "tdesign-vue-next";
import botmsg from "@/views/chat/components/botmsg.vue";
import usermsg from "@/views/chat/components/usermsg.vue";
import GeneralAgentArtifactsResult from "@/views/chat/components/tool-results/GeneralAgentArtifactsResult.vue";
import MobileChatMessage from "@/custom/modules/mobile/components/MobileChatMessage.vue";
import { copyTextToClipboard } from "@/utils/chatMessageShared";
import {
  absoluteShareURL,
  createChatShare,
  getChatShareCandidates,
  type ChatShareCandidates,
} from "../api";
import { normalizeMessageArtifacts } from "../media";
import { artifactDataFor, normalizeChatShareMessage, userQueryFor } from "../message";

const route = useRoute();
const router = useRouter();
const candidates = ref<ChatShareCandidates | null>(null);
const selectedTurnIDs = ref<Set<string>>(new Set());
const loading = ref(true);
const creating = ref(false);
const errorMessage = ref("");
const generatedLink = ref("");
const isNarrow = ref(false);
let mediaQuery: MediaQueryList | null = null;

const sessionID = computed(() => String(route.params.chatid || route.params.sessionId || "").trim());
const messages = computed(() => (candidates.value?.messages || []).map(normalizeChatShareMessage));
const selectableTurnIDs = computed(() => Array.from(new Set(
  messages.value
    .filter((message) => message.selectable !== false)
    .map(messageTurnID)
    .filter(Boolean),
)));
const selectedCount = computed(() => messages.value.filter(isMessageSelected).length);
const allSelected = computed(() =>
  selectableTurnIDs.value.length > 0 && selectableTurnIDs.value.every((id) => selectedTurnIDs.value.has(id)),
);
const partiallySelected = computed(() => selectedCount.value > 0 && !allSelected.value);
const mobileLayout = computed(() => route.name === "mobile-chat-share" || isNarrow.value);

function replaceSelection(next: Set<string>) {
  selectedTurnIDs.value = next;
  generatedLink.value = "";
}

function messageTurnID(message: Record<string, any>) {
  return String(message.turn_id || "").trim();
}

function isMessageSelected(message: Record<string, any>) {
  const turnID = messageTurnID(message);
  return Boolean(turnID && selectedTurnIDs.value.has(turnID));
}

function toggleMessage(message: Record<string, any>, selectable: boolean) {
  if (!selectable) return;
  const turnID = messageTurnID(message);
  if (!turnID) return;
  const next = new Set(selectedTurnIDs.value);
  if (next.has(turnID)) next.delete(turnID);
  else next.add(turnID);
  replaceSelection(next);
}

function toggleAll() {
  replaceSelection(allSelected.value ? new Set() : new Set(selectableTurnIDs.value));
}

async function loadCandidates() {
  if (!sessionID.value) {
    errorMessage.value = "会话地址无效";
    loading.value = false;
    return;
  }
  loading.value = true;
  errorMessage.value = "";
  try {
    const resp: any = await getChatShareCandidates(sessionID.value);
    if (!resp?.success || !resp?.data) throw new Error(resp?.message || "无法加载对话");
    candidates.value = resp.data;
    const defaults = (resp.data.messages || [])
      .filter((message: any) => message.selectable !== false)
      .map((message: any) => messageTurnID(message))
      .filter(Boolean);
    selectedTurnIDs.value = new Set(defaults);
    document.title = `选择分享内容 - ${resp.data.title || "WeKnora"}`;
  } catch (error: any) {
    errorMessage.value = error?.message || "无法加载对话";
  } finally {
    loading.value = false;
  }
}

async function copySelectedLink() {
  if (selectedCount.value === 0 || creating.value) return;
  creating.value = true;
  try {
    if (!generatedLink.value) {
      const orderedIDs = messages.value
        .filter(isMessageSelected)
        .map((message) => String(message.id));
      const resp: any = await createChatShare(sessionID.value, orderedIDs);
      const link = absoluteShareURL(resp?.data?.url, resp?.data?.token);
      if (!link) throw new Error("分享链接生成失败");
      generatedLink.value = link;
    }
    await copyTextToClipboard(generatedLink.value);
    MessagePlugin.success("分享链接已复制");
  } catch (error: any) {
    MessagePlugin.error(error?.message || "分享失败");
  } finally {
    creating.value = false;
  }
}

function cancelSelection() {
  if (route.name === "mobile-chat-share") {
    void router.replace({ name: "mobile-chat", params: { sessionId: sessionID.value } });
    return;
  }
  void router.replace({ name: "chat", params: { chatid: sessionID.value } });
}

function syncLayout(query: MediaQueryList | MediaQueryListEvent) {
  isNarrow.value = query.matches;
}

onMounted(() => {
  mediaQuery = window.matchMedia("(max-width: 768px)");
  syncLayout(mediaQuery);
  mediaQuery.addEventListener?.("change", syncLayout);
  void loadCandidates();
});

onBeforeUnmount(() => mediaQuery?.removeEventListener?.("change", syncLayout));
</script>

<template>
  <main class="chat-share-select" :class="{ 'is-mobile': mobileLayout }">
    <header class="selection-header">
      <span class="selection-header__spacer" aria-hidden="true"></span>
      <div class="selection-header__title">
        <strong>选择分享内容</strong>
        <span v-if="candidates?.title">{{ candidates.title }}</span>
      </div>
      <button type="button" class="selection-cancel" @click="cancelSelection">取消</button>
    </header>

    <div v-if="loading" class="selection-state">
      <t-loading size="small" />
      <span>正在加载对话</span>
    </div>

    <div v-else-if="errorMessage" class="selection-state selection-state--error">
      <strong>无法选择分享内容</strong>
      <span>{{ errorMessage }}</span>
      <button type="button" @click="loadCandidates">重新加载</button>
    </div>

    <template v-else>
      <div class="selection-scroll">
        <div class="selection-list">
          <article
            v-for="(message, index) in messages"
            :key="message.id || `${message.role}-${index}`"
            class="selection-message"
            :class="[
              `selection-message--${message.role}`,
              { 'is-disabled': message.selectable === false, 'is-selected': isMessageSelected(message) },
            ]"
          >
            <label class="selection-checkbox" :title="message.disabled_reason || '选择这条消息'">
              <input
                type="checkbox"
                :checked="isMessageSelected(message)"
                :disabled="message.selectable === false"
                :aria-label="message.role === 'user' ? '选择用户问题' : '选择助手回答'"
                @change="toggleMessage(message, message.selectable !== false)"
              />
            </label>

            <div class="selection-message__body">
              <MobileChatMessage
                v-if="mobileLayout"
                :message="message"
                :share-mode="true"
              />
              <template v-else>
                <usermsg
                  v-if="message.role === 'user'"
                  :content="message.content"
                  :mentioned_items="message.mentioned_items"
                  :images="message.images"
                  :attachments="message.attachments"
                  :share-mode="true"
                />
                <botmsg
                  v-else
                  :content="message.content"
                  :session="message"
                  :session-id="sessionID"
                  :user-query="userQueryFor(messages, index)"
                  :share-mode="true"
                />
                <GeneralAgentArtifactsResult
                  v-if="message.role === 'assistant' && normalizeMessageArtifacts(message.artifacts).length"
                  class="selection-artifacts"
                  :data="artifactDataFor(message)"
                  :share-mode="true"
                />
              </template>
              <div v-if="message.disabled_reason" class="selection-message__reason">
                {{ message.disabled_reason }}
              </div>
            </div>
          </article>
          <div v-if="messages.length === 0" class="selection-empty">当前会话暂无可分享内容</div>
        </div>
      </div>

      <footer class="selection-footer">
        <label class="selection-all">
          <input
            type="checkbox"
            :checked="allSelected"
            :indeterminate="partiallySelected"
            :disabled="selectableTurnIDs.length === 0"
            @change="toggleAll"
          />
          <span>全选</span>
        </label>
        <div class="selection-actions">
          <span class="selection-count">已选 {{ selectedCount }} 条</span>
          <button
            type="button"
            class="selection-copy"
            :disabled="selectedCount === 0 || creating"
            @click="copySelectedLink"
          >
            {{ creating ? "正在生成" : "复制链接" }}
          </button>
        </div>
      </footer>
    </template>
  </main>
</template>

<style scoped>
.chat-share-select {
  display: grid;
  width: 100%;
  min-width: 0;
  height: 100%;
  min-height: 0;
  grid-template-rows: auto minmax(0, 1fr) auto;
  overflow: hidden;
  background: #f7f8f8;
  color: #17211c;
}

.selection-header {
  display: grid;
  min-height: 64px;
  grid-template-columns: 96px minmax(0, 1fr) 96px;
  align-items: center;
  padding: 0 28px;
  border-bottom: 1px solid #e1e6e3;
  background: rgba(255, 255, 255, 0.96);
}

.selection-header__title {
  display: flex;
  min-width: 0;
  flex-direction: column;
  align-items: center;
  gap: 2px;
}

.selection-header__title strong {
  font-size: 16px;
  font-weight: 650;
}

.selection-header__title span {
  max-width: 100%;
  overflow: hidden;
  color: #758078;
  font-size: 12px;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.selection-cancel {
  justify-self: end;
  border: 0;
  background: transparent;
  color: #26342c;
  cursor: pointer;
  font-size: 14px;
}

.selection-scroll {
  min-height: 0;
  overflow-y: auto;
}

.selection-list {
  width: min(980px, calc(100% - 72px));
  margin: 0 auto;
  padding: 24px 0 36px;
}

.selection-message {
  display: grid;
  grid-template-columns: 30px minmax(0, 1fr);
  gap: 8px;
  margin-bottom: 12px;
  padding: 14px 18px 14px 12px;
  border: 1px solid transparent;
  border-radius: 12px;
  background: #fff;
  transition: border-color 0.16s ease, opacity 0.16s ease;
}

.selection-message--assistant {
  background: #f0f2f1;
}

.selection-message.is-selected {
  border-color: rgba(0, 82, 217, 0.2);
}

.selection-message.is-disabled {
  opacity: 0.58;
}

.selection-checkbox {
  display: flex;
  justify-content: center;
  padding-top: 2px;
}

.selection-checkbox input,
.selection-all input {
  width: 17px;
  height: 17px;
  margin: 0;
  accent-color: #1769e0;
  cursor: pointer;
}

.selection-checkbox input:disabled,
.selection-all input:disabled {
  cursor: not-allowed;
}

.selection-message__body {
  min-width: 0;
}

.selection-message__body :deep(.answer-toolbar),
.selection-message__body :deep(.chat-request-info-button) {
  display: none !important;
}

.selection-message__reason {
  margin-top: 8px;
  color: #7c6b42;
  font-size: 12px;
}

.selection-footer {
  display: flex;
  min-height: 68px;
  align-items: center;
  justify-content: space-between;
  gap: 20px;
  padding: 10px max(28px, calc((100% - 980px) / 2));
  border-top: 1px solid #dfe5e1;
  background: rgba(255, 255, 255, 0.97);
  box-shadow: 0 -8px 24px rgba(30, 45, 37, 0.04);
}

.selection-all,
.selection-actions {
  display: flex;
  align-items: center;
}

.selection-all {
  gap: 8px;
  color: #27352e;
  cursor: pointer;
  font-size: 14px;
}

.selection-actions {
  gap: 14px;
}

.selection-count {
  color: #728078;
  font-size: 13px;
}

.selection-copy,
.selection-state button {
  min-width: 92px;
  padding: 9px 18px;
  border: 0;
  border-radius: 9px;
  background: #1769e0;
  color: #fff;
  cursor: pointer;
  font-size: 14px;
  font-weight: 600;
}

.selection-copy:disabled {
  background: #b9c5bd;
  cursor: not-allowed;
}

.selection-state {
  grid-row: 2 / 4;
  display: flex;
  min-height: 0;
  align-items: center;
  justify-content: center;
  gap: 10px;
  color: #617067;
}

.selection-state--error {
  flex-direction: column;
}

.selection-empty {
  padding: 48px 0;
  color: #7a877f;
  text-align: center;
}

.chat-share-select.is-mobile {
  height: 100dvh;
  background: #f5f7f8;
}

.chat-share-select.is-mobile .selection-header {
  min-height: 54px;
  grid-template-columns: 64px minmax(0, 1fr) 64px;
  padding: calc(env(safe-area-inset-top) + 6px) 14px 6px;
}

.chat-share-select.is-mobile .selection-header__title strong {
  font-size: 15px;
}

.chat-share-select.is-mobile .selection-list {
  width: 100%;
  padding: 12px 10px 24px;
}

.chat-share-select.is-mobile .selection-message {
  grid-template-columns: 28px minmax(0, 1fr);
  gap: 4px;
  margin-bottom: 10px;
  padding: 12px 10px 12px 7px;
  border-radius: 10px;
}

.chat-share-select.is-mobile .selection-message--assistant {
  background: #fff;
}

.chat-share-select.is-mobile .selection-footer {
  min-height: 62px;
  padding: 8px 12px calc(env(safe-area-inset-bottom) + 8px);
}

.chat-share-select.is-mobile .selection-count {
  display: none;
}

.chat-share-select.is-mobile .selection-copy {
  min-width: 104px;
}

@media (max-width: 460px) {
  .selection-header__title span {
    display: none;
  }
}
</style>
