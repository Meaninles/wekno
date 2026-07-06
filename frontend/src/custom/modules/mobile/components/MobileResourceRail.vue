<script setup lang="ts">
import { computed } from "vue";
import type { MobileResourceChip, MobileResourceType } from "../utils";

const props = defineProps<{
  items: MobileResourceChip[];
  dense?: boolean;
  alignEnd?: boolean;
}>();

const emit = defineEmits<{
  remove: [item: MobileResourceChip];
  clear: [];
}>();

const iconFor = (type: MobileResourceType) => {
  const map: Record<MobileResourceType, string> = {
    agent: "user-talk",
    model: "cpu",
    kb: "folder",
    file: "file",
    tag: "tag",
    skill: "bookmark",
    professional: "education",
    mcp: "tools",
    web: "internet",
    image: "image",
    attachment: "attach",
  };
  return map[type] || "app";
};

const labelFor = (type: MobileResourceType) => {
  const map: Record<MobileResourceType, string> = {
    agent: "智能体",
    model: "模型",
    kb: "知识库",
    file: "文件",
    tag: "标签",
    skill: "技能",
    professional: "专业",
    mcp: "MCP",
    web: "联网",
    image: "图片",
    attachment: "附件",
  };
  return map[type] || "资源";
};

const showClearChip = computed(() =>
  props.items.length > 3 && props.items.some((item) => item.removable !== false),
);
</script>

<template>
  <div v-if="items.length" class="mobile-resource-rail" :class="{ 'is-dense': dense, 'is-align-end': alignEnd }">
    <div class="mobile-resource-rail__track">
      <div
        v-for="item in items"
        :key="`${item.type}:${item.id}`"
        class="resource-chip"
        :class="`is-${item.type}`"
      >
        <div class="resource-chip__icon">
          <MobileIcon :name="iconFor(item.type)" />
        </div>
        <div class="resource-chip__text">
          <span class="resource-chip__type">{{ labelFor(item.type) }}</span>
          <span class="resource-chip__name">{{ item.name }}</span>
        </div>
        <button
          v-if="item.removable !== false"
          type="button"
          class="resource-chip__remove"
          :aria-label="`移除${item.name}`"
          @click.stop="emit('remove', item)"
        >
          <MobileIcon name="close" />
        </button>
      </div>
      <button
        v-if="showClearChip"
        type="button"
        class="resource-clear-chip"
        aria-label="全部清空引用资源"
        @click.stop="emit('clear')"
      >
        <MobileIcon name="close" />
        <span>全部清空</span>
      </button>
    </div>
  </div>
</template>

<style scoped>
.mobile-resource-rail {
  display: flex;
  width: 100%;
  overflow-x: auto;
  overflow-y: hidden;
  padding: 2px 2px 4px;
  scrollbar-width: none;
  -webkit-overflow-scrolling: touch;
}

.mobile-resource-rail__track {
  display: flex;
  flex: 0 0 auto;
  min-width: 100%;
  width: max-content;
  justify-content: flex-start;
  gap: 8px;
}

.is-align-end .mobile-resource-rail__track {
  justify-content: flex-end;
}

.mobile-resource-rail::-webkit-scrollbar {
  display: none;
}

.resource-chip {
  --chip-bg: #f0f7f3;
  --chip-border: #b7e3c8;
  --chip-color: #078f49;
  display: grid;
  grid-template-columns: 28px minmax(52px, auto) 18px;
  align-items: center;
  flex: 0 0 auto;
  min-width: 112px;
  max-width: 178px;
  min-height: 44px;
  border: 1px solid var(--chip-border);
  border-radius: 8px;
  background: var(--chip-bg);
  color: #1b2a24;
  padding: 6px 6px;
}

.is-dense .resource-chip {
  min-height: 38px;
  min-width: 96px;
}

.resource-chip.is-agent {
  --chip-bg: #eff7ff;
  --chip-border: #b8daf8;
  --chip-color: #2672b9;
}

.resource-chip.is-model {
  --chip-bg: #f4f2ff;
  --chip-border: #cbc3fb;
  --chip-color: #6958d8;
}

.resource-chip.is-kb {
  --chip-bg: #eefaf3;
  --chip-border: #aee6c2;
  --chip-color: #07a557;
}

.resource-chip.is-file,
.resource-chip.is-attachment {
  --chip-bg: #fff7eb;
  --chip-border: #f3cd99;
  --chip-color: #b56d13;
}

.resource-chip.is-tag {
  --chip-bg: #fff1f4;
  --chip-border: #f6b9c6;
  --chip-color: #c64563;
}

.resource-chip.is-skill,
.resource-chip.is-professional {
  --chip-bg: #f0f8ff;
  --chip-border: #aed9fb;
  --chip-color: #1f7fc4;
}

.resource-chip.is-mcp {
  --chip-bg: #f6f7ef;
  --chip-border: #d7dd9e;
  --chip-color: #727b13;
}

.resource-chip.is-web {
  --chip-bg: #edf9fb;
  --chip-border: #a8dee8;
  --chip-color: #168596;
}

.resource-chip.is-image {
  --chip-bg: #fff0fb;
  --chip-border: #f0b5de;
  --chip-color: #bd4b99;
}

.resource-chip__icon {
  display: grid;
  width: 26px;
  height: 26px;
  place-items: center;
  border-radius: 7px;
  background: #ffffff;
  color: var(--chip-color);
  font-size: 15px;
}

.resource-chip__text {
  display: flex;
  min-width: 0;
  flex-direction: column;
  gap: 1px;
}

.resource-chip__type {
  color: var(--chip-color);
  font-size: 11px;
  line-height: 1;
}

.resource-chip__name {
  overflow: hidden;
  color: #182720;
  font-size: 13px;
  font-weight: 650;
  line-height: 1.2;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.resource-chip__remove {
  display: grid;
  width: 18px;
  height: 18px;
  place-items: center;
  border: 0;
  border-radius: 50%;
  background: rgba(255, 255, 255, 0.75);
  color: #6d7b75;
  padding: 0;
}

.resource-clear-chip {
  display: inline-flex;
  min-height: 38px;
  flex: 0 0 auto;
  align-items: center;
  gap: 5px;
  border: 1px solid #d7e2dc;
  border-radius: 8px;
  background: #fff;
  color: #5c7067;
  font-size: 13px;
  font-weight: 560;
  padding: 0 12px;
}
</style>
