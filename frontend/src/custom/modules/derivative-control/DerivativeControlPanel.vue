<template>
  <section class="derivative-control" data-testid="derivative-control-panel">
    <div class="derivative-control__header">
      <div>
        <div class="derivative-control__title-row">
          <h3>衍生任务模型池</h3>
          <t-tag theme="primary" variant="light">全平台</t-tag>
          <t-tag :theme="config?.configured ? 'success' : 'warning'" variant="light">
            {{ config?.configured ? '已生效' : '未配置' }}
          </t-tag>
        </div>
        <p>
          Wiki、知识图谱、摘要和问题生成只使用这里下发的专用模型；所有租户共享同一 TPM，
          不会回退到对话模型。
        </p>
      </div>
      <t-button variant="text" :loading="loading" @click="load">刷新</t-button>
    </div>

    <t-alert
      theme="info"
      message="下发前会校验：模型未被知识库或 Agent 使用，且 Base URL 与所有对话模型端点物理隔离。"
    />

    <div class="derivative-control__settings">
      <div class="field">
        <label for="derivative-tpm-input">全平台衍生任务 TPM</label>
        <div class="field__control">
          <t-input-number
            id="derivative-tpm-input"
            v-model="tpm"
            :min="100"
            :max="2000000"
            :step="1000"
            theme="normal"
            data-testid="derivative-tpm-input"
          />
          <t-button
            theme="primary"
            :loading="savingTPM"
            data-testid="derivative-tpm-save"
            @click="saveTPM"
          >
            保存 TPM
          </t-button>
        </div>
        <span>默认 20,000；修改后通过共享 Redis 准入在所有实例动态生效。</span>
      </div>

      <div class="field">
        <label>下发专用模型</label>
        <div class="field__control field__control--wide">
          <t-select
            v-model="candidateID"
            filterable
            placeholder="选择一个独立端点的候选模型"
            data-testid="derivative-candidate-select"
          >
            <t-option
              v-for="candidate in availableCandidates"
              :key="candidate.id"
              :value="candidate.id"
              :label="candidateLabel(candidate)"
              :disabled="!candidate.eligible"
            >
              <div class="candidate-option">
                <span>{{ candidateLabel(candidate) }}</span>
                <small v-if="candidate.reason">{{ candidate.reason }}</small>
              </div>
            </t-option>
          </t-select>
          <t-button
            theme="primary"
            :disabled="!selectedCandidate?.eligible"
            :loading="publishing"
            data-testid="derivative-publish-button"
            @click="publish"
          >
            下发
          </t-button>
        </div>
        <span v-if="selectedCandidate?.reason" class="field__error">
          {{ selectedCandidate.reason }}
        </span>
      </div>
    </div>

    <div class="derivative-control__pool">
      <div v-if="!config?.models.length" class="empty">
        尚未下发专用模型。知识库的衍生任务会安全等待，不会借用对话模型。
      </div>
      <div
        v-for="model in config?.models || []"
        :key="model.id"
        class="pool-row"
        :data-testid="`derivative-model-${model.id}`"
      >
        <div class="pool-row__main">
          <strong>{{ displayName(model) }}</strong>
          <t-tag v-if="model.is_derivative_default" theme="success" variant="light">默认</t-tag>
          <t-tag theme="warning" variant="light">仅衍生任务</t-tag>
          <span>租户 {{ model.tenant_id }}</span>
        </div>
        <div class="pool-row__actions">
          <t-button
            v-if="!model.is_derivative_default"
            size="small"
            variant="outline"
            @click="makeDefault(model.id)"
          >
            设为默认
          </t-button>
          <t-button
            size="small"
            variant="outline"
            :loading="testingID === model.id"
            @click="testModel(model.id)"
          >
            测试
          </t-button>
          <t-popconfirm
            content="撤回后，已明确选择该模型的知识库将被服务端阻止；请先完成改绑。"
            @confirm="unpublish(model.id)"
          >
            <t-button size="small" theme="danger" variant="text">撤回</t-button>
          </t-popconfirm>
        </div>
      </div>
    </div>
  </section>
</template>

<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { MessagePlugin } from 'tdesign-vue-next'
import {
  getDerivativeAdminConfig,
  publishDerivativeModel,
  setDefaultDerivativeModel,
  testDerivativeModel,
  unpublishDerivativeModel,
  updateDerivativeTPM,
  type DerivativeAdminConfig,
  type DerivativeCandidate,
  type DerivativeModel,
} from '@/api/derivative-control'

const emit = defineEmits<{
  changed: []
}>()

const config = ref<DerivativeAdminConfig | null>(null)
const loading = ref(false)
const savingTPM = ref(false)
const publishing = ref(false)
const testingID = ref('')
const tpm = ref(20_000)
const candidateID = ref('')

const availableCandidates = computed(() =>
  (config.value?.candidates || []).filter(candidate => !candidate.assigned),
)

const selectedCandidate = computed(() =>
  availableCandidates.value.find(candidate => candidate.id === candidateID.value),
)

function displayName(model: Pick<DerivativeModel, 'display_name' | 'name'>): string {
  return model.display_name?.trim() || model.name
}

function candidateLabel(candidate: DerivativeCandidate): string {
  const name = candidate.display_name?.trim() || candidate.name
  const endpoint = candidate.origin ? ` · ${candidate.origin}` : ''
  return `${name} · 租户 ${candidate.tenant_id}${endpoint}`
}

async function load() {
  loading.value = true
  try {
    config.value = await getDerivativeAdminConfig()
    tpm.value = config.value.tpm || 20_000
    if (!selectedCandidate.value) candidateID.value = ''
  } catch (error: any) {
    MessagePlugin.error(error?.message || '加载衍生任务模型配置失败')
  } finally {
    loading.value = false
  }
}

async function saveTPM() {
  if (!Number.isInteger(tpm.value) || tpm.value < 100 || tpm.value > 2_000_000) {
    MessagePlugin.warning('TPM 必须是 100 到 2,000,000 之间的整数')
    return
  }
  savingTPM.value = true
  try {
    tpm.value = await updateDerivativeTPM(tpm.value)
    if (config.value) config.value.tpm = tpm.value
    MessagePlugin.success('衍生任务 TPM 已全局生效')
  } catch (error: any) {
    MessagePlugin.error(error?.message || '保存 TPM 失败')
  } finally {
    savingTPM.value = false
  }
}

async function publish() {
  const candidate = selectedCandidate.value
  if (!candidate?.eligible || !candidate.id || !candidate.tenant_id) return
  publishing.value = true
  try {
    config.value = await publishDerivativeModel(candidate.id, candidate.tenant_id)
    candidateID.value = ''
    MessagePlugin.success('专用模型已下发到所有知识库')
    emit('changed')
  } catch (error: any) {
    MessagePlugin.error(error?.message || '下发专用模型失败')
  } finally {
    publishing.value = false
  }
}

async function makeDefault(modelID: string) {
  try {
    config.value = await setDefaultDerivativeModel(modelID)
    MessagePlugin.success('默认衍生任务模型已更新')
    emit('changed')
  } catch (error: any) {
    MessagePlugin.error(error?.message || '设置默认模型失败')
  }
}

async function unpublish(modelID: string) {
  try {
    config.value = await unpublishDerivativeModel(modelID)
    MessagePlugin.success('模型已从下发列表撤回')
    emit('changed')
  } catch (error: any) {
    MessagePlugin.error(error?.message || '撤回模型失败')
  }
}

async function testModel(modelID: string) {
  testingID.value = modelID
  try {
    const result = await testDerivativeModel(modelID)
    MessagePlugin.success(`测试成功（${result.elapsed_ms}ms）：${result.content || 'OK'}`)
  } catch (error: any) {
    MessagePlugin.error(error?.message || '专用模型测试失败')
  } finally {
    testingID.value = ''
  }
}

onMounted(load)
</script>

<style scoped lang="less">
.derivative-control {
  margin: 0 0 24px;
  padding: 20px;
  border: 1px solid var(--td-component-stroke);
  border-radius: 10px;
  background: var(--td-bg-color-container);
}

.derivative-control__header,
.derivative-control__title-row,
.field__control,
.pool-row,
.pool-row__main,
.pool-row__actions {
  display: flex;
  align-items: center;
}

.derivative-control__header {
  justify-content: space-between;
  gap: 24px;
  margin-bottom: 14px;

  h3,
  p {
    margin: 0;
  }

  p {
    margin-top: 6px;
    color: var(--td-text-color-secondary);
    font-size: 13px;
  }
}

.derivative-control__title-row {
  gap: 8px;
}

.derivative-control__settings {
  display: grid;
  grid-template-columns: minmax(260px, 0.8fr) minmax(420px, 1.4fr);
  gap: 24px;
  margin-top: 18px;
}

.field {
  display: flex;
  flex-direction: column;
  gap: 8px;

  label {
    font-weight: 600;
  }

  > span {
    color: var(--td-text-color-placeholder);
    font-size: 12px;
  }

  .field__error {
    color: var(--td-error-color);
  }
}

.field__control {
  gap: 8px;

  :deep(.t-input-number) {
    width: 180px;
  }
}

.field__control--wide :deep(.t-select__wrap) {
  flex: 1;
}

.candidate-option {
  display: flex;
  flex-direction: column;

  small {
    color: var(--td-error-color);
  }
}

.derivative-control__pool {
  margin-top: 18px;
  border-top: 1px solid var(--td-component-stroke);
}

.pool-row {
  justify-content: space-between;
  gap: 20px;
  min-height: 54px;
  border-bottom: 1px solid var(--td-component-stroke);
}

.pool-row__main,
.pool-row__actions {
  gap: 8px;
}

.pool-row__main > span {
  color: var(--td-text-color-secondary);
  font-size: 12px;
}

.empty {
  padding: 18px 0 0;
  color: var(--td-text-color-secondary);
}

@media (max-width: 900px) {
  .derivative-control__settings {
    grid-template-columns: 1fr;
  }

  .pool-row {
    align-items: flex-start;
    flex-direction: column;
    padding: 12px 0;
  }
}
</style>
