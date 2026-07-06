<script setup lang="ts">
import { computed, onBeforeUnmount, ref } from "vue";
import { MessagePlugin } from "tdesign-vue-next";
import { getDown } from "@/utils/request";
import { unwrapFinalAnswerWrappers } from "@/utils/finalAnswer";
import { splitStructuredChartMarkdown, type StructuredChartInfo } from "@/utils/structuredChartMarkdown";
import MobileResourceRail from "./MobileResourceRail.vue";
import MobileCitationSheet from "./MobileCitationSheet.vue";
import MobileSourceDetailSheet from "./MobileSourceDetailSheet.vue";
import MobileStructuredAnalysisResult from "./MobileStructuredAnalysisResult.vue";
import { renderMobileMarkdown } from "../mobileMarkdown";
import type { MobileResourceChip } from "../utils";
import { downloadBlob, formatFileSize } from "../utils";
import type { GeneralAgentArtifactFile, GeneralAgentArtifactsData, StructuredAnalysisData } from "@/types/tool-results";
import {
  buildCitedSourceReferenceItems,
  sourceTypeLabel,
  type SourceReferenceItem,
  type SourceReferenceKind,
} from "@/utils/sourceReferences";

type ChatMessage = Record<string, any>;

const props = defineProps<{
  message: ChatMessage;
}>();

type StructuredAnalysisBlock = {
  display_type: "structured_analysis_result";
  tool_data: StructuredAnalysisData;
  output?: string;
  arguments?: Record<string, any>;
};

type MobileAnswerSegment =
  | { key: string; kind: "markdown"; html: string }
  | { key: string; kind: "structured_result"; result: StructuredAnalysisBlock };

const isUser = computed(() => props.message.role === "user");
const isAssistant = computed(() => props.message.role === "assistant");
const downloadingArtifactId = ref("");

const mentionedChips = computed<MobileResourceChip[]>(() => {
  const items = Array.isArray(props.message.mentioned_items) ? props.message.mentioned_items : [];
  return items.map((item: any) => ({
    id: item.id || item.name,
    type:
      item.type === "kb"
        ? "kb"
        : item.type === "tag"
          ? "tag"
          : item.type === "mcp"
            ? "mcp"
            : item.type === "skill"
              ? "skill"
              : "file",
    name: item.name || item.id || "资源",
    removable: false,
  }));
});

const uploadChips = computed<MobileResourceChip[]>(() => {
  const chips: MobileResourceChip[] = [];
  const images = Array.isArray(props.message.images) ? props.message.images : [];
  const attachments = Array.isArray(props.message.attachments) ? props.message.attachments : [];
  images.forEach((img: any, index: number) => {
    chips.push({
      id: `image:${index}`,
      type: "image",
      name: img.name || `图片 ${index + 1}`,
      removable: false,
    });
  });
  attachments.forEach((file: any, index: number) => {
    chips.push({
      id: `attachment:${file.file_name || index}`,
      type: "attachment",
      name: file.file_name || file.name || `附件 ${index + 1}`,
      meta: formatFileSize(file.file_size || file.size),
      removable: false,
    });
  });
  return chips;
});

const latestAgentPreview = computed(() => {
  const stream = Array.isArray(props.message.agentEventStream)
    ? props.message.agentEventStream
    : [];
  for (let index = stream.length - 1; index >= 0; index -= 1) {
    const event = stream[index] || {};
    const progress = event.agent_progress?.message || event.agent_progress_history?.at?.(-1)?.message;
    const text =
      progress ||
      event.content ||
      event.output ||
      event.tool_data?.agent_progress_message ||
      event.tool_data?.message ||
      (event.tool_name ? `正在调用 ${event.tool_name}` : "");
    if (String(text || "").trim()) return String(text).trim();
  }
  if (props.message.thinkContent) return String(props.message.thinkContent);
  if (props.message.content) return String(props.message.content);
  return "正在分析上下文和可用工具";
});

const agentStepPreviews = computed(() => {
  const stream = Array.isArray(props.message.agentEventStream)
    ? props.message.agentEventStream
    : [];
  const steps: string[] = [];
  const seen = new Set<string>();

  for (const event of stream) {
    const progress = event.agent_progress?.message || event.agent_progress_history?.at?.(-1)?.message;
    const text =
      progress ||
      event.content ||
      event.output ||
      event.tool_data?.agent_progress_message ||
      event.tool_data?.message ||
      (event.tool_name ? `正在调用 ${event.tool_name}` : "");
    const normalized = String(text || "").trim();
    if (!normalized || seen.has(normalized)) continue;
    seen.add(normalized);
    steps.push(normalized);
  }

  return steps.slice(-2);
});

const shouldShowThinking = computed(() => {
  return isAssistant.value && props.message.isAgentMode && !props.message.is_completed;
});

const isStructuredAnalysisData = (value: any): value is StructuredAnalysisData => {
  if (!value || typeof value !== "object") return false;
  return (
    value.display_type === "structured_analysis_result" ||
    (
      Array.isArray(value.rows) &&
      Array.isArray(value.columns) &&
      value.chart_requested === true &&
      value.chart?.eligible === true
    )
  );
};

const normalizeStructuredAnalysisBlock = (data: StructuredAnalysisData, source: any): StructuredAnalysisBlock => ({
  display_type: "structured_analysis_result",
  tool_data: {
    ...data,
    display_type: "structured_analysis_result",
  },
  output: source?.output,
  arguments: source?.arguments,
});

const collectStructuredAnalysisFromEvent = (event: any, results: StructuredAnalysisBlock[]) => {
  if (!event || typeof event !== "object") return;
  if (event.pending === true || event.success === false) return;

  const candidates = [
    event.tool_data,
    event.data,
    event.result?.data,
    event.tool_result?.data,
    event.result,
    event.tool_result,
    event,
  ];

  for (const candidate of candidates) {
    if (!candidate || typeof candidate !== "object") continue;
    const data = isStructuredAnalysisData(candidate)
      ? candidate
      : isStructuredAnalysisData(candidate.tool_data)
        ? candidate.tool_data
        : null;
    if (!data) continue;
    if (
      data.display_type !== "structured_analysis_result" &&
      candidate.display_type !== "structured_analysis_result" &&
      event.display_type !== "structured_analysis_result"
    ) {
      continue;
    }
    results.push(normalizeStructuredAnalysisBlock(data, candidate));
    return;
  }
};

const collectStructuredAnalysisFromSteps = (steps: any[], results: StructuredAnalysisBlock[]) => {
  for (const step of steps) {
    if (!step || typeof step !== "object") continue;
    collectStructuredAnalysisFromEvent(step, results);
    const toolCalls = Array.isArray(step.tool_calls) ? step.tool_calls : [];
    for (const toolCall of toolCalls) {
      collectStructuredAnalysisFromEvent(toolCall, results);
      collectStructuredAnalysisFromEvent(toolCall?.result, results);
    }
  }
};

const structuredResultKey = (result: StructuredAnalysisBlock) => {
  const data = result.tool_data;
  const chart = (data.chart || {}) as any;
  const chartId = chart.contract?.id || chart.id || "";
  const chartType = chart.contract?.type || chart.default_type || chart.type || "";
  const chartX = chart.contract?.encoding?.x?.field || chart.x || "";
  const chartY = chart.contract?.encoding?.value?.field || (Array.isArray(chart.y) ? chart.y.join(",") : "");
  const columns = Array.isArray(data.columns)
    ? data.columns.map((column: any) => column?.name).filter(Boolean).join(",")
    : "";
  return [
    chartId,
    chartType,
    chartX,
    chartY,
    data.query || "",
    columns,
    data.row_count ?? data.rows?.length ?? "",
  ].join("|");
};

const structuredAnalysisResults = computed<StructuredAnalysisBlock[]>(() => {
  if (!isAssistant.value) return [];
  const results: StructuredAnalysisBlock[] = [];
  const stream = Array.isArray(props.message.agentEventStream) ? props.message.agentEventStream : [];
  const steps = Array.isArray(props.message.agent_steps) ? props.message.agent_steps : [];
  const toolResults = Array.isArray(props.message.tool_results) ? props.message.tool_results : [];

  stream.forEach((event) => collectStructuredAnalysisFromEvent(event, results));
  collectStructuredAnalysisFromSteps(steps, results);
  toolResults.forEach((event) => collectStructuredAnalysisFromEvent(event, results));

  const seen = new Set<string>();
  return results.filter((result) => {
    const data = result.tool_data;
    if (data.chart_requested !== true || data.chart?.eligible !== true) return false;
    const key = structuredResultKey(result);
    if (seen.has(key)) return false;
    seen.add(key);
    return true;
  });
});

const structuredChartInfos = computed<StructuredChartInfo[]>(() =>
  structuredAnalysisResults.value.map((result) => {
    const chart = (result.tool_data.chart || {}) as any;
    const columns = Array.isArray(result.tool_data.columns)
      ? result.tool_data.columns
        .map((column: any) => column?.name)
        .filter((name: unknown): name is string => typeof name === "string" && Boolean(name))
      : [];

    return {
      id: typeof chart.contract?.id === "string" ? chart.contract.id : (typeof chart.id === "string" ? chart.id : ""),
      chartType: typeof chart.contract?.type === "string" ? chart.contract.type : (typeof chart.default_type === "string" ? chart.default_type : ""),
      x: typeof chart.contract?.encoding?.x?.field === "string" ? chart.contract.encoding.x.field : (typeof chart.x === "string" ? chart.x : ""),
      y: typeof chart.contract?.encoding?.value?.field === "string"
        ? [chart.contract.encoding.value.field]
        : (Array.isArray(chart.y) ? chart.y.filter((field: unknown): field is string => typeof field === "string") : []),
      columns,
      query: typeof result.tool_data.query === "string" ? result.tool_data.query : "",
    };
  }),
);

const answerMarkdown = computed(() => unwrapFinalAnswerWrappers(String(props.message.content || "")));

const sourceReferenceItems = computed(() =>
  buildCitedSourceReferenceItems(
    Array.isArray(props.message.knowledge_references) ? props.message.knowledge_references : [],
    answerMarkdown.value,
    Boolean(props.message.is_completed),
  ),
);

const citationNumberById = computed(() => new Map(
  sourceReferenceItems.value
    .filter((item) => item.citationId)
    .map((item) => [item.citationId, item.number] as const),
));

const answerSplit = computed(() => {
  if (!answerMarkdown.value.trim()) {
    return {
      segments: [] as MobileAnswerSegment[],
      usedResultIndexes: new Set<number>(),
    };
  }

  const split = splitStructuredChartMarkdown(
    answerMarkdown.value,
    structuredAnalysisResults.value.length,
    structuredChartInfos.value,
  );

  const segments = split.segments
    .map((segment, index): MobileAnswerSegment | null => {
      if (segment.kind === "chart") {
        const result = structuredAnalysisResults.value[segment.resultIndex];
        return result ? { key: `chart:${segment.resultIndex}:${index}`, kind: "structured_result", result } : null;
      }
      const html = renderMobileMarkdown(segment.content || "", {
        knowledgeReferences: props.message.knowledge_references,
        streaming: !props.message.is_completed,
        citationNumberById: citationNumberById.value,
      });
      return html ? { key: `markdown:${index}`, kind: "markdown", html } : null;
    })
    .filter((segment): segment is MobileAnswerSegment => Boolean(segment));

  return {
    segments,
    usedResultIndexes: new Set(split.usedResultIndexes),
  };
});

const answerSegments = computed(() => answerSplit.value.segments);
const promotedStructuredResults = computed(() =>
  structuredAnalysisResults.value.filter((_, index) => !answerSplit.value.usedResultIndexes.has(index)),
);

const isArtifactsData = (value: any): value is GeneralAgentArtifactsData => {
  if (!value || typeof value !== "object") return false;
  return (
    value.display_type === "general_agent_artifacts" ||
    Array.isArray(value.artifacts) ||
    typeof value.notice === "string"
  );
};

const collectArtifactsFromEvent = (event: any, results: GeneralAgentArtifactsData[]) => {
  if (!event || typeof event !== "object") return;

  const candidates = [
    event.tool_data,
    event.data,
    event.result?.data,
    event.tool_result?.data,
    event,
  ];

  for (const candidate of candidates) {
    if (!isArtifactsData(candidate)) continue;
    if (
      candidate.display_type === "general_agent_artifacts" ||
      event.display_type === "general_agent_artifacts"
    ) {
      results.push(candidate);
    }
  }
};

const collectArtifactsFromSteps = (steps: any[], results: GeneralAgentArtifactsData[]) => {
  for (const step of steps) {
    if (!step || typeof step !== "object") continue;
    collectArtifactsFromEvent(step, results);
    const toolCalls = Array.isArray(step.tool_calls) ? step.tool_calls : [];
    for (const toolCall of toolCalls) {
      collectArtifactsFromEvent(toolCall, results);
      collectArtifactsFromEvent(toolCall?.result, results);
    }
  }
};

const artifactResult = computed<GeneralAgentArtifactsData | null>(() => {
  const results: GeneralAgentArtifactsData[] = [];
  const stream = Array.isArray(props.message.agentEventStream) ? props.message.agentEventStream : [];
  const steps = Array.isArray(props.message.agent_steps) ? props.message.agent_steps : [];
  const toolResults = Array.isArray(props.message.tool_results) ? props.message.tool_results : [];

  stream.forEach((event) => collectArtifactsFromEvent(event, results));
  collectArtifactsFromSteps(steps, results);
  toolResults.forEach((event) => collectArtifactsFromEvent(event, results));

  const visibleResults = results.filter((item) =>
    (Array.isArray(item.artifacts) && item.artifacts.length > 0) || !!item.notice,
  );
  return visibleResults[visibleResults.length - 1] || null;
});

const normalizeArtifactFile = (file: any): GeneralAgentArtifactFile | null => {
  if (!file || typeof file !== "object") return null;
  const filename = String(file.filename || file.file_name || file.name || "").trim();
  const artifactId = String(file.artifact_id || file.id || file.file_id || "").trim();
  const downloadUrl = String(file.download_url || file.url || "").trim();
  if (!filename && !artifactId && !downloadUrl) return null;

  const typeFromName = filename.includes(".") ? filename.split(".").pop() || "" : "";
  return {
    artifact_id: artifactId || downloadUrl || filename,
    filename: filename || "产物文件",
    file_type: String(file.file_type || file.type || typeFromName || "").replace(/^\./, ""),
    file_size: Number(file.file_size || file.size || 0),
    sha256: String(file.sha256 || ""),
    download_url: downloadUrl,
  };
};

const artifactFiles = computed<GeneralAgentArtifactFile[]>(() => {
  const rawFiles = artifactResult.value?.artifacts || [];
  const seen = new Set<string>();
  const files: GeneralAgentArtifactFile[] = [];
  for (const raw of rawFiles) {
    const file = normalizeArtifactFile(raw);
    if (!file) continue;
    const key = file.artifact_id || file.download_url || file.filename;
    if (seen.has(key)) continue;
    seen.add(key);
    files.push(file);
  }
  return files;
});

const visibleArtifactFiles = computed(() => artifactFiles.value.slice(0, 3));
const hiddenArtifactCount = computed(() => Math.max(0, artifactFiles.value.length - visibleArtifactFiles.value.length));
const artifactNotice = computed(() => artifactResult.value?.notice || "");
const shouldShowArtifacts = computed(() => isAssistant.value && (artifactFiles.value.length > 0 || !!artifactNotice.value));

const artifactMetaText = computed(() => {
  const total = Number(artifactResult.value?.artifact_original_count || 0);
  const dropped = Number(artifactResult.value?.artifact_dropped_count || 0);
  if (total > 0 && dropped > 0) {
    return `返回 ${Math.max(0, total - dropped)}/${total}`;
  }
  return `${artifactFiles.value.length} 个文件`;
});

const artifactFileTypeLabel = (file: GeneralAgentArtifactFile) => {
  const type = String(file.file_type || (file.filename.includes(".") ? file.filename.split(".").pop() : "") || "")
    .replace(/^\./, "")
    .toUpperCase();
  return type || "FILE";
};

const artifactDownloadUrl = (file: GeneralAgentArtifactFile) => {
  if (file.download_url) return file.download_url;
  if (!file.artifact_id) return "";
  return `/api/v1/custom/general-agent/artifacts/${encodeURIComponent(file.artifact_id)}/download`;
};

const downloadArtifact = async (file: GeneralAgentArtifactFile) => {
  const url = artifactDownloadUrl(file);
  if (!url || downloadingArtifactId.value) return;
  downloadingArtifactId.value = file.artifact_id;
  try {
    const result = await getDown(url);
    const blob = result instanceof Blob ? result : new Blob([result as any]);
    downloadBlob(blob, file.filename || "artifact");
  } catch (error: any) {
    MessagePlugin.error(error?.message || "下载失败");
  } finally {
    downloadingArtifactId.value = "";
  }
};

const selectedCitationItem = ref<SourceReferenceItem | null>(null);
const detailItem = ref<SourceReferenceItem | null>(null);
const detailHistoryPushed = ref(false);

const sourceItemFromElement = (el: HTMLElement): SourceReferenceItem | null => {
  const citationId = el.getAttribute("data-source-id") || "";
  const matched = sourceReferenceItems.value.find((item) => item.citationId === citationId);
  if (matched) return matched;

  const type = (el.getAttribute("data-source-type") || "knowledge") as SourceReferenceKind;
  const title = el.getAttribute("data-title") || sourceTypeLabel(type);
  const url = el.getAttribute("data-url") || "";
  const knowledgeBaseId = el.getAttribute("data-kb-id") || "";
  const knowledgeId = el.getAttribute("data-knowledge-id") || "";
  const slug = el.getAttribute("data-slug") || "";
  const sourceId = el.getAttribute("data-data-source-id") || "";
  return {
    key: citationId || `${type}:${title}`,
    number: Number(el.getAttribute("data-citation-number") || "0") || 0,
    citationId,
    type,
    title,
    sourceLabel: el.getAttribute("data-source-label") || sourceTypeLabel(type),
    snippet: "",
    count: 1,
    icon: type === "web" ? "internet" : type === "wiki" ? "browse" : type === "data_source" ? "server" : "file",
    url,
    knowledgeBaseId,
    knowledgeId,
    slug,
    sourceId,
    clickable: type === "web"
      ? Boolean(url)
      : type === "wiki"
        ? Boolean(knowledgeBaseId && slug)
        : type === "knowledge"
          ? Boolean(knowledgeId || knowledgeBaseId)
          : Boolean(sourceId),
  };
};

const handleMarkdownClick = (event: MouseEvent) => {
  const target = event.target as HTMLElement;
  const sourceEl = target.closest?.(".citation-source") as HTMLElement | null;
  if (!sourceEl) return;
  event.preventDefault();
  event.stopPropagation();
  const item = sourceItemFromElement(sourceEl);
  if (item) selectedCitationItem.value = item;
};

const handleMarkdownKeydown = (event: KeyboardEvent) => {
  if (event.key !== "Enter" && event.key !== " ") return;
  const target = event.target as HTMLElement;
  if (!target.closest?.(".citation-source")) return;
  handleMarkdownClick(event as unknown as MouseEvent);
};

const openExternalUrl = (url: string) => {
  const anchor = document.createElement("a");
  anchor.href = url;
  anchor.target = "_blank";
  anchor.rel = "noopener noreferrer";
  anchor.style.display = "none";
  document.body.appendChild(anchor);
  anchor.click();
  anchor.remove();
};

const pushDetailHistory = () => {
  if (detailHistoryPushed.value || typeof window === "undefined") return;
  window.history.pushState(
    { ...(window.history.state || {}), mobileSourceDetail: true },
    "",
    window.location.href,
  );
  detailHistoryPushed.value = true;
};

const openCitationSource = (item: SourceReferenceItem) => {
  selectedCitationItem.value = null;
  if (item.type === "web") {
    if (item.url) openExternalUrl(item.url);
    return;
  }
  if (item.type === "data_source") {
    MessagePlugin.info("移动端暂不支持查看数据源详情");
    return;
  }
  detailItem.value = item;
  pushDetailHistory();
};

const closeCitationPreview = () => {
  selectedCitationItem.value = null;
};

const closeSourceDetail = () => {
  if (detailHistoryPushed.value && typeof window !== "undefined") {
    window.history.back();
    return;
  }
  detailItem.value = null;
};

const handlePopState = () => {
  if (!detailHistoryPushed.value) return;
  detailHistoryPushed.value = false;
  detailItem.value = null;
  selectedCitationItem.value = null;
};

if (typeof window !== "undefined") {
  window.addEventListener("popstate", handlePopState);
}

onBeforeUnmount(() => {
  if (typeof window !== "undefined") {
    window.removeEventListener("popstate", handlePopState);
  }
});
</script>

<template>
  <article class="mobile-message" :class="{ 'is-user': isUser, 'is-assistant': isAssistant }">
    <div class="message-stack">
      <MobileResourceRail
        v-if="isUser && (mentionedChips.length || uploadChips.length)"
        dense
        :items="[...mentionedChips, ...uploadChips]"
      />

      <div v-if="isUser" class="user-bubble">
        <p>{{ message.content }}</p>
      </div>

      <div v-else class="assistant-card">
        <div v-if="shouldShowThinking" class="thinking-card">
          <div class="thinking-title">正在思考</div>
          <div v-if="agentStepPreviews.length" class="thinking-steps">
            <div v-for="step in agentStepPreviews" :key="step" class="thinking-step">
              <span />
              <em>{{ step }}</em>
            </div>
          </div>
          <div v-else class="thinking-preview">{{ latestAgentPreview }}</div>
        </div>

        <template v-if="!shouldShowThinking">
          <template v-for="segment in answerSegments" :key="segment.key">
            <div
              v-if="segment.kind === 'markdown'"
              class="mobile-markdown"
              v-html="segment.html"
              @click="handleMarkdownClick"
              @keydown.enter.prevent="handleMarkdownKeydown"
              @keydown.space.prevent="handleMarkdownKeydown"
            />
            <MobileStructuredAnalysisResult
              v-else
              :data="segment.result.tool_data"
            />
          </template>
          <MobileStructuredAnalysisResult
            v-for="(result, index) in promotedStructuredResults"
            :key="`promoted-chart:${index}`"
            :data="result.tool_data"
          />
        </template>

        <section v-if="shouldShowArtifacts" class="mobile-artifacts">
          <div class="mobile-artifacts__head">
            <div>
              <strong>产物</strong>
              <span>{{ artifactMetaText }}</span>
            </div>
          </div>
          <p v-if="artifactNotice" class="mobile-artifacts__notice">{{ artifactNotice }}</p>
          <div v-if="artifactFiles.length" class="mobile-artifacts__list">
            <div v-for="file in visibleArtifactFiles" :key="file.artifact_id" class="mobile-artifact-file">
              <div class="mobile-artifact-file__type">{{ artifactFileTypeLabel(file) }}</div>
              <div class="mobile-artifact-file__main">
                <strong>{{ file.filename }}</strong>
                <span>{{ formatFileSize(file.file_size) || '未知大小' }}</span>
              </div>
              <button
                type="button"
                class="mobile-artifact-file__download"
                :class="{ loading: downloadingArtifactId === file.artifact_id }"
                :disabled="downloadingArtifactId === file.artifact_id || !artifactDownloadUrl(file)"
                :aria-label="`下载${file.filename}`"
                @click="downloadArtifact(file)"
              >
                <MobileIcon name="download" />
              </button>
            </div>
            <div v-if="hiddenArtifactCount" class="mobile-artifacts__more">
              还有 {{ hiddenArtifactCount }} 个产物可在桌面端查看
            </div>
          </div>
          <div v-else class="mobile-artifacts__empty">没有可返回的产物文件</div>
        </section>

        <div v-if="!message.is_completed && !shouldShowThinking" class="typing-dot">
          <span />
          <span />
          <span />
        </div>
      </div>
    </div>
    <MobileCitationSheet
      :item="selectedCitationItem"
      @close="closeCitationPreview"
      @open="openCitationSource"
    />
    <MobileSourceDetailSheet
      :item="detailItem"
      @close="closeSourceDetail"
    />
  </article>
</template>

<style scoped>
.mobile-message {
  display: flex;
  align-items: flex-start;
  width: 100%;
}

.mobile-message.is-user {
  justify-content: flex-end;
}

.mobile-message.is-assistant {
  justify-content: flex-start;
}

.message-stack {
  display: flex;
  min-width: 0;
  max-width: 100%;
  flex-direction: column;
  gap: 7px;
}

.is-user .message-stack {
  align-items: flex-end;
  flex: 0 1 86%;
  width: auto;
  max-width: 680px;
}

.is-assistant .message-stack {
  align-items: flex-start;
  flex: 1 1 100%;
  width: auto;
  max-width: 100%;
}

.user-bubble {
  min-width: 0;
  max-width: 100%;
  border-radius: 16px 16px 4px 16px;
  background: #07c160;
  color: #fff;
  padding: 10px 12px;
  font-size: var(--mobile-reading-font-size);
  line-height: var(--mobile-reading-line-height);
  overflow-wrap: anywhere;
}

.user-bubble p {
  margin: 0;
  white-space: pre-wrap;
  overflow-wrap: anywhere;
  word-break: break-word;
}

.assistant-card {
  display: flex;
  min-width: 0;
  width: 100%;
  max-width: 100%;
  flex-direction: column;
  gap: 12px;
  border: 0;
  border-radius: 16px 16px 16px 4px;
  background: #fff;
  padding: 12px 14px;
  font-size: var(--mobile-reading-font-size);
  line-height: var(--mobile-reading-line-height);
  box-shadow: 0 1px 3px rgba(15, 36, 26, 0.06);
}

.assistant-card__refs {
  margin-bottom: 2px;
}

.thinking-card {
  display: flex;
  flex-direction: column;
  gap: 7px;
}

.thinking-title {
  width: max-content;
  background:
    linear-gradient(100deg, #36564a 0%, #36564a 35%, #07c160 48%, #36564a 62%, #36564a 100%);
  background-size: 240% 100%;
  color: transparent;
  font-size: 17px;
  font-weight: 650;
  line-height: 1.55;
  -webkit-background-clip: text;
  background-clip: text;
  animation: thinkingWave 1.8s linear infinite;
}

.thinking-preview {
  display: -webkit-box;
  overflow: hidden;
  color: #62736b;
  font-size: 16px;
  line-height: 1.85;
  -webkit-box-orient: vertical;
  -webkit-line-clamp: 2;
  mask-image: linear-gradient(180deg, #000 0%, #000 55%, transparent 100%);
  white-space: pre-wrap;
}

.thinking-steps {
  display: flex;
  min-width: 0;
  flex-direction: column;
  gap: 6px;
}

.thinking-step {
  display: grid;
  min-width: 0;
  grid-template-columns: 7px minmax(0, 1fr);
  align-items: start;
  gap: 7px;
  color: #62736b;
  font-size: 16px;
  line-height: 1.85;
}

.thinking-step span {
  width: 6px;
  height: 6px;
  margin-top: 7px;
  border-radius: 50%;
  background: #8ed9ae;
}

.thinking-step em {
  display: -webkit-box;
  overflow: hidden;
  font-style: normal;
  overflow-wrap: anywhere;
  -webkit-box-orient: vertical;
  -webkit-line-clamp: 1;
}

.mobile-markdown {
  min-width: 0;
  max-width: 100%;
  overflow-wrap: anywhere;
  color: #1f2d27;
  font-size: var(--mobile-reading-font-size);
  line-height: var(--mobile-reading-line-height);
}

.mobile-markdown :deep(p) {
  margin: 0 0 14px;
}

.mobile-markdown :deep(p:last-child) {
  margin-bottom: 0;
}

.mobile-markdown :deep(ul),
.mobile-markdown :deep(ol) {
  margin: 10px 0 12px;
  padding-left: 22px;
}

.mobile-markdown :deep(pre) {
  max-width: 100%;
  overflow-x: auto;
  border-radius: 8px;
  background: #111b17;
  color: #e7f3ee;
  padding: 12px;
}

.mobile-markdown :deep(code) {
  border-radius: 4px;
  background: #eef4f1;
  padding: 1px 4px;
}

.mobile-markdown :deep(.citation-source) {
  display: inline-flex;
  min-width: 25px;
  height: 25px;
  align-items: center;
  justify-content: center;
  border-radius: 999px;
  background: #e9f6ef;
  color: #078f49;
  font-size: 0.82em;
  font-weight: 620;
  line-height: 1;
  margin: 0 2px;
  vertical-align: 0.08em;
  cursor: pointer;
  -webkit-tap-highlight-color: transparent;
}

.mobile-markdown :deep(.citation-source:active) {
  background: #d8f0e2;
  transform: scale(0.96);
}

.mobile-markdown :deep(.citation-source .citation-number) {
  line-height: 1;
}

.mobile-markdown :deep(.citation-tip) {
  display: none;
}

.mobile-markdown :deep(.citation-web) {
  color: #078f49;
  text-decoration: none;
}

.mobile-markdown :deep(pre code) {
  background: transparent;
  padding: 0;
}

.mobile-markdown :deep(table) {
  width: 100%;
  max-width: 100%;
  border-collapse: collapse;
  table-layout: fixed;
  font-size: 16px;
  line-height: 1.75;
}

.mobile-markdown :deep(th),
.mobile-markdown :deep(td) {
  border-bottom: 1px solid #e4ede8;
  padding: 8px 6px;
  text-align: left;
  vertical-align: top;
  white-space: normal;
  overflow-wrap: anywhere;
  word-break: break-word;
}

.mobile-markdown :deep(th) {
  color: #334840;
  font-weight: 650;
}

.mobile-artifacts {
  display: flex;
  min-width: 0;
  flex-direction: column;
  gap: 9px;
  border-radius: 10px;
  background: #f7fbf9;
  padding: 10px;
  line-height: 1.35;
}

.mobile-artifacts__head {
  display: flex;
  align-items: center;
  justify-content: space-between;
}

.mobile-artifacts__head div {
  display: flex;
  min-width: 0;
  align-items: baseline;
  gap: 8px;
}

.mobile-artifacts__head strong {
  color: #17261f;
  font-size: 16px;
  font-weight: 650;
}

.mobile-artifacts__head span,
.mobile-artifacts__notice,
.mobile-artifacts__empty,
.mobile-artifacts__more {
  color: #708279;
  font-size: 13px;
}

.mobile-artifacts__notice {
  margin: 0;
  border-radius: 8px;
  background: #fff8e9;
  color: #9c6410;
  padding: 8px 9px;
}

.mobile-artifacts__list {
  display: flex;
  min-width: 0;
  flex-direction: column;
  gap: 8px;
}

.mobile-artifact-file {
  display: grid;
  min-width: 0;
  grid-template-columns: 42px minmax(0, 1fr) 34px;
  align-items: center;
  gap: 9px;
  border: 1px solid #dce8e2;
  border-radius: 9px;
  background: #fff;
  padding: 8px;
}

.mobile-artifact-file__type {
  display: grid;
  width: 40px;
  height: 30px;
  place-items: center;
  border-radius: 7px;
  background: #eaf8f0;
  color: #078f49;
  font-size: 11px;
  font-weight: 750;
}

.mobile-artifact-file__main {
  display: flex;
  min-width: 0;
  flex-direction: column;
  gap: 4px;
}

.mobile-artifact-file__main strong,
.mobile-artifact-file__main span {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.mobile-artifact-file__main strong {
  color: #1d2d26;
  font-size: 15px;
  font-weight: 650;
}

.mobile-artifact-file__main span {
  color: #7b8b84;
  font-size: 13px;
}

.mobile-artifact-file__download {
  display: grid;
  width: 32px;
  height: 32px;
  place-items: center;
  border: 1px solid #bfe8cf;
  border-radius: 50%;
  background: #fff;
  color: #078f49;
  padding: 0;
}

.mobile-artifact-file__download:disabled {
  color: #a6b6ae;
  border-color: #dfe8e3;
}

.mobile-artifact-file__download.loading {
  animation: artifactDownloadPulse 0.82s ease-in-out infinite;
}

@keyframes artifactDownloadPulse {
  0%,
  100% {
    opacity: 0.58;
    transform: translateY(-2px);
  }
  50% {
    opacity: 1;
    transform: translateY(2px);
  }
}

.typing-dot {
  display: flex;
  gap: 4px;
  padding: 4px 0;
}

.typing-dot span {
  width: 5px;
  height: 5px;
  border-radius: 50%;
  background: #07c160;
  animation: dotPulse 1.2s ease-in-out infinite;
}

.typing-dot span:nth-child(2) {
  animation-delay: 0.15s;
}

.typing-dot span:nth-child(3) {
  animation-delay: 0.3s;
}

@keyframes thinkingWave {
  0% {
    background-position: 140% 0;
  }
  100% {
    background-position: -140% 0;
  }
}

@keyframes dotPulse {
  0%,
  100% {
    opacity: 0.3;
    transform: translateY(0);
  }
  50% {
    opacity: 1;
    transform: translateY(-2px);
  }
}
</style>
