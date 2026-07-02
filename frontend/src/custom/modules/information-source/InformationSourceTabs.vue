<template>
  <nav class="information-source-tabs" :aria-label="t('menu.informationSources')">
    <button
      v-for="tab in tabs"
      :key="tab.value"
      type="button"
      class="information-source-tab"
      :class="{ active: activeTab === tab.value }"
      :aria-current="activeTab === tab.value ? 'page' : undefined"
      @click="goTo(tab.path)"
    >
      {{ tab.label }}
    </button>
  </nav>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'

type InformationSourceTab = 'knowledge' | 'data'

const route = useRoute()
const router = useRouter()
const { t } = useI18n()

const tabs = computed(() => [
  {
    value: 'knowledge' as const,
    label: t('menu.knowledgeBase'),
    path: '/platform/knowledge-bases',
  },
  {
    value: 'data' as const,
    label: t('menu.dataSources'),
    path: '/platform/data-sources',
  },
])

const activeTab = computed<InformationSourceTab>(() => (
  route.name === 'dataSourceList' ? 'data' : 'knowledge'
))

const goTo = (path: string) => {
  if (route.path !== path) {
    router.push(path)
  }
}
</script>

<style scoped lang="less">
.information-source-tabs {
  --wails-draggable: no-drag;
  display: inline-flex;
  align-items: center;
  gap: 22px;
  border-bottom: 1px solid var(--td-component-stroke);
  font-family: var(--app-font-family);
}

.information-source-tab {
  position: relative;
  height: 32px;
  padding: 0 0 9px;
  border: 0;
  background: transparent;
  color: var(--td-text-color-secondary);
  font-family: inherit;
  font-size: 14px;
  font-weight: 500;
  line-height: 20px;
  cursor: pointer;
  transition: color 0.18s ease;

  &:hover {
    color: var(--td-text-color-primary);
  }

  &.active {
    color: var(--td-brand-color);
    font-weight: 600;

    &::after {
      content: '';
      position: absolute;
      right: 0;
      bottom: -1px;
      left: 0;
      height: 2px;
      border-radius: 1px;
      background: var(--td-brand-color);
    }
  }
}
</style>
