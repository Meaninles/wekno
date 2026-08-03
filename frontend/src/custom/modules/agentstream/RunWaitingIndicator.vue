<template>
  <div class="run-waiting-indicator" role="status" aria-live="polite">
    <span class="run-waiting-indicator__pulse" aria-hidden="true"></span>
    <span>{{ statusText }}</span>
  </div>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import {
  RUN_WAITING_UPDATE_INTERVAL_MS,
  formatRunWaiting,
} from './runWaiting'

const startedAt = Date.now()
const elapsedMs = ref(0)
let timer: ReturnType<typeof setTimeout> | undefined

const statusText = computed(() => formatRunWaiting(elapsedMs.value))

const scheduleNextBoundary = () => {
  const elapsed = Math.max(0, Date.now() - startedAt)
  elapsedMs.value = elapsed
  const nextBoundary =
    (Math.floor(elapsed / RUN_WAITING_UPDATE_INTERVAL_MS) + 1) *
    RUN_WAITING_UPDATE_INTERVAL_MS
  timer = setTimeout(
    scheduleNextBoundary,
    Math.max(20, nextBoundary - elapsed),
  )
}

onMounted(scheduleNextBoundary)
onBeforeUnmount(() => {
  if (timer) clearTimeout(timer)
})
</script>

<style scoped>
.run-waiting-indicator {
  display: inline-flex;
  align-items: center;
  gap: 8px;
  min-height: 24px;
  color: var(--td-text-color-secondary, #5e5e5e);
  font-size: 14px;
  line-height: 24px;
}

.run-waiting-indicator__pulse {
  width: 7px;
  height: 7px;
  border-radius: 50%;
  background: var(--td-brand-color, #0052d9);
  box-shadow: 0 0 0 0 color-mix(in srgb, var(--td-brand-color, #0052d9) 35%, transparent);
  animation: run-waiting-pulse 1.5s ease-out infinite;
}

@keyframes run-waiting-pulse {
  70% {
    box-shadow: 0 0 0 6px transparent;
  }
  100% {
    box-shadow: 0 0 0 0 transparent;
  }
}

@media (prefers-reduced-motion: reduce) {
  .run-waiting-indicator__pulse {
    animation: none;
  }
}
</style>

