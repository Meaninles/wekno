<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, reactive, ref, watch } from "vue";
import { useRoute, useRouter } from "vue-router";
import { MessagePlugin } from "tdesign-vue-next";
import {
  batchQueryKnowledge,
  delKnowledgeDetails,
  downKnowledgeDetails,
  listKnowledgeBases,
  listKnowledgeFiles,
  searchKnowledge,
  uploadKnowledgeFile,
} from "@/api/knowledge-base";
import { useAuthStore } from "@/stores/auth";
import { useEditorResourcesStore } from "@/stores/editorResources";
import { useOrganizationStore } from "@/stores/organization";
import { filterUploadFiles } from "@/views/knowledge/utils/uploadSources";
import {
  buildMobileKnowledgeCatalog,
  type MobileKnowledgeBase,
} from "../knowledgeCatalog";
import { KNOWLEDGE_DOCUMENT_SEARCH_POLICY } from "@/custom/modules/knowledgeSearch/searchPolicy";
import { openRouteInNewPage } from "@/custom/modules/knowledgeSearch/openRouteInNewPage";
import {
  downloadBlob,
  formatFileSize,
  isParseInFlight,
  parseStatusClass,
  parseStatusText,
} from "../utils";

type KnowledgeBaseRow = Record<string, any>;
type KnowledgeFileRow = Record<string, any>;

const DOCUMENT_SEARCH_PAGE_SIZE = KNOWLEDGE_DOCUMENT_SEARCH_POLICY.pageSize;
const DOCUMENT_SEARCH_MIN_LENGTH = KNOWLEDGE_DOCUMENT_SEARCH_POLICY.minLength;
const DOCUMENT_SEARCH_DEBOUNCE_MS = KNOWLEDGE_DOCUMENT_SEARCH_POLICY.debounceMs;

const router = useRouter();
const route = useRoute();
const authStore = useAuthStore();
const editorResources = useEditorResourcesStore();
const organizationStore = useOrganizationStore();

const localKnowledgeBases = ref<KnowledgeBaseRow[]>([]);
const fileList = ref<KnowledgeFileRow[]>([]);
const selectedKbId = ref("");
const loadingKbs = ref(false);
const loadingFiles = ref(false);
const uploadInputRef = ref<HTMLInputElement | null>(null);
const uploading = ref(false);
const personalExpanded = ref(true);
const sharedExpanded = ref(true);
const busyMap = reactive<Record<string, "downloading" | "deleting" | undefined>>({});

const searchOpen = ref(false);
const searchQuery = ref("");
const searchInputRef = ref<HTMLInputElement | null>(null);
const documentSearchResults = ref<KnowledgeFileRow[]>([]);
const documentSearchLoading = ref(false);
const documentSearchLoadingMore = ref(false);
const documentSearchHasMore = ref(false);
const documentSearchError = ref("");
const detailFilterKeyword = ref("");
const detailFocusDocumentId = ref("");

let pollTimer: ReturnType<typeof setTimeout> | null = null;
let searchTimer: ReturnType<typeof setTimeout> | null = null;
let searchRequestGeneration = 0;

const kbList = computed<MobileKnowledgeBase[]>(() =>
  buildMobileKnowledgeCatalog(
    localKnowledgeBases.value,
    organizationStore.sharedKnowledgeBases,
    {
      currentUserId: authStore.currentUserId,
      currentTenantRole: authStore.currentTenantRole,
    },
  ),
);
const personalKnowledgeBases = computed(() => kbList.value.filter((item) => item.group === "personal"));
const sharedKnowledgeBases = computed(() => kbList.value.filter((item) => item.group === "shared"));
const selectedKb = computed(() => kbList.value.find((item) => item.id === selectedKbId.value) || null);
const selectedCanEdit = computed(() => selectedKb.value?.canEditContent === true);
const hasRunningParse = computed(() => fileList.value.some((item) => isParseInFlight(item.parse_status)));
const topbarTitle = computed(() => selectedKb.value?.name || (searchOpen.value ? "搜索" : "知识库"));
const normalizedSearchQuery = computed(() => searchQuery.value.trim());
const knowledgeBaseById = computed(() => new Map(kbList.value.map((item) => [item.id, item])));
const matchedKnowledgeBases = computed(() => {
  const keyword = normalizedSearchQuery.value.toLocaleLowerCase();
  if (!keyword) return [];
  return kbList.value.filter((item) => item.name.toLocaleLowerCase().includes(keyword));
});
const visibleDocumentSearchResults = computed(() =>
  documentSearchResults.value.filter((item) => knowledgeBaseById.value.has(String(item.knowledge_base_id || ""))),
);
const supportedFileTypes = computed<Set<string>>(() => {
  const engines = editorResources.parserEngines || [];
  if (!engines.length) return new Set<string>();

  const rules: { file_types?: string[]; engine?: string }[] =
    (selectedKb.value?.chunking_config as any)?.parser_engine_rules || [];
  const ruleMap = new Map<string, string>();
  rules.forEach((rule) => {
    (rule.file_types || []).forEach((fileType) => {
      if (fileType && rule.engine) ruleMap.set(fileType.toLowerCase(), rule.engine);
    });
  });

  const availableEngineNames = new Set(
    engines.filter((engine: any) => engine.Available !== false).map((engine: any) => engine.Name),
  );
  const available = new Set<string>();
  engines.forEach((engine: any) => {
    (engine.FileTypes || []).forEach((rawType: string) => {
      const fileType = String(rawType || "").trim().toLowerCase();
      if (!fileType || available.has(fileType)) return;
      const explicitEngine = ruleMap.get(fileType);
      if (explicitEngine) {
        if (availableEngineNames.has(explicitEngine)) available.add(fileType);
      } else if (engine.Available !== false) {
        available.add(fileType);
      }
    });
  });
  return available;
});
const acceptFileTypes = computed(() =>
  [...supportedFileTypes.value].map((fileType) => `.${fileType}`).join(","),
);
const returnTo = computed(() => {
  const raw = Array.isArray(route.query.returnTo) ? route.query.returnTo[0] : route.query.returnTo;
  if (typeof raw === "string" && (raw === "/chat" || raw.startsWith("/chat/"))) return raw;
  return "/chat";
});

const kbDocumentCount = (kb: MobileKnowledgeBase) =>
  Number(kb.type === "faq" ? kb.chunk_count || 0 : kb.document_count || kb.knowledge_count || 0);

const documentDisplayName = (item: KnowledgeFileRow) =>
  item.file_name || item.title || item.source || "未命名文档";

const documentKnowledgeBaseName = (item: KnowledgeFileRow) =>
  item.knowledge_base_name ||
  knowledgeBaseById.value.get(String(item.knowledge_base_id || ""))?.name ||
  "未知知识库";

const routeQueryString = (key: string) => {
  const raw = route.query[key];
  const value = Array.isArray(raw) ? raw[0] : raw;
  return typeof value === "string" ? value : "";
};

const backButtonLabel = computed(() => {
  if (selectedKbId.value && routeQueryString("from_search") === "1") return "返回搜索结果";
  if (selectedKbId.value || searchOpen.value) return "返回知识库列表";
  return "返回设置";
});

const backToSettings = () => {
  router.push({
    path: "/settings",
    query: { returnTo: returnTo.value },
  });
};

const clearPolling = () => {
  if (pollTimer) {
    clearTimeout(pollTimer);
    pollTimer = null;
  }
};

const cancelSearchRequests = () => {
  if (searchTimer) {
    clearTimeout(searchTimer);
    searchTimer = null;
  }
  searchRequestGeneration += 1;
  documentSearchLoading.value = false;
  documentSearchLoadingMore.value = false;
};

const clearSearchState = () => {
  cancelSearchRequests();
  searchQuery.value = "";
  documentSearchResults.value = [];
  documentSearchHasMore.value = false;
  documentSearchError.value = "";
};

const openSearch = async () => {
  searchOpen.value = true;
  await nextTick();
  searchInputRef.value?.focus();
};

const closeSearch = () => {
  searchOpen.value = false;
  clearSearchState();
};

const searchReturnQuery = () => searchOpen.value
  ? {
      from_search: "1",
      search_query: searchQuery.value,
      personal_expanded: personalExpanded.value ? "1" : "0",
      shared_expanded: sharedExpanded.value ? "1" : "0",
    }
  : {};

const restoreSearchInCurrentPage = async () => {
  clearPolling();
  selectedKbId.value = "";
  fileList.value = [];
  detailFilterKeyword.value = "";
  detailFocusDocumentId.value = "";
  clearSearchState();
  personalExpanded.value = routeQueryString("personal_expanded") !== "0";
  sharedExpanded.value = routeQueryString("shared_expanded") !== "0";
  searchOpen.value = true;
  searchQuery.value = routeQueryString("search_query");
  await router.replace({
    name: "mobile-knowledge",
    query: {
      returnTo: returnTo.value,
      search: "1",
      q: searchQuery.value,
      personal_expanded: personalExpanded.value ? "1" : "0",
      shared_expanded: sharedExpanded.value ? "1" : "0",
    },
  });
  await nextTick();
  searchInputRef.value?.focus();
};

const returnToPreservedSearch = () => {
  window.close();
  window.setTimeout(() => {
    if (!window.closed) void restoreSearchInCurrentPage();
  }, 150);
};

const handleBack = () => {
  if (searchOpen.value) {
    closeSearch();
    return;
  }
  if (selectedKbId.value) {
    if (routeQueryString("from_search") === "1") {
      returnToPreservedSearch();
      return;
    }
    clearPolling();
    selectedKbId.value = "";
    fileList.value = [];
    detailFilterKeyword.value = "";
    detailFocusDocumentId.value = "";
    if (routeQueryString("kb")) {
      void router.replace({
        name: "mobile-knowledge",
        query: { returnTo: returnTo.value },
      });
    }
    return;
  }
  backToSettings();
};

const loadKnowledgeBases = async (force = false) => {
  loadingKbs.value = true;
  try {
    const [res] = await Promise.all([
      listKnowledgeBases(),
      organizationStore.fetchSharedKnowledgeBases({ force }),
    ]);
    localKnowledgeBases.value = Array.isArray((res as any)?.data) ? (res as any).data : [];
    if (selectedKbId.value && !kbList.value.some((item) => item.id === selectedKbId.value)) {
      selectedKbId.value = "";
      fileList.value = [];
    }
  } catch (error: any) {
    MessagePlugin.error(error?.message || "加载知识库失败");
  } finally {
    loadingKbs.value = false;
  }
};

const schedulePolling = () => {
  clearPolling();
  if (!hasRunningParse.value) return;
  pollTimer = setTimeout(refreshRunningStatuses, 2500);
};

const loadFiles = async () => {
  if (!selectedKbId.value) {
    fileList.value = [];
    return;
  }
  loadingFiles.value = true;
  try {
    const res: any = await listKnowledgeFiles(selectedKbId.value, {
      page: 1,
      page_size: 80,
      keyword: detailFilterKeyword.value || undefined,
    });
    fileList.value = (Array.isArray(res?.data) ? res.data : []).map((item: any) => ({
      ...item,
      display_name: documentDisplayName(item),
    }));
    schedulePolling();
  } catch (error: any) {
    MessagePlugin.error(error?.message || "加载文档失败");
  } finally {
    loadingFiles.value = false;
  }
};

async function refreshRunningStatuses() {
  clearPolling();
  const running = fileList.value.filter((item) => isParseInFlight(item.parse_status));
  if (!running.length) return;
  const query = running.map((item) => `ids=${encodeURIComponent(item.id)}`).join("&");
  try {
    const res: any = await batchQueryKnowledge(query);
    if (res?.success && Array.isArray(res.data)) {
      res.data.forEach((next: any) => {
        const current = fileList.value.find((item) => item.id === next.id);
        if (!current) return;
        current.parse_status = next.parse_status;
        current.summary_status = next.summary_status;
        current.description = next.description;
        current.error_message = next.error_message;
      });
    }
  } finally {
    schedulePolling();
  }
}

const openKnowledgeBase = (kb: MobileKnowledgeBase) => {
  openRouteInNewPage(router, {
    name: "mobile-knowledge",
    query: {
      returnTo: returnTo.value,
      kb: kb.id,
      ...searchReturnQuery(),
    },
  });
};

const openSearchDocument = (item: KnowledgeFileRow) => {
  const kbId = String(item.knowledge_base_id || "");
  if (!kbId || !knowledgeBaseById.value.has(kbId)) {
    MessagePlugin.warning("该文档所属知识库已不可用，请刷新后重试");
    return;
  }
  openRouteInNewPage(router, {
    name: "mobile-knowledge",
    query: {
      returnTo: returnTo.value,
      kb: kbId,
      knowledge_id: String(item.id || ""),
      document_name: documentDisplayName(item),
      ...searchReturnQuery(),
    },
  });
};

const applyRouteDetailTarget = () => {
  if (routeQueryString("search") === "1") {
    personalExpanded.value = routeQueryString("personal_expanded") !== "0";
    sharedExpanded.value = routeQueryString("shared_expanded") !== "0";
    searchOpen.value = true;
    searchQuery.value = routeQueryString("q");
    void nextTick(() => searchInputRef.value?.focus());
    return;
  }
  const kbId = routeQueryString("kb");
  if (!kbId) return;
  if (!knowledgeBaseById.value.has(kbId)) {
    MessagePlugin.warning("该知识库已不可用或无权访问");
    return;
  }
  searchOpen.value = false;
  detailFilterKeyword.value = routeQueryString("document_name");
  detailFocusDocumentId.value = routeQueryString("knowledge_id");
  selectedKbId.value = kbId;
};

const clearDetailFilter = async () => {
  detailFilterKeyword.value = "";
  detailFocusDocumentId.value = "";
  await loadFiles();
};

const runDocumentSearch = async (
  keyword: string,
  offset: number,
  append: boolean,
  generation: number,
) => {
  if (append) documentSearchLoadingMore.value = true;
  else documentSearchLoading.value = true;
  documentSearchError.value = "";
  try {
    const res: any = await searchKnowledge(
      keyword,
      offset,
      DOCUMENT_SEARCH_PAGE_SIZE,
      undefined,
      { include_total: false },
    );
    if (generation !== searchRequestGeneration || keyword !== normalizedSearchQuery.value) return;
    const rows = (Array.isArray(res?.data) ? res.data : []).map((item: any) => ({
      ...item,
      display_name: documentDisplayName(item),
    }));
    if (append) {
      const merged = new Map(documentSearchResults.value.map((item) => [String(item.id), item]));
      rows.forEach((item: KnowledgeFileRow) => merged.set(String(item.id), item));
      documentSearchResults.value = [...merged.values()];
    } else {
      documentSearchResults.value = rows;
    }
    documentSearchHasMore.value = res?.has_more === true;
  } catch (error: any) {
    if (generation !== searchRequestGeneration) return;
    documentSearchError.value = error?.message || "搜索文档失败";
  } finally {
    if (generation === searchRequestGeneration) {
      documentSearchLoading.value = false;
      documentSearchLoadingMore.value = false;
    }
  }
};

const scheduleDocumentSearch = () => {
  cancelSearchRequests();
  documentSearchResults.value = [];
  documentSearchHasMore.value = false;
  documentSearchError.value = "";
  const keyword = normalizedSearchQuery.value;
  if (keyword.length < DOCUMENT_SEARCH_MIN_LENGTH) return;
  const generation = searchRequestGeneration;
  searchTimer = setTimeout(() => {
    searchTimer = null;
    void runDocumentSearch(keyword, 0, false, generation);
  }, DOCUMENT_SEARCH_DEBOUNCE_MS);
};

const loadMoreDocumentResults = async () => {
  const keyword = normalizedSearchQuery.value;
  if (
    keyword.length < DOCUMENT_SEARCH_MIN_LENGTH ||
    documentSearchLoading.value ||
    documentSearchLoadingMore.value ||
    !documentSearchHasMore.value
  ) return;
  const generation = ++searchRequestGeneration;
  await runDocumentSearch(keyword, documentSearchResults.value.length, true, generation);
};

const handleUpload = async (event: Event) => {
  const files = Array.from((event.target as HTMLInputElement).files || []);
  (event.target as HTMLInputElement).value = "";
  if (!files.length || !selectedKbId.value) return;
  if (!selectedCanEdit.value) {
    MessagePlugin.warning("你对该知识库只有查看权限");
    return;
  }
  const { validFiles, skippedCount, videoFilteredCount, hiddenFileCount } = filterUploadFiles(files, {
    supportedFileTypes: supportedFileTypes.value,
    multiFile: files.length > 1,
  });
  const filteredCount = skippedCount + videoFilteredCount + hiddenFileCount;
  if (filteredCount > 0) {
    MessagePlugin.warning(
      validFiles.length
        ? `已过滤 ${filteredCount} 个不支持的文件`
        : "选中的文件均不支持",
    );
  }
  if (!validFiles.length) return;

  uploading.value = true;
  try {
    for (const file of validFiles) {
      await uploadKnowledgeFile(selectedKbId.value, { file });
    }
    MessagePlugin.success("上传已提交");
    await loadFiles();
  } catch (error: any) {
    MessagePlugin.error(error?.message || "上传失败");
  } finally {
    uploading.value = false;
  }
};

const downloadFile = async (item: KnowledgeFileRow) => {
  if (!item?.id || busyMap[item.id]) return;
  busyMap[item.id] = "downloading";
  try {
    const blob = await downKnowledgeDetails(item.id);
    downloadBlob(blob, item.original_file_name || item.file_name || item.title || "knowledge-file");
  } catch (error: any) {
    MessagePlugin.error(error?.message || "下载失败");
  } finally {
    delete busyMap[item.id];
  }
};

const deleteFile = async (item: KnowledgeFileRow) => {
  if (!selectedCanEdit.value) {
    MessagePlugin.warning("你对该知识库只有查看权限");
    return;
  }
  if (!item?.id || busyMap[item.id]) return;
  const ok = window.confirm(`确定删除「${item.display_name || item.file_name || "该文档"}」？`);
  if (!ok) return;
  busyMap[item.id] = "deleting";
  try {
    const res: any = await delKnowledgeDetails(item.id);
    if (res?.success === false) throw new Error(res.message || "删除失败");
    MessagePlugin.success("已删除");
    fileList.value = fileList.value.filter((file) => file.id !== item.id);
  } catch (error: any) {
    MessagePlugin.error(error?.message || "删除失败");
  } finally {
    delete busyMap[item.id];
  }
};

watch(selectedKbId, () => {
  clearPolling();
  void loadFiles();
});

watch(searchQuery, scheduleDocumentSearch);

onMounted(async () => {
  await Promise.all([
    editorResources.ensureParserEngines().catch(() => undefined),
    loadKnowledgeBases(),
  ]);
  applyRouteDetailTarget();
});

onBeforeUnmount(() => {
  clearPolling();
  cancelSearchRequests();
});
</script>

<template>
  <main class="mobile-kb">
    <header class="kb-topbar">
      <button type="button" class="icon-button" :aria-label="backButtonLabel" @click="handleBack">
        <MobileIcon name="chevron-left" />
      </button>
      <strong>{{ topbarTitle }}</strong>
      <button v-if="selectedKbId" type="button" class="icon-button" aria-label="刷新文档" @click="loadFiles">
        <MobileIcon name="refresh" />
      </button>
      <button v-else-if="!searchOpen" type="button" class="icon-button" aria-label="搜索知识库和文档" data-testid="mobile-kb-search-button" @click="openSearch">
        <MobileIcon name="search" />
      </button>
      <span v-else class="topbar-placeholder" />
    </header>

    <section v-if="searchOpen" class="search-view" data-testid="mobile-kb-search-view">
      <div class="search-field">
        <MobileIcon name="search" />
        <input
          ref="searchInputRef"
          v-model="searchQuery"
          type="search"
          inputmode="search"
          autocomplete="off"
          aria-label="搜索知识库及文档"
          placeholder="搜索知识库及文档"
        >
        <button v-if="searchQuery" type="button" aria-label="清空搜索" @click="searchQuery = ''">
          <MobileIcon name="close" />
        </button>
      </div>

      <div v-if="!normalizedSearchQuery" class="search-guide">
        <MobileIcon name="file-search" />
        <strong>按名称查找知识库和文档</strong>
        <span>文档搜索至少输入 {{ DOCUMENT_SEARCH_MIN_LENGTH }} 个字符，结果按页加载。</span>
      </div>

      <template v-else>
        <section class="result-section">
          <div class="result-heading">
            <strong>知识库</strong>
            <span>{{ matchedKnowledgeBases.length }}</span>
          </div>
          <div v-if="matchedKnowledgeBases.length" class="kb-list compact">
            <button
              v-for="kb in matchedKnowledgeBases"
              :key="kb.id"
              type="button"
              class="kb-row"
              @click="openKnowledgeBase(kb)"
            >
              <span class="kb-row-icon"><MobileIcon name="folder" /></span>
              <span class="kb-row-main">
                <strong>{{ kb.name }}</strong>
                <small>{{ kb.group === 'personal' ? '个人知识库' : kb.originLabel }} · {{ kbDocumentCount(kb) }} 个文档</small>
              </span>
              <em class="permission-badge" :class="`is-${kb.access}`">{{ kb.permissionLabel }}</em>
              <MobileIcon name="chevron-right" />
            </button>
          </div>
          <div v-else class="result-empty">没有名称匹配的知识库</div>
        </section>

        <section class="result-section">
          <div class="result-heading">
            <strong>文档</strong>
            <span v-if="visibleDocumentSearchResults.length">已显示 {{ visibleDocumentSearchResults.length }}</span>
          </div>
          <div v-if="normalizedSearchQuery.length < DOCUMENT_SEARCH_MIN_LENGTH" class="result-empty">
            再输入 {{ DOCUMENT_SEARCH_MIN_LENGTH - normalizedSearchQuery.length }} 个字符后搜索文档
          </div>
          <div v-else-if="documentSearchLoading" class="result-empty">正在搜索文档</div>
          <div v-else-if="documentSearchError" class="result-empty is-error">{{ documentSearchError }}</div>
          <div v-else-if="!visibleDocumentSearchResults.length" class="result-empty">没有匹配的文档</div>
          <div v-else class="document-results">
            <button
              v-for="item in visibleDocumentSearchResults"
              :key="item.id"
              type="button"
              class="document-result-row"
              @click="openSearchDocument(item)"
            >
              <span class="doc-icon"><MobileIcon name="file" /></span>
              <span>
                <strong>{{ item.display_name }}</strong>
                <small>{{ documentKnowledgeBaseName(item) }}</small>
              </span>
              <MobileIcon name="chevron-right" />
            </button>
            <button
              v-if="documentSearchHasMore"
              type="button"
              class="load-more"
              :disabled="documentSearchLoadingMore"
              @click="loadMoreDocumentResults"
            >
              {{ documentSearchLoadingMore ? '正在加载' : '加载更多文档' }}
            </button>
          </div>
        </section>
      </template>
    </section>

    <section v-else-if="!selectedKb" class="catalog-view" data-testid="mobile-kb-catalog">
      <div v-if="loadingKbs" class="catalog-loading">正在加载知识库</div>
      <template v-else>
        <section class="kb-group">
          <button type="button" class="group-heading" :aria-expanded="personalExpanded" @click="personalExpanded = !personalExpanded">
            <span>个人知识库</span>
            <small>{{ personalKnowledgeBases.length }}</small>
            <MobileIcon :name="personalExpanded ? 'chevron-up' : 'chevron-down'" />
          </button>
          <div v-if="personalExpanded" class="kb-list">
            <button
              v-for="kb in personalKnowledgeBases"
              :key="kb.id"
              type="button"
              class="kb-row"
              :data-testid="`mobile-kb-row-${kb.id}`"
              @click="openKnowledgeBase(kb)"
            >
              <span class="kb-row-icon"><MobileIcon name="folder" /></span>
              <span class="kb-row-main">
                <strong>{{ kb.name }}</strong>
                <small>{{ kbDocumentCount(kb) }} 个文档 · {{ kb.originLabel }}</small>
              </span>
              <em class="permission-badge is-manage">可管理</em>
              <MobileIcon name="chevron-right" />
            </button>
            <div v-if="!personalKnowledgeBases.length" class="group-empty">暂无个人知识库</div>
          </div>
        </section>

        <section class="kb-group">
          <button type="button" class="group-heading" :aria-expanded="sharedExpanded" @click="sharedExpanded = !sharedExpanded">
            <span>共享知识库</span>
            <small>{{ sharedKnowledgeBases.length }}</small>
            <MobileIcon :name="sharedExpanded ? 'chevron-up' : 'chevron-down'" />
          </button>
          <div v-if="sharedExpanded" class="kb-list">
            <button
              v-for="kb in sharedKnowledgeBases"
              :key="kb.id"
              type="button"
              class="kb-row"
              :data-testid="`mobile-kb-row-${kb.id}`"
              @click="openKnowledgeBase(kb)"
            >
              <span class="kb-row-icon shared"><MobileIcon name="folder" /></span>
              <span class="kb-row-main">
                <strong>{{ kb.name }}</strong>
                <small>{{ kbDocumentCount(kb) }} 个文档 · {{ kb.originLabel }}</small>
              </span>
              <em class="permission-badge" :class="`is-${kb.access}`">{{ kb.permissionLabel }}</em>
              <MobileIcon name="chevron-right" />
            </button>
            <div v-if="!sharedKnowledgeBases.length" class="group-empty">暂无共享知识库</div>
          </div>
        </section>
      </template>
    </section>

    <template v-else>
      <section v-if="selectedCanEdit" class="upload-card">
        <div>
          <strong>{{ selectedKb.name }}</strong>
          <span>{{ selectedKb.permissionLabel }}，可上传和删除文档</span>
        </div>
        <button type="button" :disabled="uploading" @click="uploadInputRef?.click()">
          <span v-if="uploading" class="busy-icon upload" aria-label="正在上传">
            <MobileIcon name="upload" />
          </span>
          <MobileIcon v-else name="upload" />
          <span>上传文档</span>
        </button>
        <input ref="uploadInputRef" type="file" :accept="acceptFileTypes" multiple hidden @change="handleUpload">
      </section>
      <section v-else class="readonly-card" data-testid="mobile-kb-readonly-notice">
        <MobileIcon name="lock" />
        <div>
          <strong>仅查看</strong>
          <span>你可以查看和下载文档，不能上传或删除。</span>
        </div>
      </section>

      <section class="doc-section">
        <div class="section-title-row">
          <span>文档</span>
          <em class="permission-badge" :class="`is-${selectedKb.access}`">{{ selectedKb.permissionLabel }}</em>
        </div>
        <div v-if="detailFilterKeyword" class="detail-filter">
          <span>搜索定位：{{ detailFilterKeyword }}</span>
          <button type="button" @click="clearDetailFilter">查看全部</button>
        </div>
        <div v-if="loadingFiles" class="empty-state">正在加载文档</div>
        <div v-else-if="!fileList.length" class="empty-state">
          {{ detailFilterKeyword ? '未找到定位文档，可查看全部文档' : '暂无文档' }}
        </div>
        <div v-else class="doc-list">
          <article
            v-for="item in fileList"
            :key="item.id"
            class="doc-row"
            :class="{ focused: String(item.id) === detailFocusDocumentId }"
          >
            <div class="doc-icon">
              <MobileIcon name="file" />
            </div>
            <div class="doc-main">
              <strong>{{ item.display_name || item.file_name }}</strong>
              <span>
                {{ item.file_type || item.type || 'FILE' }}
                <template v-if="item.file_size"> · {{ formatFileSize(item.file_size) }}</template>
              </span>
              <em class="parse-status" :class="parseStatusClass(item.parse_status)">
                {{ parseStatusText(item.parse_status, item.summary_status) }}
              </em>
            </div>
            <div class="doc-actions">
              <button type="button" :disabled="!!busyMap[item.id]" @click="downloadFile(item)">
                <span v-if="busyMap[item.id] === 'downloading'" class="busy-icon download" aria-label="正在下载">
                  <MobileIcon name="download" />
                </span>
                <MobileIcon v-else name="download" />
                <span>下载</span>
              </button>
              <button
                v-if="selectedCanEdit"
                type="button"
                class="danger"
                :disabled="!!busyMap[item.id]"
                @click="deleteFile(item)"
              >
                <span v-if="busyMap[item.id] === 'deleting'" class="busy-icon delete" aria-label="正在删除">
                  <MobileIcon name="delete" />
                </span>
                <MobileIcon v-else name="delete" />
                <span>删除</span>
              </button>
            </div>
          </article>
        </div>
      </section>
    </template>
  </main>
</template>

<style scoped>
.mobile-kb {
  min-height: 100dvh;
  background: #f4f6f5;
  color: #18251f;
  padding-bottom: calc(env(safe-area-inset-bottom) + 18px);
}

.kb-topbar {
  display: grid;
  grid-template-columns: 42px minmax(0, 1fr) 42px;
  align-items: center;
  min-height: 54px;
  border-bottom: 1px solid #edf1ef;
  background: #fff;
  padding: calc(env(safe-area-inset-top) + 7px) 12px 7px;
}

.kb-topbar strong {
  overflow: hidden;
  font-size: 18px;
  text-align: center;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.icon-button {
  display: grid;
  width: 38px;
  height: 38px;
  place-items: center;
  border: 0;
  border-radius: 50%;
  background: transparent;
  color: #1f2f28;
  padding: 0;
  font-size: 23px;
}

.icon-button:active {
  background: #eef3f0;
}

.topbar-placeholder {
  width: 38px;
  height: 38px;
}

.catalog-view {
  padding-top: 10px;
}

.catalog-loading,
.group-empty,
.result-empty,
.empty-state {
  padding: 24px 14px;
  color: #788982;
  font-size: 14px;
  text-align: center;
}

.kb-group {
  margin-bottom: 10px;
  border-top: 1px solid #edf1ef;
  border-bottom: 1px solid #e7ece9;
  background: #fff;
}

.group-heading {
  display: grid;
  width: 100%;
  min-height: 62px;
  grid-template-columns: auto auto 1fr 20px;
  align-items: center;
  gap: 8px;
  border: 0;
  background: #fff;
  color: #18251f;
  padding: 0 17px;
  text-align: left;
}

.group-heading span {
  font-size: 18px;
  font-weight: 650;
}

.group-heading small {
  display: inline-grid;
  min-width: 22px;
  height: 22px;
  place-items: center;
  border-radius: 11px;
  background: #eef3f0;
  color: #6e8077;
  padding: 0 6px;
  font-size: 12px;
}

.group-heading > :last-child {
  grid-column: 4;
  color: #7c8b84;
}

.kb-list {
  border-top: 1px solid #edf1ef;
}

.kb-list.compact {
  border-top: 0;
}

.kb-row {
  display: grid;
  width: 100%;
  min-height: 68px;
  grid-template-columns: 38px minmax(0, 1fr) auto 18px;
  align-items: center;
  gap: 10px;
  border: 0;
  border-bottom: 1px solid #eff3f1;
  background: #fff;
  color: #203129;
  padding: 9px 15px 9px 18px;
  text-align: left;
}

.kb-row:last-child {
  border-bottom: 0;
}

.kb-row:active,
.document-result-row:active {
  background: #f2f7f4;
}

.kb-row-icon {
  display: grid;
  width: 36px;
  height: 36px;
  place-items: center;
  border-radius: 10px;
  background: #e9f7ef;
  color: #079b4e;
  font-size: 20px;
}

.kb-row-icon.shared {
  background: #edf3ff;
  color: #4977c8;
}

.kb-row-main {
  display: flex;
  min-width: 0;
  flex-direction: column;
  gap: 5px;
}

.kb-row-main strong,
.kb-row-main small {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.kb-row-main strong {
  color: #17251e;
  font-size: 15px;
  font-weight: 650;
}

.kb-row-main small {
  color: #7b8b83;
  font-size: 12px;
}

.permission-badge {
  border-radius: 999px;
  font-size: 11px;
  font-style: normal;
  font-weight: 650;
  padding: 4px 8px;
  white-space: nowrap;
}

.permission-badge.is-manage {
  background: #e8f8ef;
  color: #078f49;
}

.permission-badge.is-edit {
  background: #fff4dd;
  color: #9b6509;
}

.permission-badge.is-view {
  background: #eef1f0;
  color: #6b7b73;
}

.search-view {
  padding: 12px;
}

.search-field {
  display: grid;
  height: 46px;
  grid-template-columns: 22px minmax(0, 1fr) 30px;
  align-items: center;
  gap: 8px;
  border: 1px solid #d8e5de;
  border-radius: 12px;
  background: #fff;
  color: #71827a;
  padding: 0 9px 0 13px;
  font-size: 19px;
}

.search-field:focus-within {
  border-color: #75d39e;
  box-shadow: 0 0 0 3px rgb(7 193 96 / 8%);
}

.search-field input {
  width: 100%;
  border: 0;
  outline: 0;
  background: transparent;
  color: #17251e;
  font-size: 16px;
}

.search-field button {
  display: grid;
  width: 28px;
  height: 28px;
  place-items: center;
  border: 0;
  border-radius: 50%;
  background: #eef2f0;
  color: #77877f;
  padding: 6px;
}

.search-guide {
  display: flex;
  align-items: center;
  flex-direction: column;
  gap: 8px;
  color: #7a8b82;
  padding: 84px 22px 20px;
  text-align: center;
}

.search-guide > :first-child {
  color: #9aaba3;
  font-size: 42px;
}

.search-guide strong {
  color: #41534a;
  font-size: 15px;
}

.search-guide span {
  font-size: 13px;
  line-height: 1.6;
}

.result-section {
  overflow: hidden;
  margin-top: 12px;
  border: 1px solid #e1e9e5;
  border-radius: 10px;
  background: #fff;
}

.result-heading,
.section-title-row {
  display: flex;
  min-height: 43px;
  align-items: center;
  justify-content: space-between;
  border-bottom: 1px solid #edf1ef;
  padding: 0 13px;
}

.result-heading strong,
.section-title-row > span {
  color: #34463d;
  font-size: 14px;
  font-weight: 650;
}

.result-heading span {
  color: #84938c;
  font-size: 12px;
}

.result-empty.is-error {
  color: #bf3636;
}

.document-results {
  display: flex;
  flex-direction: column;
}

.document-result-row {
  display: grid;
  width: 100%;
  min-height: 64px;
  grid-template-columns: 34px minmax(0, 1fr) 18px;
  align-items: center;
  gap: 10px;
  border: 0;
  border-bottom: 1px solid #eff3f1;
  background: #fff;
  color: #6f8077;
  padding: 8px 12px;
  text-align: left;
}

.document-result-row > span:nth-child(2) {
  display: flex;
  min-width: 0;
  flex-direction: column;
  gap: 5px;
}

.document-result-row strong,
.document-result-row small {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.document-result-row strong {
  color: #1d2d25;
  font-size: 14px;
}

.document-result-row small {
  color: #7f8e87;
  font-size: 12px;
}

.load-more {
  min-height: 44px;
  border: 0;
  background: #fff;
  color: #078f49;
  font-size: 13px;
  font-weight: 650;
}

.load-more:disabled {
  color: #8ea099;
}

.upload-card,
.readonly-card,
.doc-section {
  margin: 12px;
  border: 1px solid #dfe9e4;
  border-radius: 10px;
  background: #fff;
}

.upload-card {
  display: grid;
  grid-template-columns: minmax(0, 1fr) auto;
  align-items: center;
  gap: 10px;
  padding: 13px;
}

.upload-card div,
.readonly-card div {
  display: flex;
  min-width: 0;
  flex-direction: column;
  gap: 4px;
}

.upload-card strong,
.upload-card span,
.readonly-card strong,
.readonly-card span {
  overflow: hidden;
  text-overflow: ellipsis;
}

.upload-card strong,
.readonly-card strong {
  color: #17261f;
  font-size: 15px;
}

.upload-card div > span,
.readonly-card span {
  color: #788982;
  font-size: 12px;
  line-height: 1.45;
}

.upload-card button {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  height: 36px;
  border: 0;
  border-radius: 18px;
  background: #07c160;
  color: #fff;
  padding: 0 13px;
  font-weight: 650;
}

.upload-card button:disabled {
  background: #c7d6cf;
}

.readonly-card {
  display: grid;
  grid-template-columns: 34px minmax(0, 1fr);
  align-items: center;
  gap: 9px;
  background: #f8faf9;
  color: #788982;
  padding: 13px;
}

.readonly-card > :first-child {
  color: #7a8b83;
  font-size: 24px;
}

.doc-section {
  padding-bottom: 6px;
}

.detail-filter {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
  border-bottom: 1px solid #e8eeeb;
  background: #f4fbf7;
  color: #52665c;
  padding: 9px 12px;
  font-size: 12px;
}

.detail-filter span {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.detail-filter button {
  flex: 0 0 auto;
  border: 0;
  background: transparent;
  color: #078f49;
  font-weight: 650;
}

.doc-list {
  display: flex;
  flex-direction: column;
  gap: 6px;
  padding: 8px;
}

.doc-row {
  display: grid;
  grid-template-columns: 34px minmax(0, 1fr);
  gap: 9px;
  border: 1px solid transparent;
  border-radius: 8px;
  background: #f8fbf9;
  padding: 10px;
}

.doc-row.focused {
  border-color: #74d29c;
  background: #effaf4;
  box-shadow: 0 0 0 2px rgb(7 193 96 / 8%);
}

.doc-icon {
  display: grid;
  width: 32px;
  height: 32px;
  place-items: center;
  border-radius: 8px;
  background: #fff4e7;
  color: #b56d13;
}

.doc-main {
  display: flex;
  min-width: 0;
  flex-direction: column;
  gap: 4px;
}

.doc-main strong,
.doc-main span {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.doc-main strong {
  color: #1c2d25;
  font-size: 15px;
}

.doc-main span {
  color: #788982;
  font-size: 13px;
}

.parse-status {
  width: max-content;
  border-radius: 999px;
  font-size: 12px;
  font-style: normal;
  padding: 3px 7px;
}

.parse-status.is-running {
  background: #fff8e9;
  color: #a06408;
}

.parse-status.is-completed {
  background: #edf9f2;
  color: #078f49;
}

.parse-status.is-failed {
  background: #fff0f0;
  color: #bf3636;
}

.parse-status.is-muted {
  background: #eef2f0;
  color: #71827a;
}

.doc-actions {
  display: flex;
  grid-column: 2;
  gap: 8px;
  padding-top: 2px;
}

.doc-actions button {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  gap: 4px;
  height: 30px;
  border: 1px solid #bfe8cf;
  border-radius: 15px;
  background: #fff;
  color: #078f49;
  padding: 0 12px;
  font-size: 13px;
  font-weight: 650;
}

.doc-actions button.danger {
  border-color: #f0c1c1;
  color: #bf3636;
}

.doc-actions button:disabled {
  opacity: 0.62;
}

.busy-icon {
  display: inline-grid;
  width: 14px;
  height: 14px;
  place-items: center;
  font-size: 14px;
}

.busy-icon.upload {
  animation: mobileUploadFloat 0.82s ease-in-out infinite;
}

.busy-icon.download {
  animation: mobileDownloadFloat 0.82s ease-in-out infinite;
}

.busy-icon.delete {
  animation: mobileDeleteSpin 0.92s ease-in-out infinite;
}

@keyframes mobileUploadFloat {
  0%,
  100% { opacity: 0.55; transform: translateY(2px); }
  50% { opacity: 1; transform: translateY(-2px); }
}

@keyframes mobileDownloadFloat {
  0%,
  100% { opacity: 0.55; transform: translateY(-2px); }
  50% { opacity: 1; transform: translateY(2px); }
}

@keyframes mobileDeleteSpin {
  0% { opacity: 0.65; transform: rotate(0deg) scale(0.92); }
  50% { opacity: 1; transform: rotate(12deg) scale(1.04); }
  100% { opacity: 0.65; transform: rotate(-12deg) scale(0.92); }
}
</style>
