<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, reactive, ref, watch } from "vue";
import { useRoute, useRouter } from "vue-router";
import { MessagePlugin } from "tdesign-vue-next";
import {
  batchQueryKnowledge,
  delKnowledgeDetails,
  downKnowledgeDetails,
  listKnowledgeBases,
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
import { useSearchFolderBrowser } from "@/custom/modules/knowledgeSearch/useSearchFolderBrowser";
import {
  downloadBlob,
  formatFileSize,
} from "../utils";
import {
  knowledgeHasDerivativeFailure,
  knowledgeIsFullyComplete,
  knowledgeNeedsStatusPolling,
} from "@/views/knowledge/wikiStatusRefresh";
import {
  createKnowledgeFolder,
  deleteKnowledgeFolder,
  listKnowledgeFolderNodes,
  listKnowledgeFolderOptions,
  moveKnowledgeDocumentsToFolder,
  searchAccessibleKnowledgeFolderNodes,
  searchKnowledgeFolderNodes,
  updateKnowledgeFolder,
  uploadKnowledgeFolderFile,
} from "@/custom/modules/knowledgeFolders/api";
import type {
  KnowledgeFolder,
  KnowledgeFolderBreadcrumb,
  KnowledgeFolderNode,
  KnowledgeFolderOption,
} from "@/custom/modules/knowledgeFolders/types";

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
const folderList = ref<KnowledgeFolder[]>([]);
const folderBreadcrumbs = ref<KnowledgeFolderBreadcrumb[]>([]);
const currentFolderId = ref("");
const nodePage = ref(1);
const nodePageSize = 20;
const nodeTotal = ref(0);
const folderOptions = ref<KnowledgeFolderOption[]>([]);
const selectedKbId = ref("");
const loadingKbs = ref(false);
const loadingFiles = ref(false);
const uploadInputRef = ref<HTMLInputElement | null>(null);
const folderUploadInputRef = ref<HTMLInputElement | null>(null);
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
const {
  originKey: expandedSearchFolderOriginKey,
  knowledgeBaseId: expandedSearchFolderKnowledgeBaseId,
  currentFolder: expandedSearchFolderCurrent,
  breadcrumbs: expandedSearchFolderBreadcrumbs,
  nodes: expandedSearchFolderNodes,
  page: expandedSearchFolderPage,
  total: expandedSearchFolderTotal,
  totalPages: expandedSearchFolderTotalPages,
  loading: expandedSearchFolderLoading,
  error: expandedSearchFolderError,
  collapse: collapseExpandedSearchFolder,
  toggleRoot: toggleExpandedSearchFolderRoot,
  enterFolder: enterExpandedSearchFolder,
  openBreadcrumb: openExpandedSearchFolderBreadcrumb,
  changePage: changeExpandedSearchFolderPage,
} = useSearchFolderBrowser();
const detailFilterKeyword = ref("");
const detailFocusDocumentId = ref("");
const folderEditorVisible = ref(false);
const folderEditorMode = ref<"create" | "edit" | "move">("create");
const folderEditorTarget = ref<KnowledgeFolder | null>(null);
const folderEditorSubmitting = ref(false);
const folderEditorForm = reactive({ name: "", description: "", parent_id: "" });
const documentMoveVisible = ref(false);
const documentMoveTarget = ref<KnowledgeFileRow | null>(null);
const documentMoveFolderId = ref("");
const documentMoveSubmitting = ref(false);

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
const hasRunningParse = computed(() => fileList.value.some((item) => knowledgeNeedsStatusPolling(item)));
const topbarTitle = computed(() =>
  folderBreadcrumbs.value.at(-1)?.name || selectedKb.value?.name || (searchOpen.value ? "搜索" : "知识库"),
);
const normalizedSearchQuery = computed(() => searchQuery.value.trim());
const knowledgeBaseById = computed(() => new Map(kbList.value.map((item) => [item.id, item])));
const matchedKnowledgeBases = computed(() => {
  const keyword = normalizedSearchQuery.value.toLocaleLowerCase();
  if (!keyword) return [];
  return kbList.value.filter((item) => item.name.toLocaleLowerCase().includes(keyword));
});
const visibleDocumentSearchResults = computed(() =>
  documentSearchResults.value.filter((item) =>
    item.node_type !== "folder" && knowledgeBaseById.value.has(String(item.knowledge_base_id || "")),
  ),
);
const visibleFolderSearchResults = computed(() =>
  documentSearchResults.value.filter((item) =>
    item.node_type === "folder" && item.folder && knowledgeBaseById.value.has(String(item.knowledge_base_id || "")),
  ),
);
const folderParentOptions = computed(() => {
  const target = folderEditorTarget.value;
  return [
    { label: "知识库根目录", value: "" },
    ...folderOptions.value
      .filter((option) => !target || (option.id !== target.id && !option.path.startsWith(`${target.path}/`)))
      .map((option) => ({ label: option.path, value: option.id })),
  ];
});
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
  collapseExpandedSearchFolder();
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
    if (currentFolderId.value) {
      const parent = folderBreadcrumbs.value.length > 1
        ? folderBreadcrumbs.value[folderBreadcrumbs.value.length - 2]
        : null;
      currentFolderId.value = parent?.id || "";
      nodePage.value = 1;
      void router.replace({
        name: "mobile-knowledge",
        query: {
          ...route.query,
          folder_id: currentFolderId.value || undefined,
        },
      });
      void loadFiles();
      return;
    }
    if (routeQueryString("from_search") === "1") {
      returnToPreservedSearch();
      return;
    }
    clearPolling();
    selectedKbId.value = "";
    fileList.value = [];
    folderList.value = [];
    folderBreadcrumbs.value = [];
    currentFolderId.value = "";
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
    folderList.value = [];
    nodeTotal.value = 0;
    return;
  }
  loadingFiles.value = true;
  try {
    const params = {
      folder_id: currentFolderId.value || undefined,
      page: nodePage.value,
      page_size: nodePageSize,
      keyword: detailFilterKeyword.value || undefined,
    };
    const [res, optionsRes]: any[] = await Promise.all([
      detailFilterKeyword.value
        ? searchKnowledgeFolderNodes(selectedKbId.value, params)
        : listKnowledgeFolderNodes(selectedKbId.value, params),
      listKnowledgeFolderOptions(selectedKbId.value),
    ]);
    const nodes: KnowledgeFolderNode[] = Array.isArray(res?.data) ? res.data : [];
    folderOptions.value = Array.isArray(optionsRes?.data) ? optionsRes.data : [];
    folderList.value = nodes
      .filter((node) => node.node_type === "folder" && node.folder)
      .map((node) => node.folder as KnowledgeFolder);
    fileList.value = nodes
      .filter((node) => node.node_type === "document" && node.document)
      .map((node) => ({
        ...node.document,
        display_name: documentDisplayName(node.document || {}),
      }));
    nodeTotal.value = Number(res?.total || 0);
    const lastPage = Math.max(1, Math.ceil(nodeTotal.value / nodePageSize));
    if (nodePage.value > lastPage) {
      nodePage.value = lastPage;
      return await loadFiles();
    }
    if (Array.isArray(res?.breadcrumbs)) {
      folderBreadcrumbs.value = res.breadcrumbs;
    } else if (currentFolderId.value) {
      const current = folderOptions.value.find((option) => option.id === currentFolderId.value);
      folderBreadcrumbs.value = current
        ? current.path.split("/").map((name, index, parts) => {
          const path = parts.slice(0, index + 1).join("/");
          const option = folderOptions.value.find((candidate) => candidate.path === path);
          return { id: option?.id || "", name };
        }).filter((item) => item.id)
        : [];
    } else {
      folderBreadcrumbs.value = [];
    }
    schedulePolling();
  } catch (error: any) {
    MessagePlugin.error(error?.message || "加载文档失败");
  } finally {
    loadingFiles.value = false;
  }
};

async function refreshRunningStatuses() {
  clearPolling();
  const running = fileList.value.filter((item) => knowledgeNeedsStatusPolling(item));
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
        current.enrichment_status = next.enrichment_status;
        current.wiki_status = next.wiki_status;
        current.description = next.description;
        current.error_message = next.error_message;
      });
    }
  } finally {
    schedulePolling();
  }
}

function documentStatusText(item: KnowledgeFileRow) {
  if (item.parse_status === "pending") return "待解析";
  if (item.parse_status === "processing") return "解析中";
  if (item.parse_status === "finalizing") return "整理中";
  if (item.parse_status === "failed") return "解析失败";
  if (item.parse_status === "cancelled") return "已取消";
  if (item.parse_status === "draft") return "草稿";
  if (knowledgeHasDerivativeFailure(item)) return "衍生处理失败";
  if (knowledgeNeedsStatusPolling(item)) {
    if (["pending", "processing"].includes(String(item.summary_status || ""))) return "生成摘要中";
    return "优化中";
  }
  if (knowledgeIsFullyComplete(item)) return "已完成";
  return item.parse_status || "未知";
}

function documentStatusClass(item: KnowledgeFileRow) {
  if (knowledgeIsFullyComplete(item)) return "is-completed";
  if (item.parse_status === "failed" || knowledgeHasDerivativeFailure(item)) return "is-failed";
  if (knowledgeNeedsStatusPolling(item)) return "is-running";
  return "is-muted";
}

const openKnowledgeBase = (kb: MobileKnowledgeBase) => {
  void router.push({
    name: "mobile-knowledge",
    query: {
      returnTo: returnTo.value,
      kb: kb.id,
      ...searchReturnQuery(),
    },
  });
};

const openFolder = async (folder: KnowledgeFolder | null) => {
  currentFolderId.value = folder?.id || "";
  nodePage.value = 1;
  detailFilterKeyword.value = "";
  detailFocusDocumentId.value = "";
  await router.replace({
    name: "mobile-knowledge",
    query: {
      ...route.query,
      kb: selectedKbId.value,
      folder_id: currentFolderId.value || undefined,
      knowledge_id: undefined,
      document_name: undefined,
    },
  });
  await loadFiles();
};

const openSearchFolder = (item: KnowledgeFileRow) => {
  const kbID = String(item.knowledge_base_id || "");
  const folder = item.folder as KnowledgeFolder | undefined;
  if (!kbID || !folder || !knowledgeBaseById.value.has(kbID)) {
    MessagePlugin.warning("该文件夹所属知识库已不可用，请刷新后重试");
    return;
  }
  void router.push({
    name: "mobile-knowledge",
    query: {
      returnTo: returnTo.value,
      kb: kbID,
      folder_id: folder.id,
      ...searchReturnQuery(),
    },
  });
};

const toggleSearchFolderResult = async (item: KnowledgeFileRow) => {
  const kbID = String(item.knowledge_base_id || "");
  const folder = item.folder as KnowledgeFolder | undefined;
  if (!kbID || !folder || !knowledgeBaseById.value.has(kbID)) {
    MessagePlugin.warning("该文件夹所属知识库已不可用，请刷新后重试");
    return;
  }
  await toggleExpandedSearchFolderRoot(String(item.id), kbID, folder);
};

const openExpandedSearchFolderLocation = () => {
  const kbID = expandedSearchFolderKnowledgeBaseId.value;
  const folder = expandedSearchFolderCurrent.value;
  if (!kbID || !folder || !knowledgeBaseById.value.has(kbID)) return;
  openSearchFolder({
    knowledge_base_id: kbID,
    folder,
  });
};

const openExpandedSearchDocument = (node: KnowledgeFolderNode) => {
  const document = node.document || {};
  openSearchDocument({
    ...document,
    knowledge_base_id: expandedSearchFolderKnowledgeBaseId.value,
    knowledge_base_name: knowledgeBaseById.value.get(
      expandedSearchFolderKnowledgeBaseId.value,
    )?.name,
  });
};

const openSearchDocument = (item: KnowledgeFileRow) => {
  const kbId = String(item.knowledge_base_id || "");
  if (!kbId || !knowledgeBaseById.value.has(kbId)) {
    MessagePlugin.warning("该文档所属知识库已不可用，请刷新后重试");
    return;
  }
  void router.push({
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
  currentFolderId.value = routeQueryString("folder_id");
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
    const page = Math.floor(offset / DOCUMENT_SEARCH_PAGE_SIZE) + 1;
    const res: any = await searchAccessibleKnowledgeFolderNodes({
      keyword,
      page,
      page_size: DOCUMENT_SEARCH_PAGE_SIZE,
    });
    if (generation !== searchRequestGeneration || keyword !== normalizedSearchQuery.value) return;
    const rows = (Array.isArray(res?.data) ? res.data : []).map((node: any) => {
      if (node?.node_type === "folder" && node.folder) {
        return {
          ...node,
          id: `folder:${node.folder.id}`,
          display_name: node.folder.name,
        };
      }
      const document = node?.document || {};
      return {
        ...document,
        node_type: "document",
        knowledge_base_id: node?.knowledge_base_id || document.knowledge_base_id,
        knowledge_base_name: node?.knowledge_base_name,
        display_name: documentDisplayName(document),
      };
    });
    if (append) {
      const merged = new Map(documentSearchResults.value.map((item) => [String(item.id), item]));
      rows.forEach((item: KnowledgeFileRow) => merged.set(String(item.id), item));
      documentSearchResults.value = [...merged.values()];
    } else {
      documentSearchResults.value = rows;
    }
    documentSearchHasMore.value = page * DOCUMENT_SEARCH_PAGE_SIZE < Number(res?.total || 0);
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
  collapseExpandedSearchFolder();
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
      const relativePath = (file as File & { webkitRelativePath?: string }).webkitRelativePath;
      await uploadKnowledgeFolderFile(selectedKbId.value, {
        file,
        folder_id: currentFolderId.value || undefined,
        relative_path: relativePath || undefined,
      });
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

const refreshFolderOptions = async () => {
  if (!selectedKbId.value) return;
  try {
    const res: any = await listKnowledgeFolderOptions(selectedKbId.value);
    folderOptions.value = Array.isArray(res?.data) ? res.data : [];
  } catch {
    folderOptions.value = [];
  }
};

const openDocumentMove = async (item: KnowledgeFileRow) => {
  await refreshFolderOptions();
  documentMoveTarget.value = item;
  documentMoveFolderId.value = String(item.folder_id || "");
  documentMoveVisible.value = true;
};

const submitDocumentMove = async () => {
  if (!selectedKbId.value || !documentMoveTarget.value) return;
  documentMoveSubmitting.value = true;
  try {
    await moveKnowledgeDocumentsToFolder(
      selectedKbId.value,
      [String(documentMoveTarget.value.id)],
      documentMoveFolderId.value,
    );
    MessagePlugin.success("文档已移动");
    documentMoveVisible.value = false;
    await loadFiles();
  } catch (error: any) {
    MessagePlugin.error(error?.message || "移动文档失败");
  } finally {
    documentMoveSubmitting.value = false;
  }
};

const openFolderEditor = async (
  mode: "create" | "edit" | "move",
  folder: KnowledgeFolder | null = null,
) => {
  await refreshFolderOptions();
  folderEditorMode.value = mode;
  folderEditorTarget.value = folder;
  folderEditorForm.name = folder?.name || "";
  folderEditorForm.description = folder?.description || "";
  folderEditorForm.parent_id = folder?.parent_id ?? currentFolderId.value;
  folderEditorVisible.value = true;
};

const submitFolderEditor = async () => {
  if (!selectedKbId.value) return;
  if (folderEditorMode.value !== "move" && !folderEditorForm.name.trim()) {
    MessagePlugin.warning("请输入文件夹名称");
    return;
  }
  folderEditorSubmitting.value = true;
  try {
    if (folderEditorMode.value === "create") {
      await createKnowledgeFolder(selectedKbId.value, {
        parent_id: folderEditorForm.parent_id,
        name: folderEditorForm.name.trim(),
        description: folderEditorForm.description.trim(),
      });
      MessagePlugin.success("文件夹已创建");
    } else if (folderEditorTarget.value) {
      await updateKnowledgeFolder(
        selectedKbId.value,
        folderEditorTarget.value.id,
        folderEditorMode.value === "move"
          ? { parent_id: folderEditorForm.parent_id }
          : {
            parent_id: folderEditorForm.parent_id,
            name: folderEditorForm.name.trim(),
            description: folderEditorForm.description.trim(),
          },
      );
      MessagePlugin.success(folderEditorMode.value === "move" ? "文件夹已移动" : "文件夹已更新");
    }
    folderEditorVisible.value = false;
    await loadFiles();
  } catch (error: any) {
    MessagePlugin.error(error?.message || "文件夹操作失败");
  } finally {
    folderEditorSubmitting.value = false;
  }
};

const removeFolder = async (folder: KnowledgeFolder) => {
  if (!selectedCanEdit.value || !selectedKbId.value) return;
  const nonEmpty = folder.stats.direct_child_folder_count > 0 || folder.stats.subtree_document_count > 0;
  const message = nonEmpty
    ? `“${folder.name}”中仍有内容，是否将内容移动到上一级后删除？`
    : `确定删除空文件夹“${folder.name}”吗？`;
  if (!window.confirm(message)) return;
  try {
    await deleteKnowledgeFolder(
      selectedKbId.value,
      folder.id,
      nonEmpty ? "move_to_parent" : "reject",
    );
    MessagePlugin.success("文件夹已删除");
    await loadFiles();
  } catch (error: any) {
    MessagePlugin.error(error?.message || "删除文件夹失败");
  }
};

const goToNodePage = async (nextPage: number) => {
  const lastPage = Math.max(1, Math.ceil(nodeTotal.value / nodePageSize));
  nodePage.value = Math.min(lastPage, Math.max(1, nextPage));
  await loadFiles();
};

watch(selectedKbId, () => {
  clearPolling();
  nodePage.value = 1;
  void loadFiles();
});

watch(searchQuery, scheduleDocumentSearch);

watch(
  () => route.fullPath,
  () => applyRouteDetailTarget(),
);

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
            <strong>文件夹</strong>
            <span v-if="visibleFolderSearchResults.length">已显示 {{ visibleFolderSearchResults.length }}</span>
          </div>
          <div v-if="normalizedSearchQuery.length < DOCUMENT_SEARCH_MIN_LENGTH" class="result-empty">
            再输入 {{ DOCUMENT_SEARCH_MIN_LENGTH - normalizedSearchQuery.length }} 个字符后搜索文件夹
          </div>
          <div v-else-if="documentSearchLoading" class="result-empty">正在搜索文件夹</div>
          <div v-else-if="documentSearchError" class="result-empty is-error">{{ documentSearchError }}</div>
          <div v-else-if="!visibleFolderSearchResults.length" class="result-empty">没有匹配的文件夹</div>
          <div v-else class="document-results">
            <template v-for="item in visibleFolderSearchResults" :key="item.id">
              <button type="button" class="document-result-row"
                :aria-expanded="expandedSearchFolderOriginKey === item.id"
                @click="toggleSearchFolderResult(item)">
                <span class="doc-icon is-folder"><MobileIcon name="folder" /></span>
                <span>
                  <strong>{{ item.folder.name }}</strong>
                  <small>{{ documentKnowledgeBaseName(item) }} · {{ item.folder.path }}</small>
                </span>
                <MobileIcon :name="expandedSearchFolderOriginKey === item.id ? 'chevron-up' : 'chevron-down'" />
              </button>
              <div v-if="expandedSearchFolderOriginKey === item.id" class="search-folder-browser">
                <div class="search-folder-toolbar">
                  <nav aria-label="搜索文件夹路径">
                    <template v-for="(crumb, index) in expandedSearchFolderBreadcrumbs" :key="crumb.id">
                      <MobileIcon v-if="index" name="chevron-right" />
                      <button type="button" @click="openExpandedSearchFolderBreadcrumb(crumb.id)">
                        {{ crumb.name }}
                      </button>
                    </template>
                  </nav>
                  <button type="button" @click="openExpandedSearchFolderLocation">打开</button>
                </div>
                <div v-if="expandedSearchFolderLoading" class="search-folder-state">正在加载文件夹内容</div>
                <div v-else-if="expandedSearchFolderError" class="search-folder-state is-error">
                  {{ expandedSearchFolderError }}
                </div>
                <div v-else-if="!expandedSearchFolderNodes.length" class="search-folder-state">
                  当前文件夹暂无内容
                </div>
                <div v-else class="search-folder-nodes">
                  <button v-for="node in expandedSearchFolderNodes"
                    :key="`${node.node_type}-${node.folder?.id || node.document?.id}`"
                    type="button" @click="node.node_type === 'folder' && node.folder
                      ? enterExpandedSearchFolder(node.folder)
                      : openExpandedSearchDocument(node)">
                    <span class="doc-icon" :class="{ 'is-folder': node.node_type === 'folder' }">
                      <MobileIcon :name="node.node_type === 'folder' ? 'folder' : 'file'" />
                    </span>
                    <span>
                      <strong>{{ node.node_type === 'folder'
                        ? node.folder?.name
                        : (node.document?.file_name || node.document?.title || '未命名文档') }}</strong>
                      <small v-if="node.node_type === 'folder'">
                        {{ node.folder?.stats.subtree_document_count || 0 }} 个文档
                      </small>
                      <small v-else>文档</small>
                    </span>
                    <MobileIcon name="chevron-right" />
                  </button>
                </div>
                <div v-if="expandedSearchFolderTotalPages > 1" class="search-folder-pagination">
                  <button type="button" :disabled="expandedSearchFolderPage <= 1"
                    @click="changeExpandedSearchFolderPage(expandedSearchFolderPage - 1)">上一页</button>
                  <span>{{ expandedSearchFolderPage }} / {{ expandedSearchFolderTotalPages }} ·
                    {{ expandedSearchFolderTotal }} 项</span>
                  <button type="button" :disabled="expandedSearchFolderPage >= expandedSearchFolderTotalPages"
                    @click="changeExpandedSearchFolderPage(expandedSearchFolderPage + 1)">下一页</button>
                </div>
              </div>
            </template>
          </div>
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
          </div>
        </section>
        <button v-if="documentSearchHasMore" type="button" class="load-more search-load-more"
          :disabled="documentSearchLoadingMore" @click="loadMoreDocumentResults">
          {{ documentSearchLoadingMore ? '正在加载' : '加载更多结果' }}
        </button>
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
          <span>{{ selectedKb.permissionLabel }}，可管理文件夹和文档</span>
        </div>
        <div class="upload-actions">
          <button type="button" class="secondary-action" @click="openFolderEditor('create')">
            <MobileIcon name="folder-add" />
            <span>新建文件夹</span>
          </button>
          <button type="button" class="primary-action" :disabled="uploading" @click="uploadInputRef?.click()">
            <span v-if="uploading" class="busy-icon upload" aria-label="正在上传">
              <MobileIcon name="upload" />
            </span>
            <MobileIcon v-else name="upload" />
            <span>上传文档</span>
          </button>
          <button type="button" class="secondary-action" :disabled="uploading" @click="folderUploadInputRef?.click()">
            <MobileIcon name="folder-open" />
            <span>上传文件夹</span>
          </button>
        </div>
        <input ref="uploadInputRef" type="file" :accept="acceptFileTypes" multiple hidden @change="handleUpload">
        <input ref="folderUploadInputRef" type="file" :accept="acceptFileTypes" webkitdirectory multiple hidden
          @change="handleUpload">
      </section>
      <section v-else class="readonly-card" data-testid="mobile-kb-readonly-notice">
        <MobileIcon name="lock" />
        <div>
          <strong>仅查看</strong>
          <span>你可以查看和下载文档，不能上传或删除。</span>
        </div>
      </section>

      <section class="doc-section">
        <nav class="folder-crumbs" aria-label="文件夹路径">
          <button type="button" :class="{ active: !currentFolderId }" @click="openFolder(null)">
            <MobileIcon name="home" />
            <span>根目录</span>
          </button>
          <template v-for="crumb in folderBreadcrumbs" :key="crumb.id">
            <MobileIcon name="chevron-right" />
            <button type="button" :class="{ active: crumb.id === currentFolderId }"
              @click="openFolder(folderOptions.find((option) => option.id === crumb.id) as any)">
              {{ crumb.name }}
            </button>
          </template>
        </nav>
        <div class="section-title-row">
          <span>文件夹与文档</span>
          <em class="permission-badge" :class="`is-${selectedKb.access}`">{{ selectedKb.permissionLabel }}</em>
        </div>
        <div v-if="detailFilterKeyword" class="detail-filter">
          <span>搜索定位：{{ detailFilterKeyword }}</span>
          <button type="button" @click="clearDetailFilter">查看全部</button>
        </div>
        <div v-if="loadingFiles" class="empty-state">正在加载内容</div>
        <div v-else-if="!fileList.length && !folderList.length" class="empty-state">
          {{ detailFilterKeyword ? '未找到匹配内容，可查看全部内容' : '当前文件夹暂无内容' }}
        </div>
        <div v-else class="doc-list">
          <article v-for="folder in folderList" :key="`folder-${folder.id}`" class="doc-row folder-row"
            @click="openFolder(folder)">
            <div class="doc-icon is-folder">
              <MobileIcon name="folder" />
            </div>
            <div class="doc-main">
              <strong>{{ folder.name }}</strong>
              <span>{{ folder.stats.subtree_document_count }} 个文档 ·
                {{ folder.stats.direct_child_folder_count }} 个子文件夹</span>
              <em class="parse-status"
                :class="folder.stats.abnormal_document_count ? 'is-failed' : (folder.stats.parse_pending_count + folder.stats.parse_running_count + folder.stats.enrichment_pending_task_count + folder.stats.wiki_pending_task_count ? 'is-running' : 'is-completed')">
                解析 {{ folder.stats.parse_pending_count + folder.stats.parse_running_count }} ·
                衍生 {{ folder.stats.enrichment_pending_task_count + folder.stats.wiki_pending_task_count }} ·
                异常 {{ folder.stats.abnormal_document_count }}
              </em>
            </div>
            <div v-if="selectedCanEdit" class="doc-actions folder-actions" @click.stop>
              <button type="button" @click="openFolderEditor('edit', folder)">
                <MobileIcon name="edit" /><span>编辑</span>
              </button>
              <button type="button" @click="openFolderEditor('move', folder)">
                <MobileIcon name="swap" /><span>移动</span>
              </button>
              <button type="button" class="danger" @click="removeFolder(folder)">
                <MobileIcon name="delete" /><span>删除</span>
              </button>
            </div>
            <MobileIcon v-else name="chevron-right" />
          </article>
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
              <em class="parse-status" :class="documentStatusClass(item)">
                {{ documentStatusText(item) }}
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
                :disabled="!!busyMap[item.id]"
                @click="openDocumentMove(item)"
              >
                <MobileIcon name="folder-open" />
                <span>移动</span>
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
        <div v-if="nodeTotal > nodePageSize" class="mobile-pagination">
          <button type="button" :disabled="nodePage <= 1" @click="goToNodePage(nodePage - 1)">上一页</button>
          <span>{{ nodePage }} / {{ Math.ceil(nodeTotal / nodePageSize) }}</span>
          <button type="button" :disabled="nodePage >= Math.ceil(nodeTotal / nodePageSize)"
            @click="goToNodePage(nodePage + 1)">下一页</button>
        </div>
      </section>
    </template>

    <div v-if="folderEditorVisible" class="mobile-sheet-backdrop" @click.self="folderEditorVisible = false">
      <section class="mobile-folder-sheet" role="dialog" aria-modal="true">
        <header>
          <strong>{{ folderEditorMode === 'create' ? '新建文件夹' : folderEditorMode === 'move' ? '移动文件夹' : '编辑文件夹' }}</strong>
          <button type="button" aria-label="关闭" @click="folderEditorVisible = false">
            <MobileIcon name="close" />
          </button>
        </header>
        <label v-if="folderEditorMode !== 'move'">
          <span>文件夹名称</span>
          <input v-model.trim="folderEditorForm.name" maxlength="120" placeholder="请输入名称">
        </label>
        <label v-if="folderEditorMode !== 'move'">
          <span>描述</span>
          <textarea v-model="folderEditorForm.description" maxlength="1000" rows="3"
            placeholder="选填，说明该文件夹的内容"></textarea>
        </label>
        <label>
          <span>上级文件夹</span>
          <select v-model="folderEditorForm.parent_id">
            <option v-for="option in folderParentOptions" :key="option.value || 'root'" :value="option.value">
              {{ option.label }}
            </option>
          </select>
        </label>
        <footer>
          <button type="button" class="secondary" @click="folderEditorVisible = false">取消</button>
          <button type="button" class="primary" :disabled="folderEditorSubmitting" @click="submitFolderEditor">
            {{ folderEditorSubmitting ? '正在保存' : '保存' }}
          </button>
        </footer>
      </section>
    </div>

    <div v-if="documentMoveVisible" class="mobile-sheet-backdrop" @click.self="documentMoveVisible = false">
      <section class="mobile-folder-sheet" role="dialog" aria-modal="true">
        <header>
          <strong>移动文档</strong>
          <button type="button" aria-label="关闭" @click="documentMoveVisible = false">
            <MobileIcon name="close" />
          </button>
        </header>
        <p class="move-document-name">{{ documentMoveTarget?.display_name || documentMoveTarget?.file_name }}</p>
        <label>
          <span>目标文件夹</span>
          <select v-model="documentMoveFolderId">
            <option value="">知识库根目录</option>
            <option v-for="folder in folderOptions" :key="folder.id" :value="folder.id">{{ folder.path }}</option>
          </select>
        </label>
        <footer>
          <button type="button" class="secondary" @click="documentMoveVisible = false">取消</button>
          <button type="button" class="primary" :disabled="documentMoveSubmitting" @click="submitDocumentMove">
            {{ documentMoveSubmitting ? '正在移动' : '确认移动' }}
          </button>
        </footer>
      </section>
    </div>
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

.document-result-row[aria-expanded="true"] {
  color: var(--mobile-accent);
  background: #f2f7f4;
}

.search-folder-browser {
  margin: 0 8px 8px;
  border: 1px solid #dfe9e4;
  border-top: 0;
  border-radius: 0 0 9px 9px;
  background: #f7faf8;
  overflow: hidden;
}

.search-folder-toolbar,
.search-folder-pagination {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
  min-height: 38px;
  padding: 6px 8px;
  border-bottom: 1px solid #dfe9e4;
}

.search-folder-toolbar nav {
  display: flex;
  align-items: center;
  min-width: 0;
  gap: 2px;
  overflow: hidden;
}

.search-folder-toolbar nav > button,
.search-folder-toolbar > button,
.search-folder-pagination button {
  flex: none;
  max-width: 112px;
  padding: 4px 6px;
  border: 0;
  border-radius: 5px;
  color: #617269;
  background: transparent;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.search-folder-toolbar > button,
.search-folder-pagination button {
  border: 1px solid #d8e3dd;
  background: #fff;
}

.search-folder-pagination button:disabled {
  opacity: 0.42;
}

.search-folder-state {
  display: flex;
  align-items: center;
  justify-content: center;
  min-height: 68px;
  padding: 12px;
  color: #7f8e87;
  font-size: 12px;
}

.search-folder-state.is-error {
  color: #bf3636;
}

.search-folder-nodes {
  display: flex;
  flex-direction: column;
  padding: 4px;
}

.search-folder-nodes > button {
  display: grid;
  grid-template-columns: 32px minmax(0, 1fr) 18px;
  align-items: center;
  gap: 8px;
  min-height: 52px;
  padding: 6px 8px;
  border: 0;
  border-radius: 7px;
  color: #6f8077;
  background: transparent;
  text-align: left;
}

.search-folder-nodes > button:active {
  background: #edf5f0;
}

.search-folder-nodes > button > span:nth-child(2) {
  display: flex;
  min-width: 0;
  flex-direction: column;
  gap: 4px;
}

.search-folder-nodes strong,
.search-folder-nodes small {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.search-folder-nodes strong {
  color: #1d2d25;
  font-size: 13px;
}

.search-folder-nodes small {
  color: #7f8e87;
  font-size: 11px;
}

.search-folder-pagination {
  border-top: 1px solid #dfe9e4;
  border-bottom: 0;
  color: #7f8e87;
  font-size: 11px;
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
  padding: 14px 12px 12px;
}

.upload-card > div:first-child,
.readonly-card > div {
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
  justify-content: center;
  gap: 5px;
  min-width: 0;
  height: 40px;
  border: 1px solid #cfe5d9;
  border-radius: 9px;
  background: #f3faf6;
  color: #078f49;
  padding: 0 5px;
  font-size: 13px;
  font-weight: 650;
  white-space: nowrap;
}

.upload-card button.primary-action {
  border-color: #07b859;
  background: #07b859;
  color: #fff;
  box-shadow: 0 3px 9px rgb(7 184 89 / 18%);
}

.upload-card button:disabled {
  border-color: #c7d6cf;
  background: #c7d6cf;
  color: #fff;
  box-shadow: none;
}

.upload-card .upload-actions {
  display: grid;
  grid-template-columns: repeat(3, minmax(0, 1fr));
  gap: 7px;
  margin-top: 12px;
  padding-top: 11px;
  border-top: 1px solid #edf3f0;
}

@media (max-width: 350px) {
  .upload-card button {
    height: 48px;
    flex-direction: column;
    gap: 2px;
    font-size: 12px;
    line-height: 1.1;
  }

  .upload-card button > span:not(.busy-icon) {
    width: 100%;
    overflow: visible;
    text-overflow: clip;
  }
}

.search-load-more {
  width: 100%;
  margin-top: 10px;
  border: 1px solid #dfe9e4;
  border-radius: 10px;
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

.folder-crumbs {
  display: flex;
  min-height: 42px;
  align-items: center;
  gap: 3px;
  overflow-x: auto;
  border-bottom: 1px solid #edf1ef;
  padding: 5px 9px;
}

.folder-crumbs > button {
  display: inline-flex;
  min-height: 30px;
  align-items: center;
  gap: 4px;
  flex: 0 0 auto;
  border: 0;
  border-radius: 6px;
  background: transparent;
  color: #667970;
  padding: 0 7px;
  font-size: 13px;
}

.folder-crumbs > button.active {
  background: #edf8f2;
  color: #078f49;
  font-weight: 650;
}

.folder-crumbs > :not(button) {
  flex: 0 0 auto;
  color: #a0ada7;
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

.doc-row.folder-row {
  grid-template-columns: 34px minmax(0, 1fr) auto;
  border-color: #e2ebe7;
  background: #fbfdfc;
}

.doc-row.folder-row:active {
  background: #eff8f3;
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

.doc-icon.is-folder {
  background: #fff7e7;
  color: #bc7613;
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

.folder-actions {
  grid-column: 2 / 4;
  flex-wrap: wrap;
}

.mobile-pagination {
  display: flex;
  align-items: center;
  justify-content: space-between;
  border-top: 1px solid #edf1ef;
  padding: 10px 12px 5px;
}

.mobile-pagination button {
  min-width: 72px;
  height: 34px;
  border: 1px solid #cfe0d7;
  border-radius: 17px;
  background: #fff;
  color: #078f49;
  font-size: 13px;
}

.mobile-pagination button:disabled {
  color: #9aaba3;
  opacity: 0.58;
}

.mobile-pagination span {
  color: #6d7e76;
  font-size: 13px;
}

.mobile-sheet-backdrop {
  position: fixed;
  z-index: 1000;
  inset: 0;
  display: flex;
  align-items: flex-end;
  background: rgb(16 30 23 / 38%);
}

.mobile-folder-sheet {
  width: 100%;
  border-radius: 18px 18px 0 0;
  background: #fff;
  padding: 12px 16px calc(env(safe-area-inset-bottom) + 18px);
  box-shadow: 0 -12px 42px rgb(24 48 36 / 18%);
}

.mobile-folder-sheet header {
  display: flex;
  height: 44px;
  align-items: center;
  justify-content: space-between;
  margin-bottom: 8px;
}

.mobile-folder-sheet header strong {
  color: #17261f;
  font-size: 18px;
}

.mobile-folder-sheet header button {
  display: grid;
  width: 34px;
  height: 34px;
  place-items: center;
  border: 0;
  border-radius: 17px;
  background: #eef3f0;
  color: #64766d;
}

.mobile-folder-sheet label {
  display: flex;
  flex-direction: column;
  gap: 7px;
  margin-top: 14px;
  color: #40534a;
  font-size: 13px;
  font-weight: 650;
}

.move-document-name {
  overflow: hidden;
  margin: 2px 0 8px;
  color: #65776e;
  font-size: 13px;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.mobile-folder-sheet input,
.mobile-folder-sheet textarea,
.mobile-folder-sheet select {
  width: 100%;
  box-sizing: border-box;
  border: 1px solid #d7e3dd;
  border-radius: 9px;
  outline: 0;
  background: #fff;
  color: #17261f;
  padding: 10px 11px;
  font: inherit;
  font-weight: 400;
}

.mobile-folder-sheet input:focus,
.mobile-folder-sheet textarea:focus,
.mobile-folder-sheet select:focus {
  border-color: #58c88b;
  box-shadow: 0 0 0 3px rgb(7 193 96 / 8%);
}

.mobile-folder-sheet footer {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: 10px;
  margin-top: 20px;
}

.mobile-folder-sheet footer button {
  height: 42px;
  border-radius: 21px;
  font-size: 15px;
  font-weight: 650;
}

.mobile-folder-sheet footer .secondary {
  border: 1px solid #d7e3dd;
  background: #fff;
  color: #53665d;
}

.mobile-folder-sheet footer .primary {
  border: 1px solid #07c160;
  background: #07c160;
  color: #fff;
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
