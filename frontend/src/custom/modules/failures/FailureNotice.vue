<script setup lang="ts">
import type { PublicFailure } from './failures'
defineProps<{ failure: PublicFailure }>()
</script>

<template>
  <div class="failure-notice" :data-error-code="failure.code" :role="failure.code === 'cancelled' ? 'status' : 'alert'">
    <svg aria-hidden="true" viewBox="0 0 24 24"><circle cx="12" cy="12" r="9" /><path d="M12 7v6m0 4h.01" /></svg>
    <span>{{ failure.message }}</span>
  </div>
</template>

<style scoped>
.failure-notice { display: flex; align-items: flex-start; gap: 8px; padding: 12px 14px; border: 1px solid var(--td-component-border, #ddd); border-radius: 8px; background: var(--td-bg-color-secondarycontainer, #f7f8fa); color: var(--td-text-color-secondary, #555); font-size: 14px; line-height: 1.6; overflow-wrap: anywhere; }
svg { flex: 0 0 18px; width: 18px; height: 18px; margin-top: 2px; fill: none; stroke: currentColor; stroke-width: 1.7; stroke-linecap: round; }
</style>
