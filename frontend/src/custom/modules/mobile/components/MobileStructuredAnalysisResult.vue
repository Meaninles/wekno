<script setup lang="ts">
import { nextTick, onBeforeUnmount, onMounted, ref } from "vue";
import StructuredAnalysisResult from "@/views/chat/components/tool-results/StructuredAnalysisResult.vue";
import type { StructuredAnalysisData } from "@/types/tool-results";

defineOptions({ name: "MobileStructuredAnalysisResult" });

defineProps<{
  data: StructuredAnalysisData;
}>();

const rootRef = ref<HTMLElement | null>(null);
const renderKey = ref(0);
let resizeObserver: ResizeObserver | null = null;
let resizeFrame = 0;
let lastWidth = 0;

const refreshChartWhenWidthSettles = async () => {
  await nextTick();
  if (typeof window === "undefined") return;
  window.cancelAnimationFrame(resizeFrame);
  resizeFrame = window.requestAnimationFrame(() => {
    const width = Math.round(rootRef.value?.clientWidth || 0);
    if (!width || Math.abs(width - lastWidth) <= 2) return;
    lastWidth = width;
    renderKey.value += 1;
  });
};

onMounted(() => {
  void refreshChartWhenWidthSettles();
  if (rootRef.value && typeof ResizeObserver !== "undefined") {
    resizeObserver = new ResizeObserver(() => {
      void refreshChartWhenWidthSettles();
    });
    resizeObserver.observe(rootRef.value);
  }
});

onBeforeUnmount(() => {
  resizeObserver?.disconnect();
  resizeObserver = null;
  if (typeof window !== "undefined") window.cancelAnimationFrame(resizeFrame);
});
</script>

<template>
  <section ref="rootRef" class="mobile-structured-analysis">
    <StructuredAnalysisResult :key="renderKey" :data="data" mobile-mode />
  </section>
</template>

<style scoped>
.mobile-structured-analysis {
  width: 100%;
  min-width: 0;
  max-width: 100%;
  overflow: hidden;
  border-radius: 12px;
  margin: 8px 0 2px;
}
</style>
