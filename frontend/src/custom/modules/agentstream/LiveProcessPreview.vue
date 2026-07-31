<template>
  <section class="live-process-preview" aria-live="polite" aria-label="智能体执行进度">
    <div class="live-process-preview__title">
      <span class="live-process-preview__pulse" aria-hidden="true"></span>
      <span>正在处理</span>
    </div>
    <div class="live-process-preview__steps">
      <div
        v-for="item in visibleItems"
        :key="item.key"
        class="live-process-preview__step"
        :class="`is-${item.state}`"
      >
        <span class="live-process-preview__dot" aria-hidden="true"></span>
        <span class="live-process-preview__text" :title="item.text">{{ item.text }}</span>
      </div>
      <div v-if="visibleItems.length === 0" class="live-process-preview__step is-running">
        <span class="live-process-preview__dot" aria-hidden="true"></span>
        <span class="live-process-preview__text">正在分析上下文和可用工具</span>
      </div>
    </div>
  </section>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import {
  MAX_LIVE_PROCESS_PREVIEWS,
  type LiveProcessPreviewItem,
} from './liveProcessPreview'

const props = defineProps<{
  items?: LiveProcessPreviewItem[]
}>()

const visibleItems = computed(() =>
  (Array.isArray(props.items) ? props.items : []).slice(-MAX_LIVE_PROCESS_PREVIEWS),
)
</script>

<style scoped>
.live-process-preview {
  width: min(640px, 100%);
  box-sizing: border-box;
  padding: 12px 14px;
  border: 1px solid var(--td-component-border, #e7e7e7);
  border-radius: 10px;
  background: var(--td-bg-color-container, #fff);
}

.live-process-preview__title {
  display: flex;
  align-items: center;
  gap: 8px;
  color: var(--td-text-color-primary, #1d2129);
  font-size: 14px;
  font-weight: 600;
}

.live-process-preview__pulse {
  width: 7px;
  height: 7px;
  border-radius: 50%;
  background: var(--td-brand-color, #0052d9);
  box-shadow: 0 0 0 0 color-mix(in srgb, var(--td-brand-color, #0052d9) 35%, transparent);
  animation: live-process-pulse 1.5s ease-out infinite;
}

.live-process-preview__steps {
  display: grid;
  gap: 6px;
  margin-top: 9px;
}

.live-process-preview__step {
  display: grid;
  grid-template-columns: 8px minmax(0, 1fr);
  align-items: center;
  gap: 8px;
  min-width: 0;
  color: var(--td-text-color-secondary, #5e5e5e);
  font-size: 13px;
  line-height: 20px;
}

.live-process-preview__dot {
  width: 5px;
  height: 5px;
  border-radius: 50%;
  background: var(--td-brand-color, #0052d9);
}

.live-process-preview__step.is-success .live-process-preview__dot {
  background: var(--td-success-color, #00a870);
}

.live-process-preview__step.is-error .live-process-preview__dot {
  background: var(--td-error-color, #d54941);
}

.live-process-preview__text {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

@keyframes live-process-pulse {
  70% {
    box-shadow: 0 0 0 6px transparent;
  }
  100% {
    box-shadow: 0 0 0 0 transparent;
  }
}

@media (prefers-reduced-motion: reduce) {
  .live-process-preview__pulse {
    animation: none;
  }
}
</style>
