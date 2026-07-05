<template>
  <t-tooltip :content="t('knowledgeBase.uploadInfoButton')" placement="top">
    <span class="upload-info-button-wrap">
      <t-button
        variant="text"
        theme="default"
        size="small"
        :class="['upload-info-button', triggerClass, { 'is-active': activeUploadCount > 0 }]"
        :aria-label="t('knowledgeBase.uploadInfoButton')"
        @click="uploadInfo.openPanel()"
      >
        <template #icon>
          <t-icon :name="activeUploadCount > 0 ? 'loading' : 'queue'" size="16px" />
        </template>
      </t-button>
      <span v-if="activeUploadCount > 0" class="upload-info-button__badge">
        {{ activeUploadCount > 99 ? '99+' : activeUploadCount }}
      </span>
    </span>
  </t-tooltip>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { useUploadInfoStore } from './store'

withDefaults(defineProps<{
  triggerClass?: string
}>(), {
  triggerClass: '',
})

const { t } = useI18n()
const uploadInfo = useUploadInfoStore()
const activeUploadCount = computed(() => uploadInfo.activeUploadCount.value)
</script>

<style lang="less" scoped>
.upload-info-button-wrap {
  position: relative;
  display: inline-flex;
}

.upload-info-button {
  align-items: center;
  justify-content: center;
  color: var(--td-text-color-secondary);
  background: transparent;
  transition: color 0.15s ease, background-color 0.15s ease;

  &:hover {
    color: var(--td-brand-color);
    background: var(--td-bg-color-secondarycontainer);
  }

  &.is-active {
    color: var(--td-brand-color);
  }

  &.is-active :deep(.t-icon) {
    animation: upload-info-spin 1.1s linear infinite;
  }
}

.upload-info-button__badge {
  position: absolute;
  top: 2px;
  right: 1px;
  min-width: 16px;
  height: 16px;
  padding: 0 4px;
  border-radius: 8px;
  box-sizing: border-box;
  border: 1px solid var(--td-bg-color-container);
  background: var(--td-error-color);
  color: var(--td-text-color-anti);
  font-size: 10px;
  line-height: 14px;
  text-align: center;
}

@keyframes upload-info-spin {
  from {
    transform: rotate(0deg);
  }

  to {
    transform: rotate(360deg);
  }
}
</style>
