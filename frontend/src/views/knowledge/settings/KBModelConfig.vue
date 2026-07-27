<template>
  <div class="kb-model-config">
    <div class="section-header">
      <h2>{{ $t('knowledgeEditor.models.title') }}</h2>
      <p class="section-description">{{ $t('knowledgeEditor.models.description') }}</p>
    </div>

    <div class="settings-group">
      <!-- LLM 大语言模型 -->
      <div class="setting-row" data-guide="kb-create-llm">
        <div class="setting-info">
          <label>{{ $t('knowledgeEditor.models.llmLabel') }} <span class="required">*</span></label>
          <p class="desc">{{ $t('knowledgeEditor.models.llmDesc') }}</p>
        </div>
        <div class="setting-control">
          <ModelSelector
            ref="llmSelectorRef"
            model-type="KnowledgeQA"
            :selected-model-id="config.llmModelId"
            :all-models="allModels"
            @update:selected-model-id="handleLLMChange"
            @add-model="handleAddModel('chat')"
            :placeholder="$t('knowledgeEditor.models.llmPlaceholder')"
          />
        </div>
      </div>

      <!-- Embedding 嵌入模型: RAG 检索启用时必填; 纯 Wiki 时可选(用于目录归类相似度) -->
      <div v-if="ragEnabled !== false || wikiEnabled" class="setting-row" data-guide="kb-create-embedding">
        <div class="setting-info">
          <label>
            {{ $t('knowledgeEditor.models.embeddingLabel') }}
            <span v-if="ragEnabled" class="required">*</span>
            <span v-else-if="wikiEnabled" class="optional">{{ $t('knowledgeEditor.models.embeddingOptional') }}</span>
          </label>
          <p class="desc">
            {{ (wikiEnabled && ragEnabled === false)
              ? $t('knowledgeEditor.models.embeddingWikiOptionalDesc')
              : $t('knowledgeEditor.models.embeddingDesc') }}
          </p>
          <t-alert
            v-if="ragEnabled && hasFiles"
            theme="warning"
            :message="$t('knowledgeEditor.models.embeddingLocked')"
            style="margin-top: 8px;"
          />
        </div>
        <div class="setting-control">
          <ModelSelector
            ref="embeddingSelectorRef"
            model-type="Embedding"
            :selected-model-id="config.embeddingModelId"
            :all-models="allModels"
            :disabled="ragEnabled && hasFiles"
            @update:selected-model-id="handleEmbeddingChange"
            @add-model="handleAddModel('embedding')"
            :placeholder="$t('knowledgeEditor.models.embeddingPlaceholder')"
          />
        </div>
      </div>

      <!-- 平台下发的衍生任务专用模型 -->
      <div class="setting-row" data-testid="kb-derivative-model-row">
        <div class="setting-info">
          <label>衍生任务模型</label>
          <p class="desc">
            用于摘要、问题生成、知识图谱和 Wiki。这里只显示系统管理员下发的专用模型，
            不会使用或回退到上面的对话模型。
          </p>
          <t-alert
            v-if="!derivativeLoading && derivativeModels.length === 0"
            theme="warning"
            message="平台尚未下发衍生任务模型；向量检索仍可使用，衍生任务会等待管理员配置。"
            style="margin-top: 8px"
          />
        </div>
        <div class="setting-control">
          <ModelSelector
            model-type="KnowledgeQA"
            usage-scope="derivative"
            :selected-model-id="config.derivativeModelId"
            :all-models="derivativeModels"
            :disabled="derivativeLoading || derivativeModels.length === 0"
            @update:selected-model-id="handleDerivativeModelChange"
            placeholder="选择管理员下发的衍生任务模型"
          />
        </div>
      </div>

    </div>
  </div>
</template>

<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { useUIStore } from '@/stores/ui'
import ModelSelector from '@/components/ModelSelector.vue'
import { getDerivativeStatus, type DerivativeModel } from '@/api/derivative-control'

interface ModelConfig {
  llmModelId?: string
  embeddingModelId?: string
  vllmModelId?: string
  derivativeModelId?: string
}

interface Props {
  config: ModelConfig
  hasFiles: boolean
  wikiEnabled?: boolean
  ragEnabled?: boolean
  allModels?: any[]
}

const props = defineProps<Props>()

const emit = defineEmits<{
  'update:config': [value: ModelConfig]
}>()

const uiStore = useUIStore()

const llmSelectorRef = ref<InstanceType<typeof ModelSelector>>()
const embeddingSelectorRef = ref<InstanceType<typeof ModelSelector>>()
const derivativeModels = ref<DerivativeModel[]>([])
const derivativeLoading = ref(false)

const handleLLMChange = (modelId: string) => {
  emit('update:config', {
    ...props.config,
    llmModelId: modelId
  })
}

const handleEmbeddingChange = (modelId: string) => {
  emit('update:config', {
    ...props.config,
    embeddingModelId: modelId
  })
}

const handleDerivativeModelChange = (modelId: string) => {
  emit('update:config', {
    ...props.config,
    derivativeModelId: modelId
  })
}

const loadDerivativeModels = async () => {
  derivativeLoading.value = true
  try {
    const status = await getDerivativeStatus()
    derivativeModels.value = status.models || []
    const selectedIsPublished = derivativeModels.value.some(
      model => model.id === props.config.derivativeModelId,
    )
    if (!selectedIsPublished) {
      // Old KB rows may still carry a former conversation-model ID in the
      // legacy Wiki field. Never keep that value selected: replace it with
      // the platform derivative default, or clear it so the server can hold
      // the task until an administrator publishes one.
      handleDerivativeModelChange(status.default_model_id || '')
    }
  } catch (error) {
    console.error('Failed to load derivative models', error)
    derivativeModels.value = []
  } finally {
    derivativeLoading.value = false
  }
}

const handleAddModel = (subSection: string) => {
  uiStore.openSettings('models', subSection)
}

onMounted(loadDerivativeModels)
</script>

<style lang="less" scoped>
.kb-model-config {
  width: 100%;
}

.section-header {
  margin-bottom: 20px;

  h2 {
    font-size: 20px;
    font-weight: 600;
    color: var(--td-text-color-primary);
    margin: 0 0 6px 0;
  }

  .section-description {
    font-size: 14px;
    color: var(--td-text-color-secondary);
    margin: 0;
    line-height: 1.5;
  }
}

.settings-group {
  display: flex;
  flex-direction: column;
  gap: 0;
}

.setting-row {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  padding: 16px 0;
  border-bottom: 1px solid var(--td-component-stroke);

  &:last-child {
    border-bottom: none;
  }
}

.setting-info {
  flex: 0 0 40%;
  max-width: 40%;
  padding-right: 24px;

  label {
    font-size: 15px;
    font-weight: 500;
    color: var(--td-text-color-primary);
    display: block;
    margin-bottom: 4px;

    .required {
      color: var(--td-error-color);
      margin-left: 2px;
    }

    .optional {
      color: var(--td-text-color-placeholder);
      font-size: 12px;
      font-weight: 400;
      margin-left: 4px;
    }
  }

  .desc {
    font-size: 13px;
    color: var(--td-text-color-secondary);
    margin: 0;
    line-height: 1.5;
  }
}

.setting-control {
  flex: 0 0 55%;
  max-width: 55%;
  display: flex;
  justify-content: flex-end;
  align-items: flex-start;
}
</style>

