<template>
  <div class="wiki-indexing-disclosure">
    <button
      type="button"
      class="disclosure-trigger"
      data-testid="wiki-advanced-disclosure"
      :aria-expanded="expanded"
      @click="expanded = !expanded"
    >
      <span class="trigger-leading">
        <t-icon :name="expanded ? 'chevron-down' : 'chevron-right'" size="16px" />
        <span class="trigger-copy">
          <span class="trigger-title">{{ $t('knowledgeEditor.indexing.advancedTitle') }}</span>
          <span class="trigger-desc">{{ $t('knowledgeEditor.indexing.advancedDesc') }}</span>
        </span>
      </span>
      <span v-if="wikiEnabled" class="enabled-summary">
        {{ $t('knowledgeEditor.indexing.advancedEnabledSummary', { granularity: granularityLabel }) }}
      </span>
    </button>

    <Transition name="wiki-disclosure">
      <div
        v-if="expanded"
        class="wiki-panel"
        :class="{ 'is-unavailable': !wikiAvailable }"
        data-testid="wiki-indexing-panel"
      >
        <div class="wiki-panel-header">
          <div class="wiki-panel-copy">
            <label class="wiki-title">{{ $t('knowledgeEditor.indexing.wikiTitle') }}</label>
            <p class="wiki-desc">{{ $t('knowledgeEditor.indexing.wikiDesc') }}</p>
          </div>
          <t-tooltip
            v-if="!wikiAvailable"
            :content="$t('knowledgeEditor.indexing.wikiUnavailable')"
            placement="top"
          >
            <span class="wiki-disabled-switch">
              <t-switch
                :model-value="wikiEnabled"
                disabled
                :aria-label="$t('knowledgeEditor.indexing.wikiTitle')"
                data-testid="wiki-indexing-switch"
                size="medium"
              />
            </span>
          </t-tooltip>
          <span v-else class="wiki-switch">
            <t-switch
              :model-value="wikiEnabled"
              :disabled="locked"
              :aria-label="$t('knowledgeEditor.indexing.wikiTitle')"
              data-testid="wiki-indexing-switch"
              size="medium"
              @change="emit('toggle-wiki')"
            />
          </span>
        </div>

        <div class="wiki-cost-note">
          <t-icon name="info-circle" size="16px" />
          <span>{{ $t('knowledgeEditor.indexing.wikiCostDesc') }}</span>
        </div>

        <div v-if="wikiEnabled" class="wiki-settings">
          <label class="setting-label">{{ $t('knowledgeEditor.wiki.extractionGranularityLabel') }}</label>
          <p class="setting-tip">{{ $t('knowledgeEditor.wiki.extractionGranularityTip') }}</p>
          <t-radio-group
            :value="resolvedGranularity"
            class="granularity-radio-group"
            @change="(value: string | number | boolean) => emit('change-granularity', value)"
          >
            <t-radio-button value="focused">
              {{ $t('knowledgeEditor.wiki.granularityFocused') }}
            </t-radio-button>
            <t-radio-button value="standard">
              {{ $t('knowledgeEditor.wiki.granularityStandard') }}
            </t-radio-button>
            <t-radio-button value="exhaustive">
              {{ $t('knowledgeEditor.wiki.granularityExhaustive') }}
            </t-radio-button>
          </t-radio-group>
          <p class="setting-tip granularity-hint">{{ granularityHint }}</p>
        </div>
      </div>
    </Transition>
  </div>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'

import {
  getInitialWikiDisclosureExpanded,
  syncWikiDisclosureExpanded,
} from '../disclosureState'

type Granularity = 'focused' | 'standard' | 'exhaustive'

const props = defineProps<{
  wikiEnabled: boolean
  locked: boolean
  wikiAvailable: boolean
  resolvedGranularity: Granularity
  granularityHint: string
}>()

const emit = defineEmits<{
  (event: 'toggle-wiki'): void
  (event: 'change-granularity', value: string | number | boolean): void
}>()

const { t } = useI18n()
const expanded = ref(getInitialWikiDisclosureExpanded(props.wikiEnabled))

const granularityLabel = computed(() => {
  switch (props.resolvedGranularity) {
    case 'focused':
      return t('knowledgeEditor.wiki.granularityFocused')
    case 'exhaustive':
      return t('knowledgeEditor.wiki.granularityExhaustive')
    default:
      return t('knowledgeEditor.wiki.granularityStandard')
  }
})

watch(
  () => props.wikiEnabled,
  (enabled) => {
    expanded.value = syncWikiDisclosureExpanded(expanded.value, enabled)
  },
)
</script>

<style scoped lang="less">
.wiki-indexing-disclosure {
  margin-top: 12px;
}

.disclosure-trigger {
  width: 100%;
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 16px;
  padding: 10px 12px;
  border: 1px solid var(--td-component-stroke);
  border-radius: 8px;
  color: var(--td-text-color-primary);
  background: var(--td-bg-color-secondarycontainer);
  cursor: pointer;
  text-align: left;
  transition: border-color 0.2s ease, background 0.2s ease;

  &:hover {
    border-color: var(--td-brand-color);
    background: var(--td-bg-color-container-hover);
  }
}

.trigger-leading {
  min-width: 0;
  display: flex;
  align-items: center;
  gap: 8px;
}

.trigger-copy {
  min-width: 0;
  display: flex;
  flex-direction: column;
  gap: 2px;
}

.trigger-title {
  font-size: 14px;
  font-weight: 500;
  line-height: 20px;
}

.trigger-desc {
  color: var(--td-text-color-placeholder);
  font-size: 12px;
  line-height: 18px;
}

.enabled-summary {
  flex-shrink: 0;
  padding: 2px 8px;
  border-radius: 10px;
  color: var(--td-brand-color);
  background: var(--td-brand-color-light);
  font-size: 12px;
  line-height: 18px;
}

.wiki-panel {
  margin-top: 8px;
  padding: 14px;
  border: 1px solid var(--td-component-stroke);
  border-radius: 8px;
  background: var(--td-bg-color-container);

  &.is-unavailable {
    .wiki-panel-copy,
    .wiki-cost-note {
      opacity: 0.58;
    }
  }
}

.wiki-panel-header {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 20px;
}

.wiki-panel-copy {
  min-width: 0;
}

.wiki-disabled-switch,
.wiki-switch {
  display: inline-flex;
  flex-shrink: 0;
  padding: 2px;
}

.wiki-disabled-switch {
  cursor: not-allowed;
}

.wiki-title,
.setting-label {
  display: block;
  color: var(--td-text-color-primary);
  font-size: 14px;
  font-weight: 500;
  line-height: 22px;
}

.wiki-desc,
.setting-tip {
  margin: 4px 0 0;
  color: var(--td-text-color-placeholder);
  font-size: 12px;
  line-height: 18px;
}

.wiki-cost-note {
  display: flex;
  align-items: flex-start;
  gap: 7px;
  margin-top: 12px;
  padding: 9px 10px;
  border-radius: 6px;
  color: var(--td-warning-color);
  background: var(--td-warning-color-light);
  font-size: 12px;
  line-height: 18px;

  :deep(.t-icon) {
    flex-shrink: 0;
    margin-top: 1px;
  }
}

.wiki-settings {
  margin-top: 16px;
  padding-top: 14px;
  border-top: 1px solid var(--td-component-stroke);
}

.granularity-radio-group {
  margin-top: 10px;
}

.granularity-hint {
  margin-top: 8px;
  color: var(--td-text-color-secondary);
  line-height: 1.6;
  white-space: normal;
  word-break: break-word;
}

.wiki-disclosure-enter-active,
.wiki-disclosure-leave-active {
  transition: opacity 0.16s ease, transform 0.16s ease;
}

.wiki-disclosure-enter-from,
.wiki-disclosure-leave-to {
  opacity: 0;
  transform: translateY(-4px);
}

@media (max-width: 720px) {
  .disclosure-trigger,
  .wiki-panel-header {
    align-items: stretch;
  }

  .disclosure-trigger {
    flex-direction: column;
    gap: 8px;
  }

  .enabled-summary {
    align-self: flex-start;
  }
}
</style>
