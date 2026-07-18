<template>
  <div class="general-artifacts">
    <div class="general-artifacts__header">
      <div>
        <div class="general-artifacts__title">产物</div>
        <div class="general-artifacts__meta">{{ metaText }}</div>
      </div>
    </div>

    <t-alert
      v-if="notice"
      class="general-artifacts__notice"
      :theme="noticeTheme"
      :message="notice"
    />

    <p v-if="data.summary" class="general-artifacts__summary">{{ data.summary }}</p>

    <div class="general-artifacts__files">
      <div v-for="file in visibleFiles" :key="file.artifact_id" class="general-artifacts__file">
        <div class="general-artifacts__file-main">
          <div class="general-artifacts__file-icon">{{ fileTypeLabel(artifactFileType(file)) }}</div>
          <div class="general-artifacts__file-text">
            <div class="general-artifacts__file-name" :title="file.filename">{{ file.filename }}</div>
            <div class="general-artifacts__file-meta">
              {{ formatSize(file.file_size) }}
              <template v-if="file.sha256">
                · SHA256 {{ shortSha(file.sha256) }}
              </template>
            </div>
          </div>
        </div>
        <div class="general-artifacts__actions">
          <t-button
            v-if="canPreview(file)"
            size="small"
            variant="outline"
            @click="openPreview(file)"
          >
            预览
          </t-button>
          <t-button
            size="small"
            variant="outline"
            :loading="downloadingId === file.artifact_id"
            :disabled="!canDownload(file) || downloadingId === file.artifact_id"
            @click="download(file)"
          >
            下载
          </t-button>
          <t-button
            v-if="!isEmbedMode && !shareMode"
            size="small"
            theme="primary"
            :loading="importingId === file.artifact_id"
            :disabled="!file.download_url || importingId === file.artifact_id"
            @click="openImport(file)"
          >
            入库
          </t-button>
        </div>
      </div>
      <div v-if="hiddenArtifactCount > 0" class="general-artifacts__more">
        <t-button variant="outline" size="small" @click="showMoreArtifacts">
          展示更多（剩余 {{ hiddenArtifactCount }} 个）
        </t-button>
      </div>
      <div v-if="files.length === 0" class="general-artifacts__empty">
        没有可返回的产物文件。
      </div>
    </div>

    <t-dialog
      v-model:visible="dialogVisible"
      header="导入知识库"
      width="480px"
      attach="body"
      :confirm-btn="{ content: '确认入库', loading: importing, disabled: !canConfirm }"
      :cancel-btn="{ content: '取消' }"
      @confirm="handleImport"
    >
      <div v-if="selectedFile" class="import-dialog">
        <div class="import-dialog__file">
          <span class="import-dialog__label">文件</span>
          <span class="import-dialog__name">{{ selectedFile.filename }}</span>
        </div>

        <div class="import-dialog__field">
          <label>目标知识库</label>
          <t-select
            v-model="selectedKbId"
            filterable
            :loading="loadingKbs"
            placeholder="请选择知识库"
          >
            <t-option
              v-for="kb in knowledgeBases"
              :key="kb.id"
              :value="kb.id"
              :label="kb.name"
            />
          </t-select>
        </div>

        <p class="import-dialog__hint">
          产物会作为新文件导入目标知识库，并按知识库解析配置处理。
        </p>
      </div>
    </t-dialog>

    <t-dialog
      v-model:visible="previewDialogVisible"
      :header="previewTitle"
      width="80vw"
      attach="body"
      :footer="false"
      destroy-on-close
    >
      <DocumentPreview
        v-if="previewFile"
        :source-key="previewFile.artifact_id"
        :file-type="artifactFileType(previewFile)"
        :file-name="previewFile.filename || 'artifact'"
        :active="previewDialogVisible"
        :blob-loader="loadPreviewBlob"
      />
    </t-dialog>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, ref } from 'vue';
import { MessagePlugin } from 'tdesign-vue-next';
import DocumentPreview from '@/components/document-preview.vue';
import { getDown } from '@/utils/request';
import { downloadEmbedArtifact } from '@/api/embed';
import { getKnowledgeBaseById, listKnowledgeBases, uploadKnowledgeFile } from '@/api/knowledge-base';
import { useUploadConfirmStore } from '@/stores/uploadConfirm';
import { useEditorResourcesStore } from '@/stores/editorResources';
import {
  getDocumentPreviewMimeType,
  isDocumentPreviewSupported,
  normalizePreviewFileType,
} from '@/utils/documentPreview';
import type { GeneralAgentArtifactFile, GeneralAgentArtifactsData } from '@/types/tool-results';
import {
  ARTIFACT_PAGE_SIZE,
  artifactMetaText,
  nextArtifactVisibleCount,
  remainingArtifactCount,
  visibleArtifacts,
} from '@/custom/modules/generalagent/artifactPagination';

const props = defineProps<{
  data: GeneralAgentArtifactsData;
  embeddedMode?: boolean;
  embedChannelId?: string;
  embedToken?: string;
  embedSessionId?: string;
  embedSessionSig?: string;
  shareMode?: boolean;
}>();

interface KnowledgeBaseOption {
  id: string;
  name: string;
  type?: string;
}

const uploadConfirmStore = useUploadConfirmStore();
const editorResources = useEditorResourcesStore();
const downloadingId = ref('');
const importingId = ref('');
const importing = ref(false);
const loadingKbs = ref(false);
const dialogVisible = ref(false);
const previewDialogVisible = ref(false);
const selectedFile = ref<GeneralAgentArtifactFile | null>(null);
const previewFile = ref<GeneralAgentArtifactFile | null>(null);
const previewBlob = ref<Blob | null>(null);
const selectedKbId = ref('');
const knowledgeBases = ref<KnowledgeBaseOption[]>([]);
const visibleCount = ref(ARTIFACT_PAGE_SIZE);

onMounted(() => {
  editorResources.ensureParserEngines().catch(() => {});
});

const files = computed(() => props.data.artifacts || []);
const visibleFiles = computed(() => visibleArtifacts(files.value, visibleCount.value));
const hiddenArtifactCount = computed(() => remainingArtifactCount(files.value.length, visibleFiles.value.length));
const notice = computed(() => props.data.notice || '');
const noticeTheme = computed(() => props.data.persist_failed ? 'error' : 'warning');
const isEmbedMode = computed(() => props.embeddedMode === true);
const shareMode = computed(() => props.shareMode === true);
const canUseEmbedDownload = computed(() => {
  return isEmbedMode.value
    && !!props.embedChannelId
    && !!props.embedToken
    && !!props.embedSessionId
    && !!props.embedSessionSig;
});
const metaText = computed(() => {
  return artifactMetaText(props.data, files.value.length);
});
const canConfirm = computed(() => !!selectedFile.value && !!selectedKbId.value && !importing.value);
const previewTitle = computed(() => previewFile.value?.filename ? `预览：${previewFile.value.filename}` : '预览');
const parserSupportedFileTypes = computed(() => {
  const supported = new Set<string>();
  for (const engine of editorResources.parserEngines || []) {
    if (engine.Available === false) continue;
    for (const fileType of engine.FileTypes || []) {
      supported.add(normalizePreviewFileType(fileType));
    }
  }
  return supported;
});

function fileTypeLabel(type: string) {
  return String(type || '').replace('.', '').toUpperCase() || 'TXT';
}

function artifactFileType(file: GeneralAgentArtifactFile): string {
  const explicit = normalizePreviewFileType(file.file_type);
  if (explicit) return explicit;
  const name = file.filename || '';
  const dot = name.lastIndexOf('.');
  return dot >= 0 ? normalizePreviewFileType(name.slice(dot + 1)) : '';
}

function formatSize(size: number) {
  if (!Number.isFinite(size) || size <= 0) return '未知大小';
  if (size < 1024) return `${size} B`;
  if (size < 1024 * 1024) return `${(size / 1024).toFixed(1)} KB`;
  return `${(size / 1024 / 1024).toFixed(1)} MB`;
}

function shortSha(sha: string) {
  return sha.length > 12 ? sha.slice(0, 12) : sha;
}

function showMoreArtifacts() {
  visibleCount.value = nextArtifactVisibleCount(visibleCount.value, files.value.length);
}

async function fetchBlob(file: GeneralAgentArtifactFile): Promise<Blob> {
  if (canUseEmbedDownload.value) {
    const result = await downloadEmbedArtifact(
      props.embedChannelId || '',
      props.embedToken || '',
      props.embedSessionId || '',
      props.embedSessionSig || '',
      file.artifact_id,
    );
    return result instanceof Blob ? result : new Blob([result]);
  }
  const result = await getDown(file.download_url);
  return result instanceof Blob ? result : new Blob([result]);
}

function mimeForFile(file: GeneralAgentArtifactFile, blob: Blob): string {
  if (blob.type) return blob.type;
  const type = artifactFileType(file);
  if (type === 'docx') return 'application/vnd.openxmlformats-officedocument.wordprocessingml.document';
  if (type === 'xlsx') return 'application/vnd.openxmlformats-officedocument.spreadsheetml.sheet';
  if (type === 'pdf') return 'application/pdf';
  if (type === 'zip') return 'application/zip';
  if (type === 'json') return 'application/json';
  if (type === 'csv') return 'text/csv';
  if (type === 'html') return 'text/html';
  if (type === 'md' || type === 'markdown') return 'text/markdown';
  const previewMime = getDocumentPreviewMimeType(type);
  if (previewMime !== 'application/octet-stream') return previewMime;
  return 'text/plain';
}

function canPreview(file: GeneralAgentArtifactFile): boolean {
  const type = artifactFileType(file);
  return canDownload(file)
    && parserSupportedFileTypes.value.has(type)
    && isDocumentPreviewSupported(type);
}

function canDownload(file: GeneralAgentArtifactFile): boolean {
  if (isEmbedMode.value) {
    return canUseEmbedDownload.value && !!file.artifact_id;
  }
  return !!file.download_url;
}

function openPreview(file: GeneralAgentArtifactFile) {
  if (!canPreview(file)) return;
  previewFile.value = file;
  previewBlob.value = null;
  previewDialogVisible.value = true;
}

async function loadPreviewBlob(): Promise<Blob> {
  if (!previewFile.value) throw new Error('没有可预览的产物');
  if (!previewBlob.value) {
    previewBlob.value = await fetchBlob(previewFile.value);
  }
  return previewBlob.value;
}

async function download(file: GeneralAgentArtifactFile) {
  if (!canDownload(file)) return;
  downloadingId.value = file.artifact_id;
  try {
    const blob = await fetchBlob(file);
    const objectUrl = URL.createObjectURL(blob);
    const link = document.createElement('a');
    link.href = objectUrl;
    link.download = file.filename || 'artifact';
    link.rel = 'noopener noreferrer';
    document.body.appendChild(link);
    link.click();
    document.body.removeChild(link);
    window.setTimeout(() => URL.revokeObjectURL(objectUrl), 1000);
  } catch (err: any) {
    MessagePlugin.error(err?.message || '下载失败');
  } finally {
    downloadingId.value = '';
  }
}

async function loadKnowledgeBases() {
  if (knowledgeBases.value.length > 0) return;
  loadingKbs.value = true;
  try {
    const res: any = await listKnowledgeBases();
    const list = Array.isArray(res?.data) ? res.data : [];
    knowledgeBases.value = list
      .filter((kb: any) => (kb.type || 'document') === 'document')
      .map((kb: any) => ({ id: kb.id, name: kb.name, type: kb.type || 'document' }));
  } catch (err: any) {
    MessagePlugin.error(err?.message || '加载知识库失败');
  } finally {
    loadingKbs.value = false;
  }
}

async function openImport(file: GeneralAgentArtifactFile) {
  if (isEmbedMode.value) return;
  if (!file.download_url) return;
  selectedFile.value = file;
  selectedKbId.value = '';
  dialogVisible.value = true;
  await loadKnowledgeBases();
}

async function loadKnowledgeBaseDetail(kbId: string): Promise<any> {
  const res: any = await getKnowledgeBaseById(kbId);
  return res?.data || res;
}

function supportedFileTypes(kbInfo: any): string[] {
  const values = kbInfo?.supported_file_types || kbInfo?.supportedFileTypes || [];
  return Array.isArray(values) ? values.map((v) => String(v).replace(/^\./, '')) : [];
}

function acceptFileTypes(kbInfo: any): string {
  return supportedFileTypes(kbInfo).map((v) => `.${v}`).join(',');
}

async function handleImport() {
  if (!selectedFile.value || !canConfirm.value) return;
  const artifact = selectedFile.value;
  const kbId = selectedKbId.value;
  importing.value = true;
  importingId.value = artifact.artifact_id;
  try {
    const [blob, kbInfo] = await Promise.all([
      fetchBlob(artifact),
      loadKnowledgeBaseDetail(kbId),
    ]);
    const uploadFile = new File([blob], artifact.filename || 'artifact', {
      type: mimeForFile(artifact, blob),
    });
    const confirmResult = await uploadConfirmStore.open({
      mode: 'file',
      kbInfo,
      files: [uploadFile],
      acceptFileTypes: acceptFileTypes(kbInfo),
      supportedFileTypes: supportedFileTypes(kbInfo),
    });
    const filesToUpload = confirmResult.files?.length ? confirmResult.files : [uploadFile];
    for (const file of filesToUpload) {
      const response: any = await uploadKnowledgeFile(kbId, {
        file,
        process_config: confirmResult.processConfig,
      });
      const ok = response?.success || response?.code === 200 || response?.status === 'success' || (!response?.error && response);
      if (!ok) {
        throw new Error(response?.error?.message || response?.message || '导入失败');
      }
    }
    window.dispatchEvent(new CustomEvent('knowledgeFileUploaded', {
      detail: { kbId },
    }));
    MessagePlugin.success('产物已导入知识库，正在解析');
    dialogVisible.value = false;
  } catch (err: any) {
    if (err) {
      MessagePlugin.error(err?.message || '导入失败');
    }
  } finally {
    importing.value = false;
    importingId.value = '';
  }
}
</script>

<style scoped lang="less">
.general-artifacts {
  border: 1px solid var(--td-component-stroke);
  border-radius: 6px;
  background: var(--td-bg-color-container);
  padding: 12px;
}

.general-artifacts__header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-bottom: 8px;
}

.general-artifacts__title {
  font-size: 14px;
  font-weight: 600;
  color: var(--td-text-color-primary);
}

.general-artifacts__meta,
.general-artifacts__summary,
.general-artifacts__file-meta,
.import-dialog__hint {
  font-size: 12px;
  color: var(--td-text-color-secondary);
}

.general-artifacts__summary {
  margin: 6px 0 10px;
  line-height: 1.6;
}

.general-artifacts__notice {
  margin: 8px 0 10px;
}

.general-artifacts__files {
  display: flex;
  flex-direction: column;
  gap: 8px;
}

.general-artifacts__more {
  display: flex;
  justify-content: center;
  padding-top: 2px;
}

.general-artifacts__file {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  padding: 10px;
  border: 1px solid var(--td-component-stroke);
  border-radius: 6px;
  background: var(--td-bg-color-secondarycontainer);
}

.general-artifacts__file-main {
  display: flex;
  align-items: center;
  gap: 10px;
  min-width: 0;
  flex: 1;
}

.general-artifacts__file-icon {
  min-width: 42px;
  max-width: 58px;
  height: 28px;
  padding: 0 6px;
  border-radius: 4px;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  font-size: 12px;
  font-weight: 700;
  color: var(--td-brand-color);
  background: color-mix(in srgb, var(--td-brand-color) 10%, transparent);
  border: 1px solid color-mix(in srgb, var(--td-brand-color) 20%, transparent);
  overflow: hidden;
  text-overflow: ellipsis;
}

.general-artifacts__file-text {
  min-width: 0;
}

.general-artifacts__file-name {
  font-size: 13px;
  font-weight: 500;
  color: var(--td-text-color-primary);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  max-width: 360px;
}

.general-artifacts__actions {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-shrink: 0;
}

.general-artifacts__empty {
  padding: 10px;
  border: 1px dashed var(--td-component-stroke);
  border-radius: 6px;
  color: var(--td-text-color-secondary);
  font-size: 12px;
  background: var(--td-bg-color-secondarycontainer);
}

.import-dialog {
  display: flex;
  flex-direction: column;
  gap: 14px;
}

.import-dialog__file {
  display: flex;
  gap: 8px;
  align-items: center;
}

.import-dialog__label {
  color: var(--td-text-color-secondary);
}

.import-dialog__name {
  color: var(--td-text-color-primary);
  font-weight: 500;
  word-break: break-all;
}

.import-dialog__field {
  display: flex;
  flex-direction: column;
  gap: 6px;
}

.import-dialog__hint {
  margin: 0;
  line-height: 1.6;
}

@media (max-width: 720px) {
  .general-artifacts__file {
    align-items: flex-start;
    flex-direction: column;
  }

  .general-artifacts__actions {
    width: 100%;
    justify-content: flex-end;
  }
}
</style>
