<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, reactive, ref, watch } from "vue";
import { useRoute, useRouter } from "vue-router";
import { useI18n } from "vue-i18n";
import { MessagePlugin } from "tdesign-vue-next";
import {
  createSessions,
  delSession,
  getMessageList,
  getSessionsList,
  pinSession,
  stopSession,
  unpinSession,
} from "@/api/chat";
import { useStream } from "@/api/chat/streame";
import { listKnowledgeFiles } from "@/api/knowledge-base";
import { BUILTIN_QUICK_ANSWER_ID, BUILTIN_SIMPLE_CHAT_ID, type CustomAgent } from "@/api/agent";
import { listSkills, type SkillInfo } from "@/api/skill";
import type { ModelConfig } from "@/api/model";
import { useChatStreamHandler } from "@/composables/useChatStreamHandler";
import { useAuthStore } from "@/stores/auth";
import { useChatResourcesStore } from "@/stores/chatResources";
import { useSettingsStore } from "@/stores/settings";
import { agentPinKey, useChatAgentPins } from "@/custom/modules/agentPins/agentPins";
import { listSessionStatuses, markSessionRead, type SessionStatusMap } from "@/custom/modules/sessionState/api";
import { useChatSkillPins, type SkillPinKind } from "@/custom/modules/skillhub/skillPins";
import MobileChatMessage from "../components/MobileChatMessage.vue";
import MobileResourceRail from "../components/MobileResourceRail.vue";
import {
  agentLabel,
  fileToBase64,
  fileToDataUrl,
  formatFileSize,
  modelLabel,
  skillLabel,
  type MobileMentionItem,
  type MobileResourceChip,
  type MobileUploadAttachment,
} from "../utils";

type ChatMessage = Record<string, any>;
type SheetTab = "agent" | "model" | "context" | "skill";
type KnowledgeSheetTab = "kb" | "file";
type SkillSelectionMode = "all" | "selected" | "none";

const route = useRoute();
const router = useRouter();
const { t } = useI18n();
const authStore = useAuthStore();
const settingsStore = useSettingsStore();
const chatResources = useChatResourcesStore();
const agentPins = useChatAgentPins();
const lightweightPins = useChatSkillPins("lightweight");
const professionalPins = useChatSkillPins("professional");

const messagesList = reactive<ChatMessage[]>([]);
const currentSessionId = ref(String(route.params.sessionId || ""));
const inputValue = ref("");
const loading = ref(false);
const isReplying = ref(false);
const isLoadingHistory = ref(false);
const sessionsLoading = ref(false);
const resourcesLoading = ref(false);
const drawerOpen = ref(false);
const sheetOpen = ref(false);
const activeSheet = ref<SheetTab>("context");
const activeKnowledgeTab = ref<KnowledgeSheetTab>("kb");
const activeSkillTab = ref<SkillPinKind>("lightweight");
const currentAssistantMessageId = ref("");
const fullContent = ref("");
const scrollRef = ref<HTMLElement | null>(null);
const textareaRef = ref<HTMLTextAreaElement | null>(null);
const imageInputRef = ref<HTMLInputElement | null>(null);
const attachmentInputRef = ref<HTMLInputElement | null>(null);
const shouldFollowAnswer = ref(true);

const sessions = ref<any[]>([]);
const sessionStatuses = ref<SessionStatusMap>({});
const skills = ref<SkillInfo[]>([]);
const professionalSkills = ref<SkillInfo[]>([]);
const skillsAvailable = ref(true);
const professionalSkillsAvailable = ref(false);
const knowledgeFiles = ref<any[]>([]);
const activeFileKbId = ref("");
const fileListLoading = ref(false);
const loadedFileKbIds = ref<Set<string>>(new Set());
const pendingImages = ref<File[]>([]);
const pendingAttachments = ref<MobileUploadAttachment[]>([]);
const startedSessionIds = ref<Set<string>>(new Set());
const sessionPinningIds = ref<Set<string>>(new Set());
const sessionActionMenuId = ref("");
const isResumingStream = ref(false);
const resumeFailedMessageIds = ref<Set<string>>(new Set());
let recoverPollTimer: ReturnType<typeof setTimeout> | null = null;
let recoverPollAttempts = 0;
let suppressNextResume = false;
let activeStreamScrollIntent: "answer" | "non-answer" | "" = "";
let lastMessageScrollTop = 0;
let programmaticScrollUntil = 0;
let sessionsSyncing = false;

const { onChunk, startStream, stopStream, error } = useStream();

const SCROLL_BOTTOM_THRESHOLD = 140;
const STREAM_NON_ANSWER_TYPES = new Set([
  "agent_query",
  "thinking",
  "tool_call",
  "tool_result",
  "agent_progress",
  "reflection",
  "tool_approval_required",
  "tool_approval_resolved",
  "mcp_oauth_required",
  "mcp_oauth_resolved",
  "session_title",
  "stop",
]);

const isNearMessageBottom = (el: HTMLElement) =>
  el.scrollHeight - el.scrollTop - el.clientHeight <= SCROLL_BOTTOM_THRESHOLD;

const inferStreamScrollIntent = (data: ChatMessage): "answer" | "non-answer" => {
  const responseType = String(data.response_type || "");
  if (STREAM_NON_ANSWER_TYPES.has(responseType)) return "non-answer";
  if (responseType === "answer" || responseType === "complete" || responseType === "references") return "answer";

  const nextContent = `${fullContent.value || ""}${data.content || ""}`;
  if (nextContent.includes("<think>")) {
    const thinkCloseTag = "</think>";
    if (!nextContent.includes(thinkCloseTag)) return "non-answer";
    const visibleAnswer = nextContent.slice(nextContent.lastIndexOf(thinkCloseTag) + thinkCloseTag.length).trim();
    if (!visibleAnswer) return "non-answer";
  }
  return "answer";
};

const scrollToBottom = async (force = false) => {
  const intent = activeStreamScrollIntent;
  if (intent === "non-answer") return;
  await nextTick();
  const el = scrollRef.value;
  if (!el) return;
  if (!force && !shouldFollowAnswer.value && !isNearMessageBottom(el)) return;
  if (!force && intent === "answer" && !shouldFollowAnswer.value) return;
  if (force || shouldFollowAnswer.value || isNearMessageBottom(el)) {
    programmaticScrollUntil = Date.now() + 180;
    el.scrollTop = el.scrollHeight;
    lastMessageScrollTop = el.scrollTop;
    shouldFollowAnswer.value = true;
  }
};

const handleMessageScroll = () => {
  const el = scrollRef.value;
  if (!el) return;
  const currentTop = el.scrollTop;
  const nearBottom = isNearMessageBottom(el);
  if (nearBottom) {
    shouldFollowAnswer.value = true;
  } else if (Date.now() > programmaticScrollUntil && currentTop < lastMessageScrollTop - 2) {
    shouldFollowAnswer.value = false;
  }
  lastMessageScrollTop = currentTop;
};

const {
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
  isAgentStreamSession: () => settingsStore.isAgentStreamMode,
  scrollToBottom,
  preserveIncompleteStreamReactive: true,
  onAfterMsgList: () => {
    void resumeTrailingIncompleteReply();
  },
  onError: (message) => MessagePlugin.error(message),
});

const knowledgeBases = computed(() => chatResources.validKnowledgeBases);
const agents = computed(() => chatResources.agents);
const chatModels = computed(() => chatResources.chatModels);
const modelTypeShortKeyByBackendType: Record<string, "chat" | "embedding" | "rerank" | "vllm" | "asr"> = {
  KnowledgeQA: "chat",
  Embedding: "embedding",
  Rerank: "rerank",
  VLLM: "vllm",
  ASR: "asr",
};
const mobileModelTypeLabel = (type?: ModelConfig["type"] | string) => {
  const key = modelTypeShortKeyByBackendType[String(type || "")];
  return key ? t(`modelSettings.typeShort.${key}`) : String(type || "");
};
const selectedKbIds = computed(() => settingsStore.settings.selectedKnowledgeBases || []);
const selectedFileIds = computed(() => settingsStore.settings.selectedFiles || []);
const activeFileKnowledgeBase = computed(() =>
  knowledgeBases.value.find((kb) => kb.id === activeFileKbId.value) || null,
);
const activeFileRows = computed(() =>
  activeFileKbId.value ? knowledgeFiles.value.filter((file) => file.kb_id === activeFileKbId.value) : [],
);
const selectedSkillNames = computed(() => {
  const names = [
    ...(settingsStore.settings.selectedSkillNames || []),
    ...(settingsStore.settings.selectedSkills || []),
  ];
  return Array.from(new Set(names.map((name) => String(name || "").trim()).filter(Boolean)));
});
const selectedAgentId = computed(() => settingsStore.selectedAgentId || BUILTIN_QUICK_ANSWER_ID);
const selectedModelId = computed(() => settingsStore.conversationModels.selectedChatModelId || "");

const selectedAgent = computed(() => {
  return agents.value.find((agent) => agent.id === selectedAgentId.value) || null;
});

const mobileAgentRows = computed<CustomAgent[]>(() => {
  const quickAgent = {
    id: BUILTIN_QUICK_ANSWER_ID,
    name: "快速问答",
    description: "知识库问答模式",
    is_builtin: true,
    config: {},
  } as CustomAgent;
  const merged = [
    quickAgent,
    ...agents.value.filter((agent) => agent.id !== BUILTIN_QUICK_ANSWER_ID),
  ];
  return agentPins.sortPinnedFirst(merged, (agent) => agentPinKey(agent.id));
});

const currentAgentConfig = computed(() => selectedAgent.value?.config || {});

const agentProfessionalSkillsSelectionMode = computed<SkillSelectionMode>(() => {
  return currentAgentConfig.value?.professional_skills_selection_mode || "none";
});

const agentConfiguredProfessionalSkillNames = computed(() => {
  const names = currentAgentConfig.value?.selected_professional_skills || [];
  return Array.from(new Set(names.map((name: string) => String(name || "").trim()).filter(Boolean)));
});

const professionalOptions = computed(() => {
  const mode = agentProfessionalSkillsSelectionMode.value;
  if (mode === "none") return [];
  if (mode === "selected") {
    const allowed = new Set(agentConfiguredProfessionalSkillNames.value);
    return professionalSkills.value.filter((skill) => allowed.has(skill.name));
  }
  return professionalSkills.value;
});

const hasProfessionalSkillTab = computed(() =>
  professionalSkillsAvailable.value &&
  agentProfessionalSkillsSelectionMode.value !== "none" &&
  professionalOptions.value.length > 0,
);

const selectedProfessionalSkillNames = computed(() => {
  const names = settingsStore.settings.selectedProfessionalSkillNames || [];
  const normalized = Array.from(new Set(names.map((name) => String(name || "").trim()).filter(Boolean)));
  const mode = agentProfessionalSkillsSelectionMode.value;
  if (mode === "none") return [];
  if (mode === "selected") {
    const allowed = new Set(agentConfiguredProfessionalSkillNames.value);
    return normalized.filter((name) => allowed.has(name));
  }
  return normalized;
});

const selectedSkillContextCount = computed(() => selectedSkillNames.value.length + selectedProfessionalSkillNames.value.length);

const sortedLightweightSkills = computed(() =>
  lightweightPins.sortPinnedFirst(
    skills.value.filter((skill) => skill.kind !== "professional"),
    (skill) => skill.name,
  ),
);

const sortedProfessionalSkills = computed(() =>
  professionalPins.sortPinnedFirst(professionalOptions.value, (skill) => skill.name),
);

const activeSkillRows = computed(() => {
  if (activeSkillTab.value === "professional" && hasProfessionalSkillTab.value) {
    return sortedProfessionalSkills.value;
  }
  return sortedLightweightSkills.value;
});

const selectedModel = computed(() => {
  return chatModels.value.find((model) => model.id === selectedModelId.value) || chatModels.value[0] || null;
});

const webSearchProviders = computed(() => chatResources.webSearchProviders || []);

const agentWebSearchEnabled = computed(() => {
  if (!selectedAgent.value?.config) return true;
  return currentAgentConfig.value?.web_search_enabled ?? true;
});

const agentWebSearchProviderId = computed(() => {
  if (!selectedAgent.value?.config) return "";
  return currentAgentConfig.value?.web_search_provider_id || "";
});

const isWebSearchDisabledByAgent = computed(() => !!selectedAgent.value?.config && agentWebSearchEnabled.value === false);

const isWebSearchConfigured = computed(() => {
  const providerId = agentWebSearchProviderId.value;
  if (providerId) {
    return webSearchProviders.value.some((provider: any) => provider.id === providerId);
  }
  return webSearchProviders.value.some((provider: any) => provider.is_default);
});

const canUseWebSearch = computed(() =>
  settingsStore.isWebSearchEnabled && isWebSearchConfigured.value && !isWebSearchDisabledByAgent.value,
);

const compactTitle = computed(() => {
  const row = sessions.value.find((item) => item.id === currentSessionId.value);
  return row?.title || "新对话";
});

const selectedResourceChips = computed<MobileResourceChip[]>(() => {
  const chips: MobileResourceChip[] = [];
  for (const kbId of selectedKbIds.value) {
    const kb = knowledgeBases.value.find((item) => item.id === kbId);
    chips.push({ id: kbId, type: "kb", name: kb?.name || "知识库" });
  }
  for (const fileId of selectedFileIds.value) {
    const file = knowledgeFiles.value.find((item) => item.id === fileId);
    chips.push({ id: fileId, type: "file", name: file?.file_name || file?.display_name || fileId });
  }
  for (const name of selectedSkillNames.value) {
    chips.push({ id: `skill:${name}`, type: "skill", name });
  }
  for (const name of selectedProfessionalSkillNames.value) {
    chips.push({ id: `professional:${name}`, type: "skill", name });
  }
  pendingImages.value.forEach((file, index) => {
    chips.push({ id: `image:${index}:${file.name}`, type: "image", name: file.name || `图片 ${index + 1}` });
  });
  pendingAttachments.value.forEach((file, index) => {
    chips.push({
      id: `attachment:${index}:${file.name}`,
      type: "attachment",
      name: file.name,
      meta: formatFileSize(file.size),
    });
  });
  return chips;
});

const selectedKnowledgeContextCount = computed(() => selectedKbIds.value.length + selectedFileIds.value.length);

type SessionGroup = {
  key: string;
  label: string;
  items: any[];
};

const dayMs = 24 * 60 * 60 * 1000;

const sessionTimestamp = (item: any) => {
  const timestamp = Date.parse(item?.updated_at || item?.created_at || "");
  return Number.isFinite(timestamp) ? timestamp : 0;
};

const isSessionUnread = (item: any) => {
  const id = String(item?.id || "");
  if (!id || id === currentSessionId.value) return false;
  return sessionStatuses.value[id]?.unread === true;
};

const mergeSessionStatuses = (statuses: SessionStatusMap) => {
  if (!statuses || Object.keys(statuses).length === 0) return;
  sessionStatuses.value = { ...sessionStatuses.value, ...statuses };
  const currentStatus = currentSessionId.value ? statuses[currentSessionId.value] : null;
  if (currentStatus?.unread) {
    void markCurrentSessionRead();
  }
};

const refreshSessionStatuses = async (rows = sessions.value) => {
  const ids = rows.map((item) => String(item?.id || "")).filter(Boolean);
  if (!ids.length) return;
  try {
    mergeSessionStatuses(await listSessionStatuses(ids));
  } catch (err) {
    console.warn("[mobile] failed to refresh session status", err);
  }
};

const markSessionReadLocal = (sessionId: string) => {
  if (!sessionId) return;
  const current = sessionStatuses.value[sessionId];
  if (!current) return;
  sessionStatuses.value = {
    ...sessionStatuses.value,
    [sessionId]: { ...current, unread: false },
  };
};

const markSessionAsRead = async (sessionId: string) => {
  if (!sessionId) return;
  markSessionReadLocal(sessionId);
  try {
    const status = await markSessionRead(sessionId);
    if (status) mergeSessionStatuses({ [sessionId]: status });
  } catch (err) {
    console.warn("[mobile] failed to mark session read", err);
  }
};

function markCurrentSessionRead() {
  if (!currentSessionId.value) return;
  void markSessionAsRead(currentSessionId.value);
}

const monthLabel = (date: Date) => `${date.getFullYear()}年${date.getMonth() + 1}月`;

const sessionGroups = computed<SessionGroup[]>(() => {
  const todayStart = new Date();
  todayStart.setHours(0, 0, 0, 0);
  const todayTime = todayStart.getTime();
  const sevenDaysTime = todayTime - 7 * dayMs;
  const thirtyDaysTime = todayTime - 30 * dayMs;
  const pinned: SessionGroup = { key: "pinned", label: "置顶", items: [] };
  const buckets: SessionGroup[] = [
    { key: "today", label: "今天", items: [] },
    { key: "seven", label: "7天内", items: [] },
    { key: "thirty", label: "30天内", items: [] },
  ];
  const monthBuckets = new Map<string, SessionGroup>();

  sessions.value
    .slice()
    .sort((a, b) => sessionTimestamp(b) - sessionTimestamp(a))
    .forEach((item) => {
      if (item.is_pinned) {
        pinned.items.push(item);
        return;
      }
      const timestamp = sessionTimestamp(item);
      if (!timestamp) return;
      if (timestamp >= todayTime) {
        buckets[0].items.push(item);
        return;
      }
      if (timestamp >= sevenDaysTime) {
        buckets[1].items.push(item);
        return;
      }
      if (timestamp >= thirtyDaysTime) {
        buckets[2].items.push(item);
        return;
      }

      const date = new Date(timestamp);
      const key = `month-${date.getFullYear()}-${date.getMonth()}`;
      if (!monthBuckets.has(key)) {
        monthBuckets.set(key, { key, label: monthLabel(date), items: [] });
      }
      monthBuckets.get(key)?.items.push(item);
    });

  pinned.items.sort((a, b) => {
    const aTime = Date.parse(a?.pinned_at || a?.updated_at || a?.created_at || "");
    const bTime = Date.parse(b?.pinned_at || b?.updated_at || b?.created_at || "");
    return (Number.isFinite(bTime) ? bTime : 0) - (Number.isFinite(aTime) ? aTime : 0);
  });

  return [pinned, ...buckets, ...monthBuckets.values()].filter((group) => group.items.length > 0);
});

const userDisplayName = computed(() => authStore.user?.username || "企业用户");

const userAvatarText = computed(() => {
  const name = userDisplayName.value.trim();
  return (name[0] || "W").toUpperCase();
});

const openSheet = (tab: SheetTab) => {
  activeSheet.value = tab;
  sheetOpen.value = true;
  if (tab === "context") {
    void loadKnowledgeChildren();
  }
  if (tab === "skill" && hasProfessionalSkillTab.value) {
    activeSkillTab.value = "professional";
  }
};

const closeSheet = () => {
  sheetOpen.value = false;
};

const openSettings = () => {
  drawerOpen.value = false;
  router.push({
    path: "/settings",
    query: { returnTo: route.fullPath || "/chat" },
  });
};

const ensureDefaultModel = () => {
  if (selectedModelId.value || !chatModels.value.length) return;
  const first = chatModels.value[0];
  settingsStore.updateConversationModels({
    summaryModelId: first.id || "",
    selectedChatModelId: first.id || "",
  });
};

const loadResources = async () => {
  resourcesLoading.value = true;
  try {
    await chatResources.prefetchChatInput();
    ensureDefaultModel();
    const skillResponse = await listSkills().catch(() => null);
    skillsAvailable.value = skillResponse?.skills_available !== false;
    professionalSkillsAvailable.value = skillResponse?.professional_skills_available === true;
    skills.value = (skillResponse?.data || []).filter((skill) => skill.kind !== "professional");
    professionalSkills.value = skillResponse?.professional_data || [];
    await loadKnowledgeChildren();
  } finally {
    resourcesLoading.value = false;
  }
};

const responseList = (res: any) => {
  if (Array.isArray(res?.data)) return res.data;
  if (Array.isArray(res?.data?.data)) return res.data.data;
  if (Array.isArray(res?.data?.items)) return res.data.items;
  if (Array.isArray(res?.items)) return res.items;
  return [];
};

const normalizeKnowledgeFile = (kbId: string, file: any) => {
  const kb = knowledgeBases.value.find((item) => item.id === kbId);
  return {
    ...file,
    kb_id: kbId,
    kb_name: kb?.name,
    display_name: file.file_name || file.title || file.source || "未命名文档",
  };
};

const mergeKnowledgeFiles = (kbId: string, files: any[]) => {
  knowledgeFiles.value = [
    ...knowledgeFiles.value.filter((file) => file.kb_id !== kbId),
    ...files,
  ];
};

const loadKnowledgeFilesForKb = async (kbId: string, force = false) => {
  if (!kbId) return;
  if (!force && loadedFileKbIds.value.has(kbId)) return;
  fileListLoading.value = true;
  try {
    const res: any = await listKnowledgeFiles(kbId, { page: 1, page_size: 100 }).catch(() => ({ data: [] }));
    mergeKnowledgeFiles(kbId, responseList(res).map((file: any) => normalizeKnowledgeFile(kbId, file)));
    loadedFileKbIds.value = new Set([...loadedFileKbIds.value, kbId]);
  } finally {
    fileListLoading.value = false;
  }
};

const sessionHasActivity = (item: any) => {
  if (!item?.id) return false;
  if (startedSessionIds.value.has(item.id)) return true;
  if (String(item.title || "").trim()) return true;
  if (item.last_request_state && Object.keys(item.last_request_state).length > 0) return true;

  const createdAt = Date.parse(item.created_at || "");
  const updatedAt = Date.parse(item.updated_at || "");
  return Number.isFinite(createdAt) && Number.isFinite(updatedAt) && updatedAt - createdAt > 1000;
};

const markSessionStarted = (sessionId: string) => {
  if (!sessionId) return;
  const next = new Set(startedSessionIds.value);
  next.add(sessionId);
  startedSessionIds.value = next;
};

const upsertStartedSession = (sessionId: string) => {
  if (!sessionId) return;
  const now = new Date().toISOString();
  const index = sessions.value.findIndex((item) => item.id === sessionId);
  const row = {
    id: sessionId,
    title: "",
    created_at: now,
    updated_at: now,
  };
  if (index >= 0) {
    sessions.value[index] = { ...sessions.value[index], updated_at: sessions.value[index].updated_at || now };
    return;
  }
  sessions.value.unshift(row);
};

const loadKnowledgeChildren = async () => {
  if (activeFileKbId.value && !knowledgeBases.value.some((kb) => kb.id === activeFileKbId.value)) {
    activeFileKbId.value = "";
  }

  const kbIds = new Set(
    Object.values(settingsStore.settings.selectedFileKbMap || {}).filter(Boolean),
  );
  if (activeFileKbId.value) kbIds.add(activeFileKbId.value);

  if (!knowledgeBases.value.length) {
    knowledgeFiles.value = [];
    loadedFileKbIds.value = new Set();
    return;
  }

  await Promise.all([...kbIds].map((kbId) => loadKnowledgeFilesForKb(kbId)));
};

const loadSessions = async (options: { silent?: boolean } = {}) => {
  if (sessionsSyncing) return;
  sessionsSyncing = true;
  if (!options.silent) sessionsLoading.value = true;
  try {
    const res: any = await getSessionsList(1, 40);
    const rows = (res?.data || []).filter(sessionHasActivity);
    sessions.value = rows;
    await refreshSessionStatuses(rows);
  } finally {
    sessionsSyncing = false;
    if (!options.silent) sessionsLoading.value = false;
  }
};

const ensureSession = async () => {
  if (currentSessionId.value) return currentSessionId.value;
  const response: any = await createSessions({});
  const id = response?.data?.id;
  if (!id) throw new Error("创建会话失败");
  currentSessionId.value = id;
  await router.replace(`/chat/${id}`);
  return id;
};

const loadMessages = async () => {
  if (!currentSessionId.value) return;
  isLoadingHistory.value = true;
  let shouldScrollToBottom = false;
  try {
    const res: any = await getMessageList({
      session_id: currentSessionId.value,
      limit: 40,
      created_at: "",
    });
    messagesList.splice(0);
    await handleMsgList(res?.data || []);
    shouldScrollToBottom = true;
  } finally {
    isLoadingHistory.value = false;
    if (shouldScrollToBottom) {
      await nextTick();
      await scrollToBottom(true);
    }
    markCurrentSessionRead();
  }
};

const clearRecoverPoll = () => {
  if (recoverPollTimer) {
    clearTimeout(recoverPollTimer);
    recoverPollTimer = null;
  }
  recoverPollAttempts = 0;
};

const trailingIncompleteAssistant = () => {
  const last = messagesList[messagesList.length - 1];
  if (last?.role === "assistant" && !last.is_completed) return last;
  return null;
};

const pollIncompleteReply = (sessionId: string, messageId: string) => {
  clearRecoverPoll();
  if (!sessionId || !messageId) return;

  const tick = async () => {
    recoverPollTimer = null;
    if (currentSessionId.value !== sessionId) return;
    recoverPollAttempts += 1;

    try {
      const res: any = await getMessageList({
        session_id: sessionId,
        limit: 40,
        created_at: "",
      });
      const rows = res?.data || [];
      suppressNextResume = true;
      messagesList.splice(0);
      await handleMsgList(rows);
      await scrollToBottom(true);

      const target = rows.find((item: any) => item.id === messageId || item.request_id === messageId);
      if (!target || target.is_completed) {
        isReplying.value = false;
        loading.value = false;
        currentAssistantMessageId.value = "";
        void markSessionAsRead(sessionId);
        void loadSessions({ silent: true });
        clearRecoverPoll();
        return;
      }
    } catch (err) {
      console.warn("[mobile] recover incomplete reply failed", err);
    }

    if (recoverPollAttempts >= 48) {
      markInFlightAssistantStopped(messageId);
      isReplying.value = false;
      loading.value = false;
      MessagePlugin.warning("当前回复暂时无法继续，请稍后重新打开会话查看结果");
      clearRecoverPoll();
      return;
    }

    recoverPollTimer = setTimeout(tick, 2500);
  };

  recoverPollTimer = setTimeout(tick, 2500);
};

async function resumeTrailingIncompleteReply() {
  if (suppressNextResume) {
    suppressNextResume = false;
    return;
  }
  const message = trailingIncompleteAssistant();
  if (!message || !currentSessionId.value) return;

  const messageId = String(message.id || message.request_id || "");
  if (!messageId || resumeFailedMessageIds.value.has(messageId)) return;

  currentAssistantMessageId.value = messageId;
  isReplying.value = true;
  loading.value = true;
  isResumingStream.value = true;

  await startStream({
    session_id: currentSessionId.value,
    query: messageId,
    method: "GET",
    url: "/api/v1/sessions/continue-stream",
  });
  void markCurrentSessionRead();
}

const startNewChat = async () => {
  clearRecoverPoll();
  stopStream();
  messagesList.splice(0);
  shouldFollowAnswer.value = true;
  lastMessageScrollTop = 0;
  currentSessionId.value = "";
  currentAssistantMessageId.value = "";
  fullContent.value = "";
  inputValue.value = "";
  loading.value = false;
  isReplying.value = false;
  drawerOpen.value = false;
  await router.replace("/chat");
};

const openSession = async (id: string) => {
  closeSessionActionMenu();
  if (!id || id === currentSessionId.value) {
    if (id) void markSessionAsRead(id);
    drawerOpen.value = false;
    return;
  }
  clearRecoverPoll();
  stopStream();
  isResumingStream.value = false;
  currentSessionId.value = id;
  currentAssistantMessageId.value = "";
  fullContent.value = "";
  loading.value = false;
  isReplying.value = false;
  drawerOpen.value = false;
  await router.replace(`/chat/${id}`);
  await loadMessages();
  void markSessionAsRead(id);
};

const closeSessionActionMenu = () => {
  sessionActionMenuId.value = "";
};

const toggleSessionActionMenu = (id: string) => {
  sessionActionMenuId.value = sessionActionMenuId.value === id ? "" : id;
};

const updateSessionPinState = (sessionId: string, pin: boolean, pinnedAt?: string | null) => {
  const index = sessions.value.findIndex((item) => item.id === sessionId);
  if (index < 0) return;
  sessions.value[index] = {
    ...sessions.value[index],
    is_pinned: pin,
    pinned_at: pin ? pinnedAt || new Date().toISOString() : null,
  };
};

const toggleSessionPin = async (item: any) => {
  if (!item?.id || sessionPinningIds.value.has(item.id)) return;
  closeSessionActionMenu();
  const pin = !item.is_pinned;
  const previous = { is_pinned: !!item.is_pinned, pinned_at: item.pinned_at || null };
  const nextIds = new Set(sessionPinningIds.value);
  nextIds.add(item.id);
  sessionPinningIds.value = nextIds;
  updateSessionPinState(item.id, pin);

  try {
    const res: any = pin ? await pinSession(item.id) : await unpinSession(item.id);
    if (!res?.success) throw new Error("pin failed");
    updateSessionPinState(item.id, pin, pin ? (res?.data?.pinned_at || new Date().toISOString()) : null);
  } catch (error) {
    updateSessionPinState(item.id, previous.is_pinned, previous.pinned_at);
    MessagePlugin.error(pin ? "置顶失败" : "取消置顶失败");
  } finally {
    const doneIds = new Set(sessionPinningIds.value);
    doneIds.delete(item.id);
    sessionPinningIds.value = doneIds;
  }
};

const removeSessionsLocal = (ids: string[]) => {
  const deleting = new Set(ids);
  sessions.value = sessions.value.filter((item) => !deleting.has(item.id));
};

const deleteMobileSession = async (item: any) => {
  if (!item?.id) return;
  closeSessionActionMenu();
  if (!window.confirm(`确定删除「${item.title || "新对话"}」？`)) return;
  try {
    const res: any = await delSession(item.id);
    if (res?.success === false) throw new Error(res?.message || "删除失败");
    removeSessionsLocal([item.id]);
    MessagePlugin.success("已删除");
    if (item.id === currentSessionId.value) {
      await startNewChat();
    }
  } catch (error: any) {
    MessagePlugin.error(error?.message || "删除失败");
  }
};

const selectAgent = (agent: CustomAgent) => {
  settingsStore.selectAgent(agent.id, null);
  const mode = agent.config?.agent_mode;
  settingsStore.toggleAgent(mode === "smart-reasoning");
  if (agent.config?.web_search_enabled !== undefined) {
    settingsStore.toggleWebSearch(agent.config.web_search_enabled === true);
  }
  if (!agent.is_builtin) {
    if (agent.config?.model_id) {
      settingsStore.updateConversationModels({
        summaryModelId: agent.config.model_id,
        selectedChatModelId: agent.config.model_id,
      });
    }
  }
  closeSheet();
};

const isAgentPinned = (agent: CustomAgent) => agentPins.isPinned(agent.id);

const toggleAgentPin = (agent: CustomAgent) => {
  agentPins.togglePinned(agent.id);
};

const toggleWebSearch = () => {
  if (isWebSearchDisabledByAgent.value) {
    MessagePlugin.warning("当前智能体未启用联网搜索");
    if (settingsStore.isWebSearchEnabled) settingsStore.toggleWebSearch(false);
    return;
  }
  if (!isWebSearchConfigured.value) {
    MessagePlugin.warning("联网搜索未配置");
    if (settingsStore.isWebSearchEnabled) settingsStore.toggleWebSearch(false);
    return;
  }
  settingsStore.toggleWebSearch(!settingsStore.isWebSearchEnabled);
};

const selectModel = (model: ModelConfig) => {
  settingsStore.updateConversationModels({
    summaryModelId: model.id || "",
    selectedChatModelId: model.id || "",
  });
  closeSheet();
};

const selectKnowledgeTab = (tab: KnowledgeSheetTab) => {
  activeKnowledgeTab.value = tab;
  if (tab === "file" && activeFileKbId.value) {
    void loadKnowledgeFilesForKb(activeFileKbId.value);
  }
};

const openFileKnowledgeBase = async (kbId: string) => {
  activeFileKbId.value = kbId;
  await loadKnowledgeFilesForKb(kbId, true);
};

const backToFileKnowledgeBases = () => {
  activeFileKbId.value = "";
};

const selectedFileCountForKb = (kbId: string) => {
  const selected = new Set(selectedFileIds.value);
  return selectedFileIds.value.filter((fileId) => {
    const mappedKbId = settingsStore.settings.selectedFileKbMap?.[fileId];
    if (mappedKbId) return mappedKbId === kbId;
    const file = knowledgeFiles.value.find((item) => item.id === fileId);
    return file?.kb_id === kbId && selected.has(fileId);
  }).length;
};

const fileFolderMeta = (kb: any) => {
  const selectedCount = selectedFileCountForKb(kb.id);
  const total = kb.document_count || kb.knowledge_count || 0;
  return selectedCount > 0 ? `已选 ${selectedCount} 个 · ${total} 个文档` : `${total} 个文档`;
};

const toggleKnowledgeBase = async (kbId: string) => {
  if (selectedKbIds.value.includes(kbId)) settingsStore.removeKnowledgeBase(kbId);
  else settingsStore.addKnowledgeBase(kbId);
  settingsStore.clearFiles();
  settingsStore.clearTags();
  await loadKnowledgeChildren();
};

const toggleKnowledgeFile = (file: any) => {
  if (selectedFileIds.value.includes(file.id)) {
    settingsStore.removeFile(file.id);
  } else {
    settingsStore.addFile(file.id);
    if (file.kb_id) settingsStore.setFileKbMap({ [file.id]: file.kb_id });
  }
};

const toggleSkill = (skill: SkillInfo) => {
  const name = skill.name;
  if (selectedSkillNames.value.includes(name)) {
    settingsStore.removeSkillName(name);
    settingsStore.removeSkill(name);
  } else {
    settingsStore.addSkillName(name);
  }
};

const toggleProfessionalSkill = (skill: SkillInfo) => {
  const name = skill.name;
  if (selectedProfessionalSkillNames.value.includes(name)) settingsStore.removeProfessionalSkillName(name);
  else settingsStore.addProfessionalSkillName(name);
};

const isActiveSkillSelected = (name: string) => {
  return activeSkillTab.value === "professional"
    ? selectedProfessionalSkillNames.value.includes(name)
    : selectedSkillNames.value.includes(name);
};

const toggleActiveSkill = (skill: SkillInfo) => {
  if (activeSkillTab.value === "professional") {
    toggleProfessionalSkill(skill);
    return;
  }
  toggleSkill(skill);
};

const activeSkillPins = computed(() => activeSkillTab.value === "professional" ? professionalPins : lightweightPins);

const isActiveSkillPinned = (name: string) => activeSkillPins.value.isPinned(name);

const toggleActiveSkillPin = (name: string) => {
  activeSkillPins.value.togglePinned(name);
};

const removeChip = (chip: MobileResourceChip) => {
  if (chip.type === "kb") {
    settingsStore.removeKnowledgeBase(chip.id);
    settingsStore.clearFiles();
    settingsStore.clearTags();
    void loadKnowledgeChildren();
    return;
  }
  if (chip.type === "file") settingsStore.removeFile(chip.id);
  if (chip.type === "skill") {
    const separator = chip.id.indexOf(":");
    const kind = separator >= 0 ? chip.id.slice(0, separator) : "skill";
    const name = separator >= 0 ? chip.id.slice(separator + 1) : chip.id;
    if (kind === "professional") settingsStore.removeProfessionalSkillName(name || chip.name);
    else {
      settingsStore.removeSkillName(name || chip.id);
      settingsStore.removeSkill(name || chip.id);
    }
  }
  if (chip.type === "image") {
    const index = Number(chip.id.split(":")[1]);
    pendingImages.value.splice(index, 1);
  }
  if (chip.type === "attachment") {
    const index = Number(chip.id.split(":")[1]);
    pendingAttachments.value.splice(index, 1);
  }
};

const clearSelectedResources = () => {
  const skillNames = [...selectedSkillNames.value];
  settingsStore.clearKnowledgeBases();
  settingsStore.clearFiles();
  settingsStore.clearTags();
  settingsStore.clearSkillNames();
  skillNames.forEach((name) => settingsStore.removeSkill(name));
  settingsStore.clearProfessionalSkillNames();
  pendingImages.value = [];
  pendingAttachments.value = [];
  void loadKnowledgeChildren();
};

const buildMentionedItems = (): MobileMentionItem[] => {
  const mentioned: MobileMentionItem[] = [];
  selectedKbIds.value.forEach((id) => {
    const kb = knowledgeBases.value.find((item) => item.id === id);
    mentioned.push({ id, name: kb?.name || id, type: "kb", kb_type: kb?.type });
  });
  selectedFileIds.value.forEach((id) => {
    const file = knowledgeFiles.value.find((item) => item.id === id);
    mentioned.push({
      id,
      name: file?.display_name || file?.file_name || id,
      type: "file",
      kb_id: file?.kb_id || settingsStore.settings.selectedFileKbMap?.[id],
      kb_name: file?.kb_name,
    });
  });
  selectedSkillNames.value.forEach((name) => {
    mentioned.push({ id: name, name, type: "skill", skill_name: name });
  });
  selectedProfessionalSkillNames.value.forEach((name) => {
    mentioned.push({ id: name, name, type: "skill", skill_name: name });
  });
  return mentioned;
};

const sendMessage = async () => {
  const value = inputValue.value.trim();
  if (!value || isReplying.value) return;
  let outgoingSessionId = "";
  try {
    loading.value = true;
    isReplying.value = true;
    const agentEnabled = settingsStore.isAgentStreamMode;
    const effectiveProfessionalSkillNames = agentEnabled ? selectedProfessionalSkillNames.value : [];

    const imageAttachments = [];
    const userImages = [];
    for (const file of pendingImages.value) {
      const dataUri = await fileToDataUrl(file);
      imageAttachments.push({ data: dataUri });
      userImages.push({ url: dataUri, name: file.name });
    }

    const attachmentUploads = [];
    for (const attachment of pendingAttachments.value) {
      attachmentUploads.push({
        data: await fileToBase64(attachment.file),
        file_name: attachment.name,
        file_size: attachment.size,
      });
    }

    const sessionId = await ensureSession();
    outgoingSessionId = sessionId;
    clearRecoverPoll();
    isResumingStream.value = false;
    markSessionStarted(sessionId);
    upsertStartedSession(sessionId);
    void markSessionAsRead(sessionId);
    prepareForNewOutgoingMessage();

    const professionalPrefix = effectiveProfessionalSkillNames.length
      ? `使用${effectiveProfessionalSkillNames.map((name) => `${name}技能`).join("、")}完成以下工作\n`
      : "";
    const requestQuery = `${professionalPrefix}${value}`;
    const mentionedItems = buildMentionedItems();

    messagesList.push({
      role: "user",
      content: requestQuery,
      mentioned_items: mentionedItems,
      images: userImages,
      attachments: pendingAttachments.value.map((item) => ({
        file_name: item.name,
        file_size: item.size,
        file_type: `.${item.name.split(".").pop()?.toLowerCase() || ""}`,
      })),
      channel: "web",
    });

    inputValue.value = "";
    pendingImages.value = [];
    pendingAttachments.value = [];
    await scrollToBottom(true);
    void loadSessions();

    const kbIdSet = new Set(selectedKbIds.value);
    const fileIdSet = new Set(selectedFileIds.value);

    const endpoint = agentEnabled ? "/api/v1/agent-chat" : "/api/v1/knowledge-chat";
    const modelId = selectedModel.value?.id || selectedModelId.value || "";

    await startStream({
      session_id: sessionId,
      knowledge_base_ids: [...kbIdSet],
      knowledge_ids: [...fileIdSet],
      tag_ids: [],
      agent_enabled: agentEnabled,
      agent_id: selectedAgentId.value,
      web_search_enabled: canUseWebSearch.value,
      summary_model_id: modelId,
      mcp_service_ids: [],
      skill_names: agentEnabled ? selectedSkillNames.value : [],
      professional_skill_names: effectiveProfessionalSkillNames,
      mentioned_items: mentionedItems,
      images: imageAttachments.length ? imageAttachments : undefined,
      attachment_uploads: attachmentUploads.length ? attachmentUploads : undefined,
      query: requestQuery,
      method: "POST",
      url: endpoint,
    });
    void markSessionAsRead(sessionId);
    void loadSessions();
  } catch (err: any) {
    console.error("[mobile] send failed", err);
    MessagePlugin.error(err?.message || "发送失败");
    loading.value = false;
    isReplying.value = false;
  }
};

const stopGenerating = async () => {
  const messageId = currentAssistantMessageId.value;
  stopStream();
  markInFlightAssistantStopped(messageId);
  isReplying.value = false;
  loading.value = false;
  if (currentSessionId.value && messageId) {
    await stopSession(currentSessionId.value, messageId).catch(() => undefined);
  }
  void loadSessions({ silent: true });
};

const handleImageFiles = (event: Event) => {
  const files = Array.from((event.target as HTMLInputElement).files || []);
  pendingImages.value.push(...files.filter((file) => file.type.startsWith("image/")).slice(0, 6));
  (event.target as HTMLInputElement).value = "";
};

const handleAttachmentFiles = (event: Event) => {
  const files = Array.from((event.target as HTMLInputElement).files || []);
  pendingAttachments.value.push(
    ...files.slice(0, 6).map((file) => ({
      file,
      name: file.name,
      size: file.size,
    })),
  );
  (event.target as HTMLInputElement).value = "";
};

const autoGrow = () => {
  const el = textareaRef.value;
  if (!el) return;
  el.style.height = "auto";
  el.style.height = `${Math.min(el.scrollHeight, 116)}px`;
};

watch(inputValue, autoGrow);

watch(hasProfessionalSkillTab, (hasTab) => {
  if (!hasTab && activeSkillTab.value === "professional") {
    activeSkillTab.value = "lightweight";
  }
});

watch(
  () => route.params.sessionId,
  async (id) => {
    const nextId = String(id || "");
    if (nextId && nextId !== currentSessionId.value) {
      clearRecoverPoll();
      stopStream();
      isResumingStream.value = false;
      currentSessionId.value = nextId;
      currentAssistantMessageId.value = "";
      fullContent.value = "";
      loading.value = false;
      isReplying.value = false;
      await loadMessages();
      void markSessionAsRead(nextId);
    } else if (!nextId && currentSessionId.value) {
      clearRecoverPoll();
      stopStream();
      isResumingStream.value = false;
      currentSessionId.value = "";
      messagesList.splice(0);
      shouldFollowAnswer.value = true;
      lastMessageScrollTop = 0;
      currentAssistantMessageId.value = "";
      fullContent.value = "";
      loading.value = false;
      isReplying.value = false;
    }
  },
);

watch(error, (message) => {
  if (!message) return;
  if (isResumingStream.value) {
    const messageId = currentAssistantMessageId.value;
    isResumingStream.value = false;
    if (messageId) {
      const next = new Set(resumeFailedMessageIds.value);
      next.add(messageId);
      resumeFailedMessageIds.value = next;
      pollIncompleteReply(currentSessionId.value, messageId);
    }
    return;
  }
  MessagePlugin.error(message);
  loading.value = false;
  isReplying.value = false;
  void loadSessions({ silent: true });
});

onChunk((data) => {
  if (isResumingStream.value) {
    isResumingStream.value = false;
  }
  if (data.response_type === "session_title") {
    const title = data.content || data.data?.title;
    if (title && data.data?.session_id) {
      const row = sessions.value.find((item) => item.id === data.data.session_id);
      if (row) row.title = title;
    }
    return;
  }
  const finished =
    data.response_type === "complete" ||
    data.response_type === "stop" ||
    data.response_type === "error" ||
    (data.response_type === "answer" && data.done === true);
  activeStreamScrollIntent = inferStreamScrollIntent(data);
  try {
    processStreamChunk(data);
  } finally {
    activeStreamScrollIntent = "";
  }
  if (finished) {
    void markCurrentSessionRead();
    void loadSessions({ silent: true });
  }
});

onMounted(async () => {
  await loadResources();
  await loadSessions();
  if (currentSessionId.value) {
    await loadMessages();
    void markCurrentSessionRead();
  }
});

onBeforeUnmount(() => {
  clearRecoverPoll();
  stopStream();
});
</script>

<template>
  <main class="mobile-chat">
    <header class="chat-topbar">
      <button type="button" class="icon-button" aria-label="会话列表" @click="drawerOpen = true">
        <MobileIcon name="menu" />
      </button>
      <div class="chat-title">
        <span>{{ compactTitle }}</span>
      </div>
      <button type="button" class="icon-button" aria-label="新建会话" @click="startNewChat">
        <MobileIcon name="plus-circle" />
      </button>
    </header>

    <section ref="scrollRef" class="message-scroll" @scroll="handleMessageScroll">
      <div v-if="isLoadingHistory" class="mobile-empty">正在加载会话</div>
      <template v-else>
        <div v-if="messagesList.length === 0" class="mobile-welcome">
          <div class="mobile-welcome__mark">W</div>
          <h1>向 WeKnora 提问</h1>
          <p>可选择智能体、联网、图片、附件、技能、知识库和模型。</p>
        </div>
        <MobileChatMessage
          v-for="(message, index) in messagesList"
          :key="message.id || message.request_id || index"
          :message="message"
        />
      </template>
    </section>

    <footer class="mobile-composer">
      <MobileResourceRail :items="selectedResourceChips" @remove="removeChip" @clear="clearSelectedResources" />

      <div class="config-rail">
        <button type="button" class="config-pill" @click="openSheet('agent')">
          <MobileIcon name="user-talk" />
          <span>{{ agentLabel(selectedAgent) }}</span>
        </button>
        <button
          type="button"
          class="config-pill"
          :class="{ active: canUseWebSearch, disabled: isWebSearchDisabledByAgent || !isWebSearchConfigured }"
          :aria-disabled="isWebSearchDisabledByAgent || !isWebSearchConfigured"
          @click="toggleWebSearch"
        >
          <MobileIcon name="internet" />
          <span>联网</span>
        </button>
        <button type="button" class="config-pill" @click="imageInputRef?.click()">
          <MobileIcon name="image" />
          <span>图片</span>
          <em v-if="pendingImages.length">{{ pendingImages.length }}</em>
        </button>
        <button type="button" class="config-pill" @click="attachmentInputRef?.click()">
          <MobileIcon name="attach" />
          <span>附件</span>
          <em v-if="pendingAttachments.length">{{ pendingAttachments.length }}</em>
        </button>
        <button type="button" class="config-pill" @click="openSheet('skill')">
          <MobileIcon name="lightbulb" />
          <span>技能</span>
          <em v-if="selectedSkillContextCount">{{ selectedSkillContextCount }}</em>
        </button>
        <button type="button" class="config-pill" @click="openSheet('context')">
          <MobileIcon name="folder" />
          <span>知识库</span>
          <em v-if="selectedKnowledgeContextCount">{{ selectedKnowledgeContextCount }}</em>
        </button>
        <button type="button" class="config-pill" @click="openSheet('model')">
          <MobileIcon name="cpu" />
          <span>{{ modelLabel(selectedModel) }}</span>
        </button>
      </div>

      <div class="input-row">
        <textarea
          ref="textareaRef"
          v-model="inputValue"
          rows="1"
          placeholder="向 WeKnora 提问..."
          @keydown.enter.exact.prevent="sendMessage"
        />
        <button v-if="isReplying" type="button" class="send-button stop" aria-label="停止" @click="stopGenerating">
          <MobileIcon name="stop-circle" />
        </button>
        <button v-else type="button" class="send-button" :disabled="!inputValue.trim()" aria-label="发送" @click="sendMessage">
          <MobileIcon name="send" />
        </button>
      </div>

      <input ref="imageInputRef" type="file" accept="image/*" multiple hidden @change="handleImageFiles" />
      <input ref="attachmentInputRef" type="file" multiple hidden @change="handleAttachmentFiles" />
    </footer>

    <div v-if="drawerOpen" class="drawer-layer" @click.self="drawerOpen = false">
      <aside class="session-drawer">
        <div class="drawer-scroll" @click="closeSessionActionMenu">
          <div v-if="sessionsLoading" class="drawer-empty">正在加载</div>
          <div v-else-if="!sessionGroups.length" class="drawer-empty">暂无会话</div>
          <template v-else>
            <section v-for="group in sessionGroups" :key="group.key" class="session-group">
              <h2>{{ group.label }}</h2>
              <div
                v-for="item in group.items"
                :key="item.id"
                class="session-row"
                :class="{ active: item.id === currentSessionId, unread: isSessionUnread(item) }"
                role="button"
                tabindex="0"
                @click="openSession(item.id)"
                @keydown.enter.prevent="openSession(item.id)"
              >
                <span class="session-row__title">{{ item.title || "新对话" }}</span>
                <i v-if="isSessionUnread(item)" class="session-unread-dot" aria-label="有新回复"></i>
                <button
                  type="button"
                  class="session-more"
                  :disabled="sessionPinningIds.has(item.id)"
                  aria-label="会话操作"
                  @click.stop="toggleSessionActionMenu(item.id)"
                >
                  <MobileIcon name="ellipsis" />
                </button>
                <div v-if="sessionActionMenuId === item.id" class="session-action-menu" @click.stop>
                  <button type="button" @click="toggleSessionPin(item)">
                    <MobileIcon :name="item.is_pinned ? 'pin-filled' : 'pin'" />
                    <span>{{ item.is_pinned ? "取消置顶" : "置顶" }}</span>
                  </button>
                  <button type="button" class="danger" @click="deleteMobileSession(item)">
                    <MobileIcon name="delete" />
                    <span>删除对话</span>
                  </button>
                </div>
              </div>
            </section>
          </template>
        </div>
        <div class="drawer-footer">
          <div class="drawer-user" :aria-label="userDisplayName">
            <img v-if="authStore.user?.avatar" :src="authStore.user.avatar" alt="" />
            <span v-else class="drawer-avatar">{{ userAvatarText }}</span>
            <strong>{{ userDisplayName }}</strong>
          </div>
          <button type="button" class="drawer-settings" aria-label="设置" @click="openSettings">
            <MobileIcon name="setting" />
          </button>
        </div>
      </aside>
    </div>

    <div v-if="sheetOpen" class="sheet-layer" @click.self="closeSheet">
      <section class="config-sheet">
        <div class="sheet-grip" />
        <div class="sheet-head">
          <strong>
            {{
              activeSheet === 'agent' ? '选择智能体' :
              activeSheet === 'model' ? '选择模型' :
              activeSheet === 'skill' ? '选择技能' : '选择知识库'
            }}
          </strong>
          <button type="button" class="text-button" @click="closeSheet">完成</button>
        </div>

        <div v-if="activeSheet === 'context'" class="sheet-tabs" role="tablist">
          <button
            type="button"
            class="sheet-tab"
            :class="{ active: activeKnowledgeTab === 'kb' }"
            role="tab"
            :aria-selected="activeKnowledgeTab === 'kb'"
            @click="selectKnowledgeTab('kb')"
          >
            知识库
          </button>
          <button
            type="button"
            class="sheet-tab"
            :class="{ active: activeKnowledgeTab === 'file' }"
            role="tab"
            :aria-selected="activeKnowledgeTab === 'file'"
            @click="selectKnowledgeTab('file')"
          >
            文件
          </button>
        </div>

        <div v-if="activeSheet === 'skill' && hasProfessionalSkillTab" class="sheet-tabs" role="tablist">
          <button
            type="button"
            class="sheet-tab"
            :class="{ active: activeSkillTab === 'professional' }"
            role="tab"
            :aria-selected="activeSkillTab === 'professional'"
            @click="activeSkillTab = 'professional'"
          >
            专业技能
          </button>
          <button
            type="button"
            class="sheet-tab"
            :class="{ active: activeSkillTab === 'lightweight' }"
            role="tab"
            :aria-selected="activeSkillTab === 'lightweight'"
            @click="activeSkillTab = 'lightweight'"
          >
            轻量技能
          </button>
        </div>

        <div v-if="resourcesLoading" class="sheet-empty">正在加载配置</div>

        <div v-else class="sheet-list">
          <div
            v-for="agent in activeSheet === 'agent' ? mobileAgentRows : []"
            :key="agent.id"
            class="sheet-row sheet-row--with-action"
            :class="{ selected: selectedAgentId === agent.id || ([BUILTIN_QUICK_ANSWER_ID, BUILTIN_SIMPLE_CHAT_ID] as string[]).includes(selectedAgentId) && agent.id === BUILTIN_QUICK_ANSWER_ID }"
            role="button"
            tabindex="0"
            @click="selectAgent(agent)"
            @keydown.enter.prevent="selectAgent(agent)"
          >
            <div class="sheet-row__main">
              <span>{{ agent.name }}</span>
              <small class="agent-desc">{{ agent.description || (agent.is_builtin ? '内置智能体' : '自定义智能体') }}</small>
            </div>
            <button
              type="button"
              class="skill-pin"
              :class="{ pinned: isAgentPinned(agent) }"
              :aria-label="isAgentPinned(agent) ? '取消置顶' : '置顶'"
              @click.stop="toggleAgentPin(agent)"
            >
              <MobileIcon :name="isAgentPinned(agent) ? 'pin-filled' : 'pin'" />
            </button>
          </div>

          <button
            v-for="model in activeSheet === 'model' ? chatModels : []"
            :key="model.id"
            type="button"
            class="sheet-row"
            :class="{ selected: selectedModelId === model.id }"
            @click="selectModel(model)"
          >
            <span>{{ model.display_name || model.name }}</span>
            <small>{{ mobileModelTypeLabel(model.type) }}</small>
          </button>

          <button
            v-for="kb in activeSheet === 'context' && activeKnowledgeTab === 'kb' ? knowledgeBases : []"
            :key="kb.id"
            type="button"
            class="sheet-row"
            :class="{ selected: selectedKbIds.includes(kb.id) }"
            @click="toggleKnowledgeBase(kb.id)"
          >
            <span>{{ kb.name }}</span>
            <small>{{ kb.document_count || kb.knowledge_count || 0 }} 个文档</small>
          </button>

          <button
            v-for="kb in activeSheet === 'context' && activeKnowledgeTab === 'file' && !activeFileKbId ? knowledgeBases : []"
            :key="`file-folder:${kb.id}`"
            type="button"
            class="sheet-row sheet-row--with-action sheet-row--folder"
            @click="openFileKnowledgeBase(kb.id)"
          >
            <div class="sheet-row__main">
              <span class="sheet-row__title-with-icon">
                <MobileIcon name="folder" />
                {{ kb.name }}
              </span>
              <small>{{ fileFolderMeta(kb) }}</small>
            </div>
            <MobileIcon name="chevron-right" />
          </button>

          <div
            v-if="activeSheet === 'context' && activeKnowledgeTab === 'file' && activeFileKbId"
            class="file-folder-header"
          >
            <button type="button" @click="backToFileKnowledgeBases">
              <MobileIcon name="chevron-left" />
              <span>{{ activeFileKnowledgeBase?.name || '返回知识库' }}</span>
            </button>
            <small>选择要引用的文件</small>
          </div>

          <div
            v-if="activeSheet === 'context' && activeKnowledgeTab === 'file' && activeFileKbId && fileListLoading"
            class="sheet-empty"
          >
            正在加载文件
          </div>

          <button
            v-for="file in activeSheet === 'context' && activeKnowledgeTab === 'file' && activeFileKbId && !fileListLoading ? activeFileRows : []"
            :key="file.id"
            type="button"
            class="sheet-row"
            :class="{ selected: selectedFileIds.includes(file.id) }"
            @click="toggleKnowledgeFile(file)"
          >
            <span>{{ file.display_name || file.file_name }}</span>
            <small>{{ file.kb_name || '知识库文件' }}</small>
          </button>

          <div
            v-for="skill in activeSheet === 'skill' ? activeSkillRows : []"
            :key="skill.name"
            class="sheet-row sheet-row--with-action"
            :class="{ selected: isActiveSkillSelected(skill.name) }"
            role="button"
            tabindex="0"
            @click="toggleActiveSkill(skill)"
            @keydown.enter.prevent="toggleActiveSkill(skill)"
          >
            <div class="sheet-row__main">
              <span>{{ skillLabel(skill) }}</span>
              <small>{{ skill.description || (activeSkillTab === 'professional' ? '专业技能' : '轻量技能') }}</small>
            </div>
            <button
              type="button"
              class="skill-pin"
              :class="{ pinned: isActiveSkillPinned(skill.name) }"
              :aria-label="isActiveSkillPinned(skill.name) ? '取消置顶' : '置顶'"
              @click.stop="toggleActiveSkillPin(skill.name)"
            >
              <MobileIcon :name="isActiveSkillPinned(skill.name) ? 'pin-filled' : 'pin'" />
            </button>
          </div>

          <div
            v-if="
              (activeSheet === 'model' && !chatModels.length) ||
              (activeSheet === 'context' && activeKnowledgeTab === 'kb' && !knowledgeBases.length) ||
              (activeSheet === 'context' && activeKnowledgeTab === 'file' && !activeFileKbId && !knowledgeBases.length) ||
              (activeSheet === 'context' && activeKnowledgeTab === 'file' && activeFileKbId && !fileListLoading && !activeFileRows.length) ||
              (activeSheet === 'skill' && (!skillsAvailable || !activeSkillRows.length))
            "
            class="sheet-empty"
          >
            暂无可选项
          </div>
        </div>
      </section>
    </div>
  </main>
</template>

<style scoped>
.mobile-chat {
  display: grid;
  width: 100%;
  min-width: 0;
  max-width: 100vw;
  height: 100dvh;
  grid-template-rows: auto 1fr auto;
  overflow: hidden;
  background: #f5f7f8;
}

.chat-topbar {
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

.icon-button {
  display: grid;
  width: 40px;
  height: 40px;
  place-items: center;
  border: 0;
  border-radius: 50%;
  background: transparent;
  color: #24372f;
  padding: 0;
  font-size: 23px;
}

.chat-title {
  display: flex;
  min-width: 0;
  align-self: stretch;
  align-items: center;
  justify-content: center;
  line-height: 1.25;
}

.chat-title span {
  overflow: hidden;
  max-width: 100%;
  font-size: 16px;
  font-weight: 650;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.message-scroll {
  display: flex;
  width: 100%;
  min-height: 0;
  min-width: 0;
  flex-direction: column;
  gap: 18px;
  overflow-y: auto;
  overflow-x: hidden;
  padding: 16px 12px 12px;
  scrollbar-width: none;
  -webkit-overflow-scrolling: touch;
}

.message-scroll::-webkit-scrollbar {
  display: none;
}

.mobile-empty,
.drawer-empty,
.sheet-empty {
  padding: 22px 12px;
  color: #7a8b83;
  font-size: 14px;
  text-align: center;
}

.mobile-welcome {
  display: flex;
  min-height: 58%;
  align-items: center;
  justify-content: center;
  flex-direction: column;
  gap: 8px;
  color: #4f6259;
  text-align: center;
}

.mobile-welcome__mark {
  display: grid;
  width: 48px;
  height: 48px;
  place-items: center;
  border-radius: 14px;
  background: #07c160;
  color: #fff;
  font-size: 26px;
  font-weight: 700;
}

.mobile-welcome h1 {
  margin: 4px 0 0;
  color: #15211d;
  font-size: 20px;
}

.mobile-welcome p {
  max-width: 280px;
  margin: 0;
  font-size: 14px;
  line-height: 1.6;
}

.mobile-composer {
  display: flex;
  width: 100%;
  min-width: 0;
  flex-direction: column;
  gap: 7px;
  border-top: 1px solid #dfe8e3;
  background: rgba(255, 255, 255, 0.98);
  padding: 8px 10px calc(env(safe-area-inset-bottom) + 9px);
}

.config-rail {
  display: flex;
  gap: 7px;
  overflow-x: auto;
  padding-bottom: 2px;
  scrollbar-width: none;
  -webkit-overflow-scrolling: touch;
}

.config-rail::-webkit-scrollbar {
  display: none;
}

.config-pill {
  display: inline-flex;
  align-items: center;
  flex: 0 0 auto;
  gap: 5px;
  height: 34px;
  border: 1px solid #dbe7e1;
  border-radius: 17px;
  background: #f8fbf9;
  color: #263a31;
  padding: 0 11px;
  font-size: 14px;
  white-space: nowrap;
}

.config-pill span {
  overflow: hidden;
  max-width: 136px;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.config-pill.active {
  border-color: #92ddb3;
  background: #edf9f2;
  color: #078f49;
}

.config-pill.disabled {
  color: #9aaaa2;
  background: #f3f6f4;
  border-color: #e3ebe6;
}

.config-pill em {
  display: grid;
  min-width: 17px;
  height: 17px;
  place-items: center;
  border-radius: 9px;
  background: #07c160;
  color: #fff;
  font-size: 10px;
  font-style: normal;
}

.input-row {
  display: grid;
  grid-template-columns: minmax(0, 1fr) 40px;
  align-items: end;
  gap: 8px;
  border: 1px solid #dce8e2;
  border-radius: 18px;
  background: #fff;
  padding: 8px 8px 8px 12px;
}

.input-row textarea {
  width: 100%;
  max-height: 116px;
  min-height: 28px;
  resize: none;
  border: 0;
  outline: none;
  color: #17261f;
  font-size: 16px;
  line-height: 1.5;
}

.send-button {
  display: grid;
  width: 34px;
  height: 34px;
  place-items: center;
  border: 0;
  border-radius: 50%;
  background: #07c160;
  color: #fff;
  padding: 0;
}

.send-button:disabled {
  background: #c8d6cf;
}

.send-button.stop {
  background: #2e3c36;
}

.drawer-layer,
.sheet-layer {
  position: fixed;
  z-index: 30;
  inset: 0;
  background: rgba(10, 22, 16, 0.34);
}

.session-drawer {
  display: flex;
  width: min(82vw, 330px);
  height: 100%;
  overflow: hidden;
  flex-direction: column;
  background: #fff;
  padding: calc(env(safe-area-inset-top) + 18px) 10px calc(env(safe-area-inset-bottom) + 12px);
}

.drawer-scroll {
  min-height: 0;
  flex: 1;
  overflow-y: auto;
  padding: 0 2px 14px;
  scrollbar-width: none;
  -webkit-overflow-scrolling: touch;
}

.drawer-scroll::-webkit-scrollbar {
  display: none;
}

.sheet-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  padding: 0 2px 10px;
}

.sheet-head strong {
  font-size: 17px;
}

.text-button {
  border: 0;
  background: transparent;
  color: #07a557;
  font-size: 15px;
  font-weight: 650;
  padding: 6px;
}

.sheet-tabs {
  display: flex;
  gap: 4px;
  padding: 0 0 8px;
  border-bottom: 1px solid #edf2ef;
  margin-bottom: 4px;
}

.sheet-tab {
  flex: 1;
  height: 32px;
  border: 0;
  border-bottom: 2px solid transparent;
  background: transparent;
  color: #6f8179;
  font-size: 14px;
  font-weight: 650;
  padding: 0 8px;
}

.sheet-tab.active {
  border-bottom-color: #07c160;
  color: #047c42;
}

.sheet-row {
  display: flex;
  width: 100%;
  min-height: 60px;
  align-items: flex-start;
  justify-content: center;
  flex-direction: column;
  gap: 5px;
  border: 0;
  border-radius: 8px;
  background: transparent;
  color: #1d2e26;
  font: inherit;
  padding: 10px 12px;
  text-align: left;
}

.session-group + .session-group {
  margin-top: 20px;
}

.session-group h2 {
  margin: 0 10px 8px;
  color: #8a9992;
  font-size: 16px;
  font-weight: 400;
  line-height: 1.4;
}

.session-row {
  position: relative;
  display: flex;
  width: 100%;
  min-height: 48px;
  align-items: center;
  gap: 8px;
  border: 0;
  border-radius: 14px;
  background: transparent;
  color: #1d2e26;
  font: inherit;
  padding: 0 14px;
  text-align: left;
  cursor: pointer;
}

.sheet-row {
  cursor: pointer;
}

.sheet-row--with-action {
  display: grid;
  grid-template-columns: minmax(0, 1fr) 34px;
  align-items: center;
  justify-content: stretch;
  flex-direction: initial;
}

.sheet-row--folder {
  min-height: 62px;
}

.sheet-row__main {
  display: flex;
  min-width: 0;
  flex-direction: column;
  gap: 4px;
}

.sheet-row__title-with-icon {
  display: inline-flex;
  max-width: 100%;
  align-items: center;
  gap: 8px;
}

.sheet-row__title-with-icon .mobile-icon {
  color: #07a557;
  font-size: 17px;
}

.file-folder-header {
  display: flex;
  flex-direction: column;
  gap: 4px;
  border-bottom: 1px solid #edf2ef;
  margin: -2px 0 4px;
  padding: 2px 4px 10px;
}

.file-folder-header button {
  display: inline-flex;
  width: fit-content;
  max-width: 100%;
  align-items: center;
  gap: 6px;
  border: 0;
  background: transparent;
  color: #13261e;
  font: inherit;
  font-size: 15px;
  font-weight: 650;
  padding: 6px 0;
}

.file-folder-header button span {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.file-folder-header small {
  color: #7f9088;
  font-size: 12px;
  line-height: 1.35;
  padding-left: 24px;
}

.sheet-row.selected {
  background: #edf9f2;
  color: #057c41;
}

.session-row.active {
  background: #edf9f2;
  color: #057c41;
}

.session-row__title {
  overflow: hidden;
  min-width: 0;
  flex: 1;
  font-size: 17px;
  font-weight: 560;
  line-height: 1.35;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.session-unread-dot {
  flex: 0 0 auto;
  margin-left: 2px;
}

.session-unread-dot {
  width: 8px;
  height: 8px;
  border-radius: 999px;
  background: #07c160;
  box-shadow: 0 0 0 3px rgba(7, 193, 96, 0.12);
}

.session-more {
  display: grid;
  width: 30px;
  height: 30px;
  flex: 0 0 auto;
  place-items: center;
  border: 0;
  border-radius: 8px;
  background: transparent;
  color: #8a9b93;
  padding: 0;
}

.session-more .mobile-icon {
  font-size: 19px;
}

.session-more:disabled {
  opacity: 0.55;
}

.session-action-menu {
  position: absolute;
  right: 8px;
  top: calc(100% - 2px);
  z-index: 20;
  display: grid;
  min-width: 138px;
  border: 1px solid #dfe8e3;
  border-radius: 10px;
  background: #fff;
  box-shadow: 0 10px 28px rgba(17, 41, 28, 0.14);
  padding: 5px;
}

.session-action-menu button {
  display: flex;
  min-height: 38px;
  align-items: center;
  gap: 8px;
  border: 0;
  border-radius: 8px;
  background: transparent;
  color: #24372f;
  font: inherit;
  font-size: 14px;
  padding: 0 10px;
  text-align: left;
}

.session-action-menu button:active {
  background: #edf9f2;
}

.session-action-menu button.danger {
  color: #c93535;
}

.sheet-row span {
  overflow: hidden;
  width: 100%;
  font-size: 15px;
  font-weight: 650;
  line-height: 1.35;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.sheet-row small {
  display: block;
  overflow: hidden;
  width: 100%;
  color: #788982;
  font-size: 13px;
  line-height: 1.4;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.drawer-footer {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  border-top: 1px solid #edf2ef;
  padding: 12px 4px 0;
}

.drawer-user {
  display: flex;
  min-width: 0;
  align-items: center;
  gap: 10px;
}

.drawer-user img,
.drawer-avatar {
  display: grid;
  width: 34px;
  height: 34px;
  flex: 0 0 auto;
  place-items: center;
  border-radius: 50%;
}

.drawer-user img {
  object-fit: cover;
}

.drawer-avatar {
  background: #07c160;
  color: #fff;
  font-size: 16px;
  font-weight: 700;
}

.drawer-user strong {
  overflow: hidden;
  color: #45584f;
  font-size: 15px;
  font-weight: 500;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.drawer-settings {
  display: grid;
  width: 40px;
  height: 40px;
  flex: 0 0 auto;
  place-items: center;
  border: 0;
  border-radius: 50%;
  background: transparent;
  color: #24372f;
  font-size: 22px;
  padding: 0;
}

.skill-pin {
  display: grid;
  width: 30px;
  height: 30px;
  place-items: center;
  justify-self: end;
  border: 0;
  border-radius: 8px;
  background: transparent;
  color: #8a9b93;
  padding: 0;
}

.skill-pin.pinned {
  background: #e9f7ef;
  color: #07a557;
}

.config-sheet {
  position: absolute;
  right: 0;
  bottom: 0;
  left: 0;
  display: flex;
  max-height: min(72dvh, 640px);
  flex-direction: column;
  border-radius: 18px 18px 0 0;
  background: #fff;
  padding: 8px 12px calc(env(safe-area-inset-bottom) + 14px);
}

.sheet-grip {
  width: 38px;
  height: 4px;
  align-self: center;
  border-radius: 4px;
  background: #d5ded9;
  margin-bottom: 10px;
}

.sheet-list {
  display: flex;
  min-height: 0;
  flex-direction: column;
  gap: 6px;
  overflow-y: auto;
}
</style>
