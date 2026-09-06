<script setup lang="ts">
import type { UploadRow } from './uploads'
defineProps<{ rows: UploadRow[]; preparing: boolean }>()
defineEmits<{ (e: 'retry', key: string): void; (e: 'cancel'): void }>()
const labels = { queued: '处理中', uploading: '处理中', processing: '处理中', ready: '已完成', failed: '处理失败', cancelled: '已取消' }
</script>
<template>
  <section v-if="rows.length && preparing" class="chat-upload-progress" aria-label="附件处理状态" aria-live="polite">
    <div class="upload-summary">附件全部处理完成后发送消息 · {{ rows.filter(row => row.state === 'ready').length }}/{{ rows.length }}</div>
    <div v-for="row in rows" :key="row.key" class="upload-row" :data-state="row.state">
      <span class="upload-indicator" :class="{ busy: ['queued', 'uploading', 'processing'].includes(row.state) }" aria-hidden="true"></span>
      <span class="upload-name" :title="row.name">{{ row.name }}</span>
      <span role="status" :aria-label="`${row.name}：${labels[row.state]}`">{{ labels[row.state] }}</span>
      <button v-if="row.state === 'failed'" type="button" @click="$emit('retry', row.key)">重试</button>
      <span v-if="row.error" class="upload-error">{{ row.error }}</span>
    </div>
    <button type="button" @click="$emit('cancel')">取消文件处理并保留输入</button>
  </section>
</template>
<style scoped>
.chat-upload-progress { padding: 10px 12px; margin: 6px 0; background: var(--td-bg-color-secondarycontainer, #f4f6f8); border-radius: 8px; font-size: 12px; }
.upload-summary { margin-bottom: 6px; color: var(--td-text-color-secondary, #666); }
.upload-indicator { width: 8px; height: 8px; flex: 0 0 8px; border-radius: 50%; background: var(--td-text-color-placeholder, #999); }
.upload-indicator.busy { background: var(--td-brand-color, #1669c9); animation: pulse 1.2s ease-in-out infinite alternate; }
.upload-row[data-state='ready'] .upload-indicator { background: var(--td-success-color, #16a35e); }
.upload-row[data-state='failed'] .upload-indicator { background: var(--td-error-color, #b22); }
@keyframes pulse { to { opacity: .3; } }
@media (prefers-reduced-motion: reduce) { .upload-indicator.busy { animation: none; } }
.upload-row { display: flex; gap: 8px; align-items: center; flex-wrap: wrap; padding: 3px 0; }
.upload-name { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; max-width: 55%; }
.upload-error { flex-basis: 100%; color: var(--td-error-color, #b22); }
button { border: 0; background: transparent; color: var(--td-brand-color, #1669c9); cursor: pointer; padding: 4px; }
</style>
