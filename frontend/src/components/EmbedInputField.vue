<template>
  <div class="embed-input-box" :class="{ 'is-replying': isReplying }">
    <ChatUploadCards
      ref="uploadCardsRef"
      :files="uploadedFiles"
      :rows="props.uploadRows"
      :max-files="10"
      :supported-file-types="uploadTypes"
      :disabled="Boolean(props.inert) || Boolean(props.uploadsPreparing)"
      :validating-file="validateEmbedFile"
      @update:files="handleFilesUpdate"
      @retry="emit('upload-retry', $event)"
      @cancel="emit('upload-cancel')"
    />
    <t-textarea
      v-if="textareaReady"
      ref="textareaRef"
      v-model="query"
      class="embed-input-box__textarea"
      :class="{ 'has-files': uploadedFiles.length > 0 }"
      :placeholder="t('input.placeholder')"
      :autosize="{ minRows: 2, maxRows: 6 }"
      @keydown="onKeydown"
      @compositionstart="isComposing = true"
      @compositionend="isComposing = false"
    />
    <div class="embed-input-box__bar">
      <div v-if="showWebSearchToggle || showFileUploadToggle" class="embed-input-box__controls">
        <t-tooltip v-if="showWebSearchToggle" placement="top">
          <template #content>
            {{ webSearchEnabled ? t('input.webSearch.toggleOff') : t('input.webSearch.toggleOn') }}
          </template>
          <button
            type="button"
            class="embed-control-btn embed-websearch-btn"
            :class="{ active: webSearchEnabled }"
            :aria-label="t('input.webSearch.label')"
            @click="toggleWebSearch"
          >
            <svg width="18" height="18" viewBox="0 0 18 18" fill="none" aria-hidden="true">
              <circle cx="9" cy="9" r="7" stroke="currentColor" stroke-width="1.2" fill="none" />
              <path d="M 9 2 A 3.5 7 0 0 0 9 16" stroke="currentColor" stroke-width="1.2" fill="none" />
              <path d="M 9 2 A 3.5 7 0 0 1 9 16" stroke="currentColor" stroke-width="1.2" fill="none" />
              <line x1="2.94" y1="5.5" x2="15.06" y2="5.5" stroke="currentColor" stroke-width="1.2" stroke-linecap="round" />
              <line x1="2.94" y1="12.5" x2="15.06" y2="12.5" stroke="currentColor" stroke-width="1.2" stroke-linecap="round" />
            </svg>
          </button>
        </t-tooltip>
        <t-tooltip v-if="showFileUploadToggle" placement="top" :content="'上传文件、图片或语音'">
          <button
            type="button"
            class="embed-control-btn embed-file-btn"
            :class="{ active: uploadedFiles.length > 0 }"
            :aria-label="'上传文件、图片或语音'"
            :disabled="props.uploadsPreparing"
            @click="uploadCardsRef?.triggerFileSelect()"
          >
            <t-icon name="attach" size="18px" />
          </button>
        </t-tooltip>
      </div>
      <div class="embed-input-box__actions">
        <t-tooltip v-if="isReplying" :content="t('input.stopGeneration')" placement="top">
          <button type="button" class="embed-stop-btn" @click="emit('stop-generation')">
            <svg width="16" height="16" viewBox="0 0 16 16" fill="currentColor" aria-hidden="true">
              <rect x="5" y="5" width="6" height="6" rx="1" />
            </svg>
          </button>
        </t-tooltip>
        <button
          v-else
          type="button"
          class="embed-send-btn"
          :class="{ disabled: !canSend }"
          :aria-label="t('input.send')"
          @click="submit"
        >
          <img src="@/assets/img/sending-aircraft.svg" :alt="t('input.send')" />
        </button>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, nextTick, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { embedToast } from '@/utils/embedToast'
import { isEmbedImageFile } from '@/utils/embedFile'
import ChatUploadCards from '@/custom/modules/chatuploads/ChatUploadCards.vue'
import type { AttachmentFile } from '@/custom/modules/chatuploads/types'
import type { UploadRow } from '@/custom/modules/chatuploads/uploads'

const DEFAULT_EMBED_UPLOAD_TYPES = [
  'docx', 'doc', 'pdf', 'md', 'markdown', 'txt', 'text', 'csv', 'json',
  'xlsx', 'xls', 'pptx', 'ppt', 'epub', 'mhtml',
  'jpg', 'jpeg', 'png', 'gif', 'bmp', 'tiff', 'webp',
  'mp3', 'wav', 'm4a', 'flac', 'ogg',
]

const props = defineProps<{
  isReplying: boolean
  showWebSearchToggle?: boolean
  webSearchEnabled?: boolean
  showFileUploadToggle?: boolean
  inert?: boolean
  uploadsPreparing?: boolean
  uploadRows?: UploadRow[]
  supportedFileTypes?: string[]
  allowImages?: boolean
}>()

const emit = defineEmits<{
  (e: 'send-msg', query: string, imageFiles: File[], attachmentFiles: File[]): void
  (e: 'stop-generation'): void
  (e: 'update:webSearchEnabled', value: boolean): void
  (e: 'files-changed', files: AttachmentFile[]): void
  (e: 'upload-retry', key: string): void
  (e: 'upload-cancel'): void
}>()

const { t } = useI18n()
const query = ref('')
const isComposing = ref(false)
const textareaReady = ref(false)
const textareaRef = ref<{ $el?: HTMLElement } | HTMLTextAreaElement | null>(null)

const getTextareaEl = (): HTMLTextAreaElement | null => {
  const el = textareaRef.value
  if (!el) return null
  if (el instanceof HTMLTextAreaElement) return el
  const inner = el.$el?.querySelector?.('textarea')
  return inner instanceof HTMLTextAreaElement ? inner : null
}

onMounted(() => {
  nextTick(() => {
    textareaReady.value = true
  })
})
const uploadCardsRef = ref<InstanceType<typeof ChatUploadCards> | null>(null)
const uploadedFiles = ref<AttachmentFile[]>([])

const uploadTypes = computed(() => {
  const configured = (props.supportedFileTypes || []).map((type) => String(type || '').trim().toLowerCase().replace(/^\./, '')).filter(Boolean)
  return configured.length > 0 ? [...new Set(configured)] : DEFAULT_EMBED_UPLOAD_TYPES
})

const fileExtension = (file: File) => {
  const name = file.name.split(/[?#]/)[0]
  const dot = name.lastIndexOf('.')
  return dot >= 0 ? name.slice(dot + 1).toLowerCase() : ''
}

const isAudioFile = (file: File) => file.type.startsWith('audio/') || ['mp3', 'wav', 'm4a', 'flac', 'ogg'].includes(fileExtension(file))

const validateEmbedFile = (file: File) => {
  if (isEmbedImageFile(file) && props.allowImages === false) {
    return '当前嵌入渠道未启用图片上传'
  }
  if (isAudioFile(file)) return undefined
  return undefined
}

const canSend = computed(() =>
  query.value.trim().length > 0 || uploadedFiles.value.length > 0)

const toggleWebSearch = () => {
  const next = !props.webSearchEnabled
  emit('update:webSearchEnabled', next)
  embedToast(next ? t('input.messages.webSearchEnabled') : t('input.messages.webSearchDisabled'))
}

const handleFilesUpdate = (files: AttachmentFile[]) => {
  uploadedFiles.value = files
  emit('files-changed', files)
}

const submit = () => {
  if (props.isReplying || !canSend.value) return
  const val = query.value.trim()
  const imageFiles = uploadedFiles.value.filter((item) => isEmbedImageFile(item.file)).map((item) => item.file)
  const attachmentFiles = uploadedFiles.value.filter((item) => !isEmbedImageFile(item.file)).map((item) => item.file)
  const textarea = getTextareaEl()
  if (textarea) textarea.blur()
  emit('send-msg', val, imageFiles, attachmentFiles)
  if (getTextareaEl()) query.value = ''
}

defineExpose({ restoreDraft: (text: string, images: File[], attachments: File[]) => {
  query.value = text
  uploadedFiles.value = [...images, ...attachments].filter((file): file is File => file instanceof File).map((file) => ({
    file,
    id: crypto.randomUUID(),
    name: file.name,
    size: file.size,
    type: file.type || `.${fileExtension(file)}`,
  }))
}, clearFiles: () => { uploadedFiles.value = [] } })

const onKeydown = (_val: string, ctx: { e: KeyboardEvent }) => {
  const e = ctx?.e
  if (!e || e.keyCode !== 13) return
  if (isComposing.value) return
  if (e.shiftKey || e.ctrlKey) return
  e.preventDefault()
  submit()
}

</script>

<style scoped lang="less">
.embed-input-box {
  position: relative;
  width: 100%;
  max-width: 800px;
  margin: 0 auto;
  background: var(--td-bg-color-container, #fff);
  border-radius: 12px;
  border: 0.5px solid var(--td-component-border, #e7e7e7);
  box-shadow: 0 6px 6px rgba(0, 0, 0, 0.04), 0 12px 12px -1px rgba(0, 0, 0, 0.08);
  transition: border-color 0.15s ease;

  &:focus-within {
    border-color: var(--embed-primary, var(--td-brand-color, #07c05f));
  }

  &__textarea {
    width: 100%;

    :deep(.t-textarea__inner) {
      border: none;
      box-shadow: none;
      background: transparent;
      padding: 14px 16px 52px;
      font-size: 14px;
      line-height: 1.5;
      resize: none;
    }

    &.has-files :deep(.t-textarea__inner) {
      padding-top: 8px;
    }
  }

  &__bar {
    position: absolute;
    left: 12px;
    right: 12px;
    bottom: 12px;
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 8px;
    pointer-events: none;

    > * {
      pointer-events: auto;
    }
  }

  &__controls {
    display: flex;
    align-items: center;
    gap: 6px;
  }

  &__actions {
    margin-left: auto;
    display: flex;
    align-items: center;
  }
}

.embed-control-btn {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 28px;
  height: 28px;
  padding: 0;
  border: none;
  border-radius: 6px;
  cursor: pointer;
  color: var(--td-text-color-secondary);
  background: transparent;
  transition: color 0.15s ease, background 0.15s ease;

  &:hover {
    background: var(--td-bg-color-secondarycontainer);
    color: var(--td-text-color-primary);
  }

  &.active {
    color: var(--embed-primary, var(--td-brand-color, #07c05f));
    background: color-mix(in srgb, var(--embed-primary, #07c05f) 12%, transparent);
  }
}

.embed-send-btn {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 28px;
  height: 28px;
  padding: 0;
  border: none;
  border-radius: 6px;
  cursor: pointer;
  background: var(--embed-primary, var(--td-brand-color, #07c05f));
  transition: background 0.15s ease, opacity 0.15s ease;

  &:hover:not(.disabled) {
    filter: brightness(0.94);
  }

  &.disabled {
    cursor: not-allowed;
    opacity: 0.45;
  }

  img {
    width: 16px;
    height: 16px;
  }
}

.embed-stop-btn {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 28px;
  height: 28px;
  padding: 0;
  border: none;
  border-radius: 6px;
  cursor: pointer;
  color: var(--td-text-color-secondary);
  background: var(--td-bg-color-secondarycontainer);

  &:hover {
    background: var(--td-bg-color-component-hover);
  }
}
</style>
