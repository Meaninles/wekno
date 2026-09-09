<script setup lang="ts">
import { computed, ref } from 'vue'
import { MessagePlugin } from 'tdesign-vue-next'
import { useI18n } from 'vue-i18n'
import { CHAT_UPLOAD_MAX_BYTES, CHAT_UPLOAD_MAX_MB, chatUploadLimitMessage, type UploadRow } from './uploads'
import type { AttachmentFile } from './types'
import { fileId } from '../sessionState/storage'

const props = withDefaults(defineProps<{
  files?: AttachmentFile[]
  maxFiles?: number
  maxSizeBytes?: number
  disabled?: boolean
  supportedFileTypes?: string[]
  rows?: UploadRow[]
  validatingFile?: (file: File) => string | undefined
}>(), {
  files: () => [],
  maxFiles: 10,
  maxSizeBytes: CHAT_UPLOAD_MAX_BYTES,
  disabled: false,
  supportedFileTypes: () => [],
  rows: () => [],
})

const emit = defineEmits<{
  (event: 'update:files', files: AttachmentFile[]): void
  (event: 'remove', id: string): void
  (event: 'retry', key: string): void
  (event: 'cancel'): void
}>()

const { t } = useI18n()
const inputRef = ref<HTMLInputElement | null>(null)

const normalizedTypes = computed(() => new Set(
  props.supportedFileTypes
    .map((type) => String(type || '').trim().toLowerCase().replace(/^\./, ''))
    .filter(Boolean),
))
const accept = computed(() => [...normalizedTypes.value].map((type) => `.${type}`).join(','))

const extension = (name: string) => {
  const value = name.split(/[?#]/)[0]
  const dot = value.lastIndexOf('.')
  return dot >= 0 ? value.slice(dot + 1).toUpperCase() : 'FILE'
}

const formatSize = (bytes: number) => {
  if (bytes < 1024) return `${bytes} B`
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`
}

const fileExtension = (file: File) => {
  const value = file.name.split(/[?#]/)[0]
  const dot = value.lastIndexOf('.')
  return dot >= 0 ? value.slice(dot + 1).toLowerCase() : ''
}

const rowFor = (index: number, file: AttachmentFile) => {
  if (props.rows.some(row => row.fileId)) return props.rows.find(row => row.fileId === fileId(file.file))
  const sameNameIndex = props.files
    .slice(0, index)
    .filter((item) => item.name === file.name)
    .length
  const sameNameRows = props.rows.filter((row) => row.name === file.name)
  return sameNameRows[sameNameIndex] || props.rows[index]
}

const fileState = (index: number, file: AttachmentFile): UploadRow['state'] =>
  rowFor(index, file)?.state || 'queued'

const rowStatus = (row?: UploadRow) => {
  if (!row) return '等待上传'
  if (row.state === 'uploading') return '上传中'
  if (row.state === 'processing') return '处理中'
  if (row.state === 'ready') return '上传完成'
  if (row.state === 'failed') return row.error || '上传失败，可重试'
  if (row.state === 'cancelled') return '已取消'
  return '等待上传'
}

const isSupported = (file: File) =>
  normalizedTypes.value.size === 0 || normalizedTypes.value.has(fileExtension(file))

const triggerFileSelect = () => {
  if (!props.disabled) inputRef.value?.click()
}

const addFiles = (incoming: File[]) => {
  if (props.disabled) return
  const next = [...props.files]
  const known = new Set(next.map((item) => `${item.name}\u0000${item.size}\u0000${item.file.lastModified}`))
  for (const file of incoming) {
    if (next.length >= props.maxFiles) {
      MessagePlugin.warning(t('chat.attachmentTooMany', { max: props.maxFiles }))
      break
    }
    const key = `${file.name}\u0000${file.size}\u0000${file.lastModified}`
    if (known.has(key)) continue
    if (!file.size || file.size > props.maxSizeBytes) {
      MessagePlugin.warning(chatUploadLimitMessage(file.name))
      continue
    }
    if (!isSupported(file)) {
      MessagePlugin.warning(t('chat.attachmentTypeNotSupported', { name: file.name }))
      continue
    }
    const validationError = props.validatingFile?.(file)
    if (validationError) {
      MessagePlugin.warning(validationError)
      continue
    }
    const attachment: AttachmentFile = {
      file,
      id: crypto.randomUUID(),
      name: file.name,
      size: file.size,
      type: file.type || `.${fileExtension(file)}`,
    }
    next.push(attachment)
    known.add(key)
  }
  if (next.length !== props.files.length) emit('update:files', next)
}

const handleSelect = (event: Event) => {
  const input = event.target as HTMLInputElement
  addFiles(Array.from(input.files || []))
  input.value = ''
}

const removeFile = (id: string) => {
  emit('update:files', props.files.filter((file) => file.id !== id))
  emit('remove', id)
}

const setFiles = (files: AttachmentFile[] = []) => {
  emit('update:files', files.filter((file) => !!file?.file).slice(0, props.maxFiles).map((file) => ({ ...file })))
}

const clear = () => emit('update:files', [])

defineExpose({ files: computed(() => props.files), triggerFileSelect, addFiles, setFiles, clear })
</script>

<template>
  <!-- 文件选择器必须始终挂载；上传卡片为空时仍需要响应外部工具栏的首次点击。 -->
  <input ref="inputRef" type="file" multiple hidden :accept="accept" @change="handleSelect" />
  <div v-if="files.length || rows.some((row) => row.state !== 'ready')" class="chat-upload-cards" aria-live="polite">
    <div v-for="(file, index) in files" :key="file.id" class="chat-upload-card">
      <div
        class="chat-upload-card__progress"
        :class="`is-${fileState(index, file)}`"
        role="img"
        :aria-label="rowStatus(rowFor(index, file))"
      >
        <span class="chat-upload-card__progress-ring" aria-hidden="true" />
        <span class="chat-upload-card__file-icon" aria-hidden="true">
          <svg viewBox="0 0 24 28" fill="none" xmlns="http://www.w3.org/2000/svg">
            <path d="M4 1.5h10l6 6V26a.5.5 0 0 1-.5.5h-15A.5.5 0 0 1 4 26V1.5Z" stroke="currentColor" stroke-width="1.5" />
            <path d="M14 1.5v6h6M7.5 13h9M7.5 17h9M7.5 21h5" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" />
          </svg>
        </span>
        <span v-if="fileState(index, file) === 'ready'" class="chat-upload-card__state-mark is-ready" aria-hidden="true">✓</span>
        <span v-else-if="fileState(index, file) === 'failed'" class="chat-upload-card__state-mark is-failed" aria-hidden="true">!</span>
      </div>
      <div class="chat-upload-card__info">
        <div class="chat-upload-card__name" :title="file.name">{{ file.name }}</div>
        <div class="chat-upload-card__meta">{{ extension(file.name) }} · {{ formatSize(file.size) }}</div>
        <div class="chat-upload-card__status" :class="`is-${fileState(index, file)}`">
          {{ rowStatus(rowFor(index, file)) }}
        </div>
      </div>
      <button type="button" class="chat-upload-card__remove" :aria-label="t('common.remove')" :disabled="disabled" @click="removeFile(file.id)">×</button>
      <button
        v-if="rowFor(index, file)?.state === 'failed'"
        type="button"
        class="chat-upload-card__retry"
        :disabled="disabled && rowFor(index, file)?.state !== 'failed'"
        @click="emit('retry', rowFor(index, file)?.key || file.id)"
      >重试</button>
    </div>
    <button v-if="rows.some((row) => ['uploading', 'processing'].includes(row.state))" type="button" class="chat-upload-cards__cancel" @click="emit('cancel')">取消上传</button>
  </div>
</template>

<style scoped lang="less">
.chat-upload-cards {
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
  padding: 10px 12px 4px;
  max-height: 170px;
  overflow: auto;
}

.chat-upload-card {
  position: relative;
  display: flex;
  align-items: center;
  gap: 9px;
  min-width: 210px;
  max-width: 290px;
  padding: 8px 30px 8px 9px;
  border: 1px solid color-mix(in srgb, var(--td-text-color-primary, #333) 18%, transparent);
  border-radius: 12px;
  background: color-mix(in srgb, var(--td-bg-color-container, #fff) 92%, #000);
}

.chat-upload-card__progress {
  position: relative;
  display: grid;
  place-items: center;
  width: 34px;
  height: 34px;
  flex: 0 0 34px;
  color: var(--td-brand-color, #2f7cf6);
  border-radius: 50%;
  background: color-mix(in srgb, currentColor 8%, transparent);
}

.chat-upload-card__progress-ring {
  position: absolute;
  inset: 0;
  border: 2px solid color-mix(in srgb, currentColor 22%, transparent);
  border-top-color: currentColor;
  border-radius: 50%;
}

.chat-upload-card__progress.is-uploading .chat-upload-card__progress-ring,
.chat-upload-card__progress.is-processing .chat-upload-card__progress-ring {
  animation: chat-upload-spin .9s linear infinite;
}

.chat-upload-card__progress.is-queued .chat-upload-card__progress-ring { border-style: dashed; }
.chat-upload-card__progress.is-ready {
  color: #2fb36e;
  background: color-mix(in srgb, #2fb36e 12%, transparent);
}
.chat-upload-card__progress.is-ready .chat-upload-card__progress-ring { display: none; }
.chat-upload-card__progress.is-failed {
  color: #e34d59;
  background: color-mix(in srgb, #e34d59 12%, transparent);
}
.chat-upload-card__progress.is-failed .chat-upload-card__progress-ring { border-color: currentColor; }
.chat-upload-card__progress.is-cancelled {
  color: var(--td-text-color-secondary, #888);
  background: color-mix(in srgb, currentColor 8%, transparent);
}
.chat-upload-card__progress.is-cancelled .chat-upload-card__progress-ring { border-color: currentColor; }

.chat-upload-card__file-icon {
  z-index: 1;
  display: inline-flex;
  width: 17px;
  height: 20px;
  color: currentColor;
}

.chat-upload-card__file-icon svg { width: 100%; height: 100%; }

.chat-upload-card__state-mark {
  position: absolute;
  right: -3px;
  bottom: -3px;
  display: grid;
  place-items: center;
  width: 15px;
  height: 15px;
  border: 2px solid var(--td-bg-color-container, #fff);
  border-radius: 50%;
  color: #fff;
  font-size: 10px;
  font-weight: 700;
  line-height: 1;
}

.chat-upload-card__state-mark.is-ready { background: #2fb36e; }
.chat-upload-card__state-mark.is-failed { background: #e34d59; }

.chat-upload-card__info { min-width: 0; flex: 1; }
.chat-upload-card__name { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; font-size: 13px; font-weight: 600; }
.chat-upload-card__meta { margin-top: 2px; color: var(--td-text-color-secondary, #888); font-size: 11px; }
.chat-upload-card__status { margin-top: 2px; overflow: hidden; color: var(--td-text-color-secondary, #888); font-size: 11px; text-overflow: ellipsis; white-space: nowrap; }
.chat-upload-card__status.is-ready { color: #2f9e64; }
.chat-upload-card__status.is-failed { color: #d54955; }
.chat-upload-card__remove { position: absolute; top: 5px; right: 6px; width: 18px; height: 18px; padding: 0; border: 0; border-radius: 50%; color: #fff; background: #34373b; cursor: pointer; line-height: 16px; }
.chat-upload-card__retry { margin-left: 3px; padding: 2px 5px; border: 0; border-radius: 4px; color: var(--td-brand-color); background: transparent; cursor: pointer; font-size: 11px; }
.chat-upload-cards__cancel { width: 100%; padding: 2px 0; border: 0; color: var(--td-text-color-secondary); background: transparent; cursor: pointer; text-align: left; font-size: 11px; }

@keyframes chat-upload-spin { to { transform: rotate(360deg); } }

@media (prefers-reduced-motion: reduce) {
  .chat-upload-card__progress.is-uploading .chat-upload-card__progress-ring,
  .chat-upload-card__progress.is-processing .chat-upload-card__progress-ring {
    animation: none;
  }
}
</style>
