<script setup lang="ts">
import { computed, onBeforeUnmount, ref, shallowRef, watch } from "vue";
import { get } from "@/utils/request";
import ContentPopup from "@/views/chat/components/tool-results/ContentPopup.vue";
import {
  processItems,
  visibleProcess,
  safeSourceURL,
  type ProcessItem,
  type ProcessSource,
} from "./processPresentation";
const props = defineProps<{
  message: Record<string, any>;
  history?: boolean;
  sessionId?: string;
  embedChannelId?: string;
  embedToken?: string;
  embedSessionSig?: string;
}>();
const items = shallowRef<ProcessItem[]>([]),
  expanded = ref(""),
  visibleCount = ref(10);
const sources = shallowRef<ProcessSource[]>([]),
  total = ref(0),
  loading = ref(false),
  loadError = ref(""),
  detailSource = ref("");
let generation = 0,
  timer: ReturnType<typeof setInterval> | undefined;
function refresh() {
  items.value = processItems(
    props.message.agentEventStream || [],
    props.message.is_completed === true,
  );
}
watch(
  () => [
    props.message.id,
    props.message.is_completed,
    props.history,
    props.message.agentEventStream,
  ],
  () => {
    if (timer) clearInterval(timer);
    refresh();
    if (!props.message.is_completed) timer = setInterval(refresh, 250);
  },
  { immediate: true },
);
onBeforeUnmount(() => {
  if (timer) clearInterval(timer);
  generation++;
});
const projection = computed(() => visibleProcess(items.value, expanded.value));
const showHistory = computed(() => props.history);
const allRows = computed(() =>
  items.value.filter((i) => i.category !== "status"),
);
const rows = computed(() =>
  showHistory.value
    ? allRows.value.slice(0, visibleCount.value)
    : projection.value.actions,
);
async function fetchSources(item: ProcessItem, append = false) {
  const current = ++generation;
  loading.value = true;
  loadError.value = "";
  try {
    const message = encodeURIComponent(String(props.message.id || "")),
      call = encodeURIComponent(String(item.event.tool_call_id || ""));
    const url = props.embedChannelId
      ? `/api/v1/embed/${encodeURIComponent(props.embedChannelId)}/sessions/${encodeURIComponent(props.sessionId || "")}/messages/${message}/process/${call}/sources`
      : `/api/v1/custom/agent-runtime/messages/${message}/process/${call}/sources`;
    const result = await get<{ sources: ProcessSource[]; total: number }>(url, {
      params: { offset: append ? sources.value.length : 0 },
      headers: props.embedToken
        ? {
            Authorization: `Embed ${props.embedToken}`,
            "X-Embed-Session": props.embedSessionSig || "",
          }
        : undefined,
    });
    if (current !== generation) return;
    sources.value = append
      ? [...sources.value, ...result.sources]
      : result.sources;
    total.value = result.total;
  } catch {
    if (current === generation) loadError.value = "资料暂时无法加载，请重试";
  } finally {
    if (current === generation) loading.value = false;
  }
}
function toggle(item: ProcessItem) {
  generation++;
  if (expanded.value === item.key) {
    expanded.value = "";
    return;
  }
  expanded.value = item.key;
  detailSource.value = "";
  sources.value = [];
  total.value = item.sourceCount || 0;
  if (item.sourceCount || item.sources.length) void fetchSources(item);
}
function canExpand(item: ProcessItem) {
  return (
    (item.sourceCount || 0) > 0 ||
    item.sources.length > 0 ||
    item.category === "thought"
  );
}
function chunks(source: ProcessSource) {
  return source.snippets.map((s) => ({
    content: s.content,
    chunk_id: s.id,
    knowledge_id: source.knowledge_id,
  }));
}
</script>

<template>
  <section
    class="process-preview"
    :class="{ 'is-history': showHistory }"
    aria-label="智能体执行过程"
  >
    <div v-if="!history" class="process-heading">
      <span class="process-pulse" aria-hidden="true" /><span>{{
        message.is_completed ? "处理结束" : projection.status
      }}</span>
    </div>
    <div v-if="!showHistory && projection.thoughts.length" class="process-thoughts">
      <div
        v-for="item in projection.thoughts"
        :key="item.key"
        class="process-thought"
        :title="item.text"
      >
        {{ item.text }}
      </div>
    </div>
    <div
      class="process-actions"
      :class="{ 'process-history-scroll': showHistory }"
    >
      <article
        v-for="item in rows"
        :key="item.key"
        class="process-action"
        :data-process-category="item.category"
      >
        <button
          class="process-action-title"
          :disabled="!canExpand(item)"
          :aria-expanded="canExpand(item) ? expanded === item.key : undefined"
          @click="toggle(item)"
        >
          <span
            class="process-state"
            :class="`is-${item.state}`"
            aria-hidden="true"
            >{{
              item.state === "running"
                ? "◌"
                : item.state === "success"
                  ? "✓"
                  : item.state === "error"
                    ? "!"
                    : "−"
            }}</span
          ><span class="process-text" :title="item.text">{{ item.text }}</span
          ><span v-if="canExpand(item)">{{
            expanded === item.key ? "⌄" : "›"
          }}</span>
        </button>
        <button
          v-if="item.sources.length && expanded !== item.key"
          class="process-source-preview"
          @click="toggle(item)"
        >
          {{ item.sources.map((s) => s.title).join(" · ")
          }}<span
            v-if="
              item.sourceCount !== null &&
              item.sourceCount > item.sources.length
            "
          >
            +{{ item.sourceCount - item.sources.length }}</span
          >
        </button>
        <div v-if="expanded === item.key" class="process-detail">
          <ContentPopup
            v-if="item.category === 'thought'"
            :content="
              String(
                item.event.content ||
                  item.event.tool_data?.thought ||
                  item.event.tool_data?.task ||
                  '',
              )
            "
          />
          <template v-else>
            <div
              v-for="source in sources"
              :key="source.id"
              class="process-source"
            >
              <button
                class="process-source-name"
                :aria-expanded="detailSource === source.id"
                @click="
                  detailSource = detailSource === source.id ? '' : source.id
                "
              >
                {{ source.title }}
                {{ detailSource === source.id ? "⌄" : "›" }}</button
              ><span v-if="source.knowledge_base" class="process-source-kb">{{
                source.knowledge_base
              }}</span>
              <a
                v-if="safeSourceURL(source.url)"
                class="process-url"
                :href="safeSourceURL(source.url)"
                target="_blank"
                rel="noopener noreferrer"
                >{{ source.url }}</a
              >
              <template v-if="detailSource === source.id"
                ><ContentPopup
                  v-if="source.snippets?.length"
                  :chunks="chunks(source)"
                  :knowledge-id="source.knowledge_id"
                />
                <div v-else class="process-muted">
                  本次结果未返回片段
                </div></template
              >
            </div>
            <div v-if="loading" class="process-muted" role="status">
              正在加载资料…
            </div>
            <button
              v-else-if="loadError"
              class="process-link"
              @click="fetchSources(item)"
            >
              {{ loadError }}
            </button>
            <button
              v-else-if="sources.length < total"
              class="process-link"
              @click="fetchSources(item, true)"
            >
              查看更多（{{ sources.length }}/{{ total }}）
            </button>
          </template>
        </div>
      </article>
      <button
        v-if="showHistory && allRows.length > visibleCount"
        class="process-link"
        @click="visibleCount += 10"
      >
        查看更多步骤
      </button>
    </div>
    <div
      v-if="!showHistory && expanded && projection.hidden"
      class="process-muted"
    >
      其他进度持续更新 ·
      <button class="process-link" @click="expanded = ''">返回最新进度</button>
    </div>
  </section>
</template>

<style scoped>
.process-preview {
  width: 100%;
  max-width: 680px;
  box-sizing: border-box;
  color: var(--td-text-color-secondary, #666);
  font-size: 13px;
  line-height: 1.6;
  padding: 8px 0;
  overflow-anchor: none;
}
.process-heading {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-bottom: 8px;
  font-weight: 500;
  color: var(--td-text-color-primary, #333);
}
.process-heading .process-link {
  margin-left: auto;
  font-weight: 400;
}
.process-pulse {
  width: 6px;
  height: 6px;
  border-radius: 50%;
  background: var(--td-brand-color, #07a66c);
  animation: processPulse 1.6s ease-in-out infinite;
}
.process-thoughts {
  display: grid;
  gap: 5px;
  margin-bottom: 8px;
  max-height: 140px;
  overflow: hidden;
}
.process-thought {
  border-left: 2px solid var(--td-component-border, #ddd);
  padding-left: 10px;
  display: -webkit-box;
  -webkit-line-clamp: 2;
  -webkit-box-orient: vertical;
  overflow: hidden;
  overflow-wrap: anywhere;
}
.process-action {
  padding: 5px 0;
  min-width: 0;
}
.process-action-title {
  width: 100%;
  border: 0;
  background: none;
  color: inherit;
  display: flex;
  align-items: flex-start;
  text-align: left;
  gap: 8px;
  padding: 0;
  cursor: pointer;
  font: inherit;
}
.process-action-title:disabled {
  cursor: default;
  opacity: 1;
}
.process-text {
  display: -webkit-box;
  -webkit-line-clamp: 2;
  -webkit-box-orient: vertical;
  overflow: hidden;
  flex: 1;
  overflow-wrap: anywhere;
}
.process-state {
  flex: none;
  color: var(--td-success-color, #00a870);
}
.process-state.is-running {
  animation: processPulse 1.6s ease-in-out infinite;
}
.process-state.is-error {
  color: var(--td-error-color, #d54941);
}
.process-state.is-stopped {
  color: inherit;
}
.process-source-preview {
  margin-left: 22px;
  max-width: calc(100% - 22px);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  border: 0;
  background: none;
  padding: 0;
  color: var(--td-brand-color, #00875a);
  font: inherit;
  cursor: pointer;
  text-align: left;
}
.process-detail {
  max-height: 260px;
  overflow: auto;
  overscroll-behavior: contain;
  margin: 7px 0 4px 20px;
  padding: 8px 10px;
  background: var(--td-bg-color-secondarycontainer, #f7f8fa);
  border-radius: 6px;
}
.process-source {
  padding: 6px 0;
  border-bottom: 1px solid var(--td-component-border, #eee);
}
.process-source-name {
  border: 0;
  background: none;
  padding: 0;
  color: var(--td-text-color-primary, #333);
  font: inherit;
  text-align: left;
  cursor: pointer;
  overflow-wrap: anywhere;
}
.process-source-kb {
  font-size: 11px;
  margin-left: 8px;
  color: var(--td-text-color-placeholder, #888);
}
.process-url {
  display: block;
  overflow-wrap: anywhere;
  color: var(--td-brand-color, #00875a);
  font-size: 12px;
}
.process-link {
  border: 0;
  background: none;
  padding: 0;
  color: var(--td-brand-color, #00875a);
  cursor: pointer;
  font: inherit;
}
.process-muted {
  font-size: 12px;
  padding: 4px 0;
  color: var(--td-text-color-placeholder, #888);
}
.process-history-scroll {
  max-height: 420px;
  overflow: auto;
  overscroll-behavior: contain;
}
.process-detail :deep(.popup-content) {
  max-width: 100%;
  box-shadow: none;
  border: 0;
  background: transparent;
}
.process-detail :deep(.popup-footer) {
  display: none;
}
button:focus-visible,
a:focus-visible {
  outline: 2px solid var(--td-brand-color, #00875a);
  outline-offset: 2px;
}
@keyframes processPulse {
  50% {
    opacity: 0.35;
  }
}
@media (prefers-reduced-motion: reduce) {
  .process-pulse,
  .process-state.is-running {
    animation: none;
  }
}
</style>
