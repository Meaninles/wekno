<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, reactive, ref, watch } from "vue";
import { useRoute, useRouter } from "vue-router";
import { MessagePlugin } from "tdesign-vue-next";
import {
  batchQueryKnowledge,
  delKnowledgeDetails,
  downKnowledgeDetails,
  listKnowledgeBases,
  listKnowledgeFiles,
  uploadKnowledgeFile,
} from "@/api/knowledge-base";
import {
  downloadBlob,
  formatFileSize,
  isParseInFlight,
  parseStatusClass,
  parseStatusText,
} from "../utils";

type KnowledgeBaseRow = Record<string, any>;
type KnowledgeFileRow = Record<string, any>;

const router = useRouter();
const route = useRoute();
const kbList = ref<KnowledgeBaseRow[]>([]);
const fileList = ref<KnowledgeFileRow[]>([]);
const selectedKbId = ref("");
const loadingKbs = ref(false);
const loadingFiles = ref(false);
const uploadInputRef = ref<HTMLInputElement | null>(null);
const uploading = ref(false);
const busyMap = reactive<Record<string, "downloading" | "deleting" | undefined>>({});

let pollTimer: ReturnType<typeof setTimeout> | null = null;

const selectedKb = computed(() => kbList.value.find((item) => item.id === selectedKbId.value) || null);
const hasRunningParse = computed(() => fileList.value.some((item) => isParseInFlight(item.parse_status)));
const returnTo = computed(() => {
  const raw = Array.isArray(route.query.returnTo) ? route.query.returnTo[0] : route.query.returnTo;
  if (typeof raw === "string" && (raw === "/chat" || raw.startsWith("/chat/"))) return raw;
  return "/chat";
});

const backToSettings = () => {
  router.push({
    path: "/settings",
    query: { returnTo: returnTo.value },
  });
};

const loadKnowledgeBases = async () => {
  loadingKbs.value = true;
  try {
    const res: any = await listKnowledgeBases();
    kbList.value = Array.isArray(res?.data) ? res.data : [];
    if (!selectedKbId.value && kbList.value.length) {
      selectedKbId.value = kbList.value[0].id;
    }
  } catch (error: any) {
    MessagePlugin.error(error?.message || "加载知识库失败");
  } finally {
    loadingKbs.value = false;
  }
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
    });
    fileList.value = (res?.data || []).map((item: any) => ({
      ...item,
      display_name: item.file_name || item.title || item.source || "未命名文档",
    }));
    schedulePolling();
  } catch (error: any) {
    MessagePlugin.error(error?.message || "加载文档失败");
  } finally {
    loadingFiles.value = false;
  }
};

const clearPolling = () => {
  if (pollTimer) {
    clearTimeout(pollTimer);
    pollTimer = null;
  }
};

const schedulePolling = () => {
  clearPolling();
  if (!hasRunningParse.value) return;
  pollTimer = setTimeout(refreshRunningStatuses, 2500);
};

const refreshRunningStatuses = async () => {
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
};

const chooseKb = async (kbId: string) => {
  if (selectedKbId.value === kbId) return;
  selectedKbId.value = kbId;
  await loadFiles();
};

const handleUpload = async (event: Event) => {
  const files = Array.from((event.target as HTMLInputElement).files || []);
  (event.target as HTMLInputElement).value = "";
  if (!files.length || !selectedKbId.value) return;
  uploading.value = true;
  try {
    for (const file of files) {
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
  void loadFiles();
});

onMounted(async () => {
  await loadKnowledgeBases();
  await loadFiles();
});

onBeforeUnmount(clearPolling);
</script>

<template>
  <main class="mobile-kb">
    <header class="kb-topbar">
      <button type="button" class="icon-button" aria-label="返回设置" @click="backToSettings">
        <MobileIcon name="chevron-left" />
      </button>
      <strong>知识库管理</strong>
      <button type="button" class="icon-button" aria-label="刷新" @click="loadFiles">
        <MobileIcon name="refresh" />
      </button>
    </header>

    <section class="kb-selector">
      <div class="section-title">选择知识库</div>
      <div v-if="loadingKbs" class="empty-state">正在加载知识库</div>
      <div v-else class="kb-rail">
        <button
          v-for="kb in kbList"
          :key="kb.id"
          type="button"
          class="kb-pill"
          :class="{ active: kb.id === selectedKbId }"
          @click="chooseKb(kb.id)"
        >
          <span>{{ kb.name }}</span>
          <small>{{ kb.document_count || kb.knowledge_count || 0 }} 个文档</small>
        </button>
      </div>
    </section>

    <section class="upload-card">
      <div>
        <strong>{{ selectedKb?.name || '未选择知识库' }}</strong>
        <span>可上传文档到当前知识库</span>
      </div>
      <button type="button" :disabled="!selectedKbId || uploading" @click="uploadInputRef?.click()">
        <span v-if="uploading" class="busy-icon upload" aria-label="正在上传">
          <MobileIcon name="upload" />
        </span>
        <MobileIcon v-else name="upload" />
        <span>上传文档</span>
      </button>
      <input ref="uploadInputRef" type="file" multiple hidden @change="handleUpload" />
    </section>

    <section class="doc-section">
      <div class="section-title">文档</div>
      <div v-if="loadingFiles" class="empty-state">正在加载文档</div>
      <div v-else-if="!fileList.length" class="empty-state">暂无文档</div>
      <div v-else class="doc-list">
        <article v-for="item in fileList" :key="item.id" class="doc-row">
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
            <button type="button" class="danger" :disabled="!!busyMap[item.id]" @click="deleteFile(item)">
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
  </main>
</template>

<style scoped>
.mobile-kb {
  min-height: 100dvh;
  background: #f5f7f8;
  padding-bottom: calc(env(safe-area-inset-bottom) + 18px);
}

.kb-topbar {
  display: grid;
  grid-template-columns: 42px 1fr 42px;
  align-items: center;
  padding: calc(env(safe-area-inset-top) + 8px) 12px 8px;
}

.kb-topbar strong {
  font-size: 17px;
  text-align: center;
}

.icon-button {
  display: grid;
  width: 38px;
  height: 38px;
  place-items: center;
  border: 1px solid #dce6e1;
  border-radius: 10px;
  background: #fff;
  color: #24372f;
  padding: 0;
}

.kb-selector,
.upload-card,
.doc-section {
  margin: 0 12px 12px;
  border: 1px solid #dfe9e4;
  border-radius: 8px;
  background: #fff;
}

.section-title {
  color: #73847c;
  font-size: 13px;
  font-weight: 650;
  padding: 10px 12px 6px;
}

.kb-rail {
  display: flex;
  gap: 8px;
  overflow-x: auto;
  padding: 0 10px 12px;
  scrollbar-width: none;
  -webkit-overflow-scrolling: touch;
}

.kb-rail::-webkit-scrollbar {
  display: none;
}

.kb-pill {
  display: flex;
  min-width: 132px;
  max-width: 190px;
  min-height: 58px;
  align-items: flex-start;
  justify-content: center;
  flex-direction: column;
  gap: 4px;
  border: 1px solid #dce8e2;
  border-radius: 8px;
  background: #f8fbf9;
  color: #22342c;
  padding: 8px 10px;
}

.kb-pill.active {
  border-color: #9fe2bb;
  background: #edf9f2;
  color: #078f49;
}

.kb-pill span,
.kb-pill small {
  overflow: hidden;
  width: 100%;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.kb-pill span {
  font-size: 14px;
  font-weight: 650;
}

.kb-pill small {
  color: #7c8d85;
  font-size: 12px;
}

.upload-card {
  display: grid;
  grid-template-columns: minmax(0, 1fr) auto;
  align-items: center;
  gap: 10px;
  padding: 12px;
}

.upload-card div {
  display: flex;
  min-width: 0;
  flex-direction: column;
  gap: 3px;
}

.upload-card strong,
.upload-card span {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.upload-card strong {
  color: #17261f;
  font-size: 15px;
}

.upload-card span {
  color: #788982;
  font-size: 13px;
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

.doc-section {
  padding-bottom: 6px;
}

.doc-list {
  display: flex;
  flex-direction: column;
  gap: 6px;
  padding: 0 8px 8px;
}

.doc-row {
  display: grid;
  grid-template-columns: 34px minmax(0, 1fr);
  gap: 9px;
  border-radius: 8px;
  background: #f8fbf9;
  padding: 10px;
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
  100% {
    opacity: 0.55;
    transform: translateY(2px);
  }
  50% {
    opacity: 1;
    transform: translateY(-2px);
  }
}

@keyframes mobileDownloadFloat {
  0%,
  100% {
    opacity: 0.55;
    transform: translateY(-2px);
  }
  50% {
    opacity: 1;
    transform: translateY(2px);
  }
}

@keyframes mobileDeleteSpin {
  0% {
    opacity: 0.65;
    transform: rotate(0deg) scale(0.92);
  }
  50% {
    opacity: 1;
    transform: rotate(12deg) scale(1.04);
  }
  100% {
    opacity: 0.65;
    transform: rotate(-12deg) scale(0.92);
  }
}

.empty-state {
  padding: 22px 12px;
  color: #788982;
  font-size: 14px;
  text-align: center;
}
</style>
