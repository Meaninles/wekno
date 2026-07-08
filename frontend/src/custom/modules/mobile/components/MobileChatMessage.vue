<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, ref, watch } from "vue";
import { MessagePlugin } from "tdesign-vue-next";
import { getChunkByIdOnly } from "@/api/knowledge-base";
import { getDown } from "@/utils/request";
import { resolveCitationChunkId } from "@/utils/citationMarkdown";
import { unwrapFinalAnswerWrappers } from "@/utils/finalAnswer";
import { clearProtectedFileFailureCache, hydrateProtectedFileImages } from "@/utils/security";
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
  buildSourceReferenceItems,
  focusEmptyKnowledgeDocumentLinkReferenceTarget,
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
const messageBodyRef = ref<HTMLElement | null>(null);
let messageHydrationRun = 0;
let completionHydrationCacheCleared = false;

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

const allSourceReferenceItems = computed(() =>
  buildSourceReferenceItems(
    Array.isArray(props.message.knowledge_references) ? props.message.knowledge_references : [],
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
const hasExplicitStructuredChartPlaceholders = computed(() =>
  /\{\{\s*chart\s*:\s*([a-zA-Z0-9_-]+|\d+)\s*\}\}/.test(answerMarkdown.value),
);

const promotedStructuredResults = computed(() => {
  if (hasExplicitStructuredChartPlaceholders.value) return [];
  return structuredAnalysisResults.value.filter((_, index) => !answerSplit.value.usedResultIndexes.has(index));
});

const renderedMarkdownImageHydrationKey = computed(() =>
  answerSegments.value
    .filter((segment): segment is Extract<MobileAnswerSegment, { kind: "markdown" }> => segment.kind === "markdown")
    .map((segment) => segment.html)
    .join("\n"),
);

watch(
  [renderedMarkdownImageHydrationKey, () => props.message.is_completed],
  async ([, completed]) => {
    if (!isAssistant.value || shouldShowThinking.value) return;
    const run = ++messageHydrationRun;
    if (completed && !completionHydrationCacheCleared) {
      clearProtectedFileFailureCache();
      completionHydrationCacheCleared = true;
    } else if (!completed) {
      completionHydrationCacheCleared = false;
    }
    await nextTick();
    if (run !== messageHydrationRun) return;
    await hydrateProtectedFileImages(messageBodyRef.value);
  },
  { immediate: true, flush: "post" },
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

const emptySourceItem = (overrides: Partial<SourceReferenceItem>): SourceReferenceItem => ({
  key: "",
  number: 0,
  citationId: "",
  type: "knowledge",
  title: "来源",
  sourceLabel: "",
  snippet: "",
  count: 1,
  icon: "file",
  url: "",
  knowledgeBaseId: "",
  knowledgeId: "",
  chunkId: "",
  chunkIndex: null,
  startAt: null,
  endAt: null,
  slug: "",
  sourceId: "",
  clickable: true,
  ...overrides,
});

const pickKbId = (value: unknown): string => {
  if (!value) return "";
  if (typeof value === "string") return value;
  if (Array.isArray(value)) {
    for (const item of value) {
      if (typeof item === "string" && item) return item;
    }
  }
  return "";
};

const sameSlug = (left: string, right: string) => {
  if (left === right) return true;
  try {
    return decodeURIComponent(left) === right || left === decodeURIComponent(right);
  } catch {
    return false;
  }
};

const foundKbMapsFromEvent = (event: any): Array<Record<string, unknown>> => {
  const candidates = [
    event?.tool_data?.found_kbs,
    event?.data?.found_kbs,
    event?.result?.found_kbs,
    event?.result?.data?.found_kbs,
    event?.tool_result?.found_kbs,
    event?.tool_result?.data?.found_kbs,
  ];
  return candidates.filter((item): item is Record<string, unknown> =>
    Boolean(item && typeof item === "object" && !Array.isArray(item)),
  );
};

const getKbIdForWiki = (slug: string) => {
  if (!slug) return "";

  const fromReferences = allSourceReferenceItems.value.find((item) =>
    item.type === "wiki" && sameSlug(item.slug, slug) && item.knowledgeBaseId,
  );
  if (fromReferences?.knowledgeBaseId) return fromReferences.knowledgeBaseId;

  const eventGroups = [
    Array.isArray(props.message.agentEventStream) ? props.message.agentEventStream : [],
    Array.isArray(props.message.agent_steps) ? props.message.agent_steps : [],
    Array.isArray(props.message.tool_results) ? props.message.tool_results : [],
  ];
  for (const group of eventGroups) {
    for (let index = group.length - 1; index >= 0; index -= 1) {
      for (const foundKbs of foundKbMapsFromEvent(group[index])) {
        const direct = pickKbId(foundKbs[slug]);
        if (direct) return direct;
        const matchedKey = Object.keys(foundKbs).find((key) => sameSlug(key, slug));
        if (matchedKey) {
          const matched = pickKbId(foundKbs[matchedKey]);
          if (matched) return matched;
        }
      }
    }
  }

  return "";
};

const sourceItemFromWikiElement = (el: HTMLElement): SourceReferenceItem | null => {
  const slug = el.getAttribute("data-slug") || "";
  if (!slug) return null;
  const kbId = el.getAttribute("data-kb-id") || getKbIdForWiki(slug);
  return emptySourceItem({
    key: `wiki:${kbId}:${slug}`,
    type: "wiki",
    title: el.textContent?.trim() || slug,
    sourceLabel: "Wiki",
    icon: "bookmark",
    knowledgeBaseId: kbId,
    slug,
    clickable: Boolean(kbId && slug),
  });
};

const sourceItemFromWikiRoute = (href: string, title = ""): SourceReferenceItem | null => {
  if (!href || typeof window === "undefined") return null;
  let url: URL;
  try {
    url = new URL(href, window.location.origin);
  } catch {
    return null;
  }
  const match = url.pathname.match(/\/platform\/knowledge-bases\/([^/?#]+)/);
  if (!match) return null;
  const slug = url.searchParams.get("slug") || url.searchParams.get("wiki_slug") || "";
  if (!slug) return null;
  const tab = url.searchParams.get("tab") || "";
  if (tab && tab !== "wiki" && tab !== "graph") return null;
  const kbId = decodeURIComponent(match[1] || "");
  return emptySourceItem({
    key: `wiki:${kbId}:${slug}`,
    type: "wiki",
    title: title || slug,
    sourceLabel: "Wiki",
    icon: "bookmark",
    knowledgeBaseId: kbId,
    slug,
    clickable: Boolean(kbId && slug),
  });
};

const focusKnowledgeDocumentLinkTarget = (href: string, origin: HTMLElement) => {
  if (href !== "" || typeof window === "undefined") return false;
  return focusEmptyKnowledgeDocumentLinkReferenceTarget(
    origin.closest(".mobile-message") || origin.closest(".mobile-markdown") || document,
    origin as HTMLAnchorElement,
  );
};

const sourceItemFromElement = (el: HTMLElement): SourceReferenceItem | null => {
  const citationId = el.getAttribute("data-source-id") || "";
  const matched = sourceReferenceItems.value.find((item) => item.citationId === citationId);
  if (matched) return matched;

  const type = (el.getAttribute("data-source-type") || "knowledge") as SourceReferenceKind;
  const title = el.getAttribute("data-title") || sourceTypeLabel(type);
  const url = el.getAttribute("data-url") || "";
  const knowledgeBaseId = el.getAttribute("data-kb-id") || "";
  const knowledgeId = el.getAttribute("data-knowledge-id") || "";
  const chunkId = el.getAttribute("data-chunk-id") || "";
  const chunkIndexAttr = el.getAttribute("data-chunk-index") || "";
  const chunkIndex = chunkIndexAttr === "" ? NaN : Number(chunkIndexAttr);
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
    chunkId,
    chunkIndex: Number.isFinite(chunkIndex) ? chunkIndex : null,
    startAt: null,
    endAt: null,
    slug,
    sourceId,
    clickable: type === "web"
      ? Boolean(url)
      : type === "wiki"
        ? Boolean(slug)
        : type === "knowledge"
          ? Boolean(chunkId || knowledgeId || knowledgeBaseId)
          : Boolean(sourceId),
  };
};

const openWikiSource = (item: SourceReferenceItem | null) => {
  if (!item) return false;
  if (!item.slug) {
    MessagePlugin.warning("没有找到这个 Wiki 页面");
    return true;
  }
  detailItem.value = item;
  pushDetailHistory();
  return true;
};

const openLegacyKbCitation = async (el: HTMLElement) => {
  const title = el.getAttribute("data-doc") || "知识库文档片段";
  const kbId = el.getAttribute("data-kb-id") || "";
  const rawChunkId = el.getAttribute("data-chunk-id") || "";
  const refs = Array.isArray(props.message.knowledge_references) ? props.message.knowledge_references : [];
  const chunkId = resolveCitationChunkId(rawChunkId, { doc: title, kbId }, refs) || rawChunkId;
  const ref = refs.find((item: any) => item?.id === chunkId || item?.metadata?.chunk_id === chunkId);
  const savedContent = String(ref?.content || "").trim();

  const baseItem = emptySourceItem({
    key: `knowledge:${kbId}:${chunkId || title}`,
    type: "knowledge",
    title,
    sourceLabel: "知识库文档片段",
    content: savedContent,
    snippet: savedContent || "正在加载文档片段内容...",
    icon: "file",
    knowledgeBaseId: kbId || ref?.knowledge_base_id || ref?.metadata?.knowledge_base_id || "",
    knowledgeId: ref?.knowledge_id || ref?.metadata?.knowledge_id || "",
    chunkId,
    clickable: true,
  });
  detailItem.value = baseItem;
  pushDetailHistory();

  if (!chunkId) {
    detailItem.value = {
      ...baseItem,
      snippet: savedContent || "这个引用没有关联到可打开的文档片段内容。",
    };
    return;
  }

  try {
    const res: any = await getChunkByIdOnly(chunkId);
    const content = String(res?.data?.content || "").trim();
    detailItem.value = {
      ...baseItem,
      knowledgeId: baseItem.knowledgeId || res?.data?.knowledge_id || "",
      knowledgeBaseId: baseItem.knowledgeBaseId || res?.data?.knowledge_base_id || "",
      content,
      snippet: content || "没有找到这个文档片段的正文内容。",
    };
  } catch (error) {
    console.warn("[mobile] load citation chunk failed", error);
    detailItem.value = {
      ...baseItem,
      snippet: "文档片段内容加载失败，可以稍后重试。",
    };
  }
};

const handleMarkdownClick = (event: MouseEvent) => {
  const target = event.target as HTMLElement;
  const sourceEl = target.closest?.(".citation-source") as HTMLElement | null;
  if (sourceEl) {
    event.preventDefault();
    event.stopPropagation();
    const item = sourceItemFromElement(sourceEl);
    if (item) selectedCitationItem.value = item;
    return;
  }

  const wikiEl = target.closest?.(".citation-wiki, .wiki-content-link") as HTMLElement | null;
  if (wikiEl) {
    event.preventDefault();
    event.stopPropagation();
    openWikiSource(sourceItemFromWikiElement(wikiEl));
    return;
  }

  const kbEl = target.closest?.(".citation-kb") as HTMLElement | null;
  if (kbEl) {
    event.preventDefault();
    event.stopPropagation();
    void openLegacyKbCitation(kbEl);
    return;
  }

  const webEl = target.closest?.(".citation-web") as HTMLElement | null;
  if (webEl) {
    event.preventDefault();
    event.stopPropagation();
    const url = webEl.getAttribute("data-url") || webEl.getAttribute("href") || "";
    if (url) openExternalUrl(url);
    return;
  }

  const linkEl = target.closest?.("a") as HTMLAnchorElement | null;
  if (!linkEl) return;
  const rawHref = linkEl.getAttribute("href");
  if (focusKnowledgeDocumentLinkTarget(rawHref ?? "", linkEl)) {
    event.preventDefault();
    event.stopPropagation();
    return;
  }
  const href = rawHref || linkEl.href || "";
  const wikiRouteItem = sourceItemFromWikiRoute(href, linkEl.textContent?.trim() || "");
  if (wikiRouteItem) {
    event.preventDefault();
    event.stopPropagation();
    openWikiSource(wikiRouteItem);
    return;
  }
  if (/^https?:\/\//i.test(href)) {
    event.preventDefault();
    event.stopPropagation();
    openExternalUrl(href);
  }
};

const handleMarkdownActivationKey = (target: HTMLElement) => {
  const sourceEl = target.closest?.(".citation-source") as HTMLElement | null;
  if (sourceEl) {
    const item = sourceItemFromElement(sourceEl);
    if (item) selectedCitationItem.value = item;
    return true;
  }

  const wikiEl = target.closest?.(".citation-wiki, .wiki-content-link") as HTMLElement | null;
  if (wikiEl) return openWikiSource(sourceItemFromWikiElement(wikiEl));

  const kbEl = target.closest?.(".citation-kb") as HTMLElement | null;
  if (kbEl) {
    void openLegacyKbCitation(kbEl);
    return true;
  }

  const webEl = target.closest?.(".citation-web") as HTMLElement | null;
  if (webEl) {
    const url = webEl.getAttribute("data-url") || webEl.getAttribute("href") || "";
    if (url) openExternalUrl(url);
    return true;
  }

  const linkEl = target.closest?.("a") as HTMLAnchorElement | null;
  if (!linkEl) return false;
  const rawHref = linkEl.getAttribute("href");
  if (focusKnowledgeDocumentLinkTarget(rawHref ?? "", linkEl)) return true;
  const href = rawHref || linkEl.href || "";
  const wikiRouteItem = sourceItemFromWikiRoute(href, linkEl.textContent?.trim() || "");
  if (wikiRouteItem) return openWikiSource(wikiRouteItem);
  if (/^https?:\/\//i.test(href)) {
    openExternalUrl(href);
    return true;
  }
  return false;
};

const handleMarkdownKeydown = (event: KeyboardEvent) => {
  if (event.key !== "Enter" && event.key !== " ") return;
  const target = event.target as HTMLElement;
  if (!handleMarkdownActivationKey(target)) return;
  event.preventDefault();
  event.stopPropagation();
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
  <article ref="messageBodyRef" class="mobile-message" :class="{ 'is-user': isUser, 'is-assistant': isAssistant }">
    <div class="message-stack">
      <MobileResourceRail
        v-if="isUser && (mentionedChips.length || uploadChips.length)"
        dense
        align-end
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

.mobile-markdown :deep(.citation-source.is-source-jump-target) {
  background: #07c160;
  color: #fff;
  box-shadow: 0 0 0 4px rgba(7, 193, 96, 0.18);
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

.mobile-markdown :deep(.citation-wiki),
.mobile-markdown :deep(.wiki-content-link) {
  color: #078f49;
  font-weight: 620;
  text-decoration: underline;
  text-decoration-color: rgba(7, 143, 73, 0.32);
  text-underline-offset: 3px;
  -webkit-tap-highlight-color: transparent;
}

.mobile-markdown :deep(.citation-kb) {
  display: inline-flex;
  max-width: 100%;
  align-items: center;
  gap: 4px;
  border-radius: 999px;
  background: #eef8f3;
  color: #078f49;
  padding: 1px 7px;
  font-size: 0.86em;
  line-height: 1.6;
  vertical-align: 0.08em;
  cursor: pointer;
  -webkit-tap-highlight-color: transparent;
}

.mobile-markdown :deep(.citation-kb:active),
.mobile-markdown :deep(.citation-wiki:active),
.mobile-markdown :deep(.wiki-content-link:active) {
  opacity: 0.72;
}

.mobile-markdown :deep(.citation-kb .citation-text) {
  overflow: hidden;
  min-width: 0;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.mobile-markdown :deep(pre code) {
  background: transparent;
  padding: 0;
}

.mobile-markdown :deep(img.markdown-image),
.mobile-markdown :deep(img[data-protected-src]) {
  display: block;
  width: 100%;
  max-width: 100%;
  height: auto;
  margin: 12px 0;
  border-radius: 10px;
  background: #edf3f0;
  object-fit: contain;
}

.mobile-markdown :deep(img[data-img-loading]) {
  min-height: 160px;
  opacity: 0.9;
}

.mobile-markdown :deep(.chat-markdown-table) {
  width: 100%;
  max-width: 100%;
  overflow-x: auto;
  margin: 0 0 14px;
  border: 1px solid #e1e9e5;
  border-radius: 8px;
  background: #fff;
  -webkit-overflow-scrolling: touch;
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
