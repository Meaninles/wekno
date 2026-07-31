<template>
  <section class="admission-panel" data-testid="derivative-control-panel">
    <header class="admission-panel__header">
      <div>
        <h3>实际模型资源池</h3>
        <p>并发、TPM 和熔断按 provider + 规范化地址 + 实际模型独立生效；修改即时热更新。</p>
      </div>
      <div class="admission-panel__actions">
        <t-tag theme="primary" variant="light">{{ pools.length }} 个资源池</t-tag>
        <t-tag variant="light">{{ bindings.length }} 条绑定</t-tag>
        <t-button variant="outline" :loading="reconciling" @click="reconcile">重新识别</t-button>
        <t-button variant="text" :loading="loading" @click="load">刷新</t-button>
      </div>
    </header>

    <t-alert
      v-if="error"
      theme="error"
      :message="error"
      close
      @close="error = ''"
    />

    <t-tabs v-model="activeTab" class="admission-panel__tabs">
      <t-tab-panel value="pools" label="资源池">
        <div class="table-scroll">
          <table data-testid="resource-pool-table">
            <thead>
              <tr>
                <th>实际模型 / 类型</th>
                <th>状态</th>
                <th>总并发</th>
                <th>后台并发</th>
                <th>交互预留</th>
                <th>文档突发</th>
                <th>TPM</th>
                <th>熔断</th>
                <th>版本</th>
                <th>操作</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="pool in pools" :key="pool.id" :data-pool-id="pool.id">
                <td class="pool-name">
                  <strong>{{ pool.name }}</strong>
                  <small>{{ pool.resource_kind }} · {{ pool.id }}</small>
                </td>
                <td>
                  <t-select v-model="pool.state" size="small" :options="stateOptions" />
                </td>
                <td><t-input-number v-model="pool.max_inflight" :min="1" :max="1024" size="small" /></td>
                <td><t-input-number v-model="pool.max_background_inflight" :min="0" :max="pool.max_inflight" size="small" /></td>
                <td><t-input-number v-model="pool.interactive_reserve" :min="0" :max="pool.max_inflight" size="small" /></td>
                <td><t-input-number v-model="pool.document_burst" :min="1" :max="pool.max_inflight" size="small" /></td>
                <td><t-input-number v-model="pool.tpm" :min="0" :max="2000000" :step="1000" size="small" /></td>
                <td class="circuit-cell">
                  <span>{{ pool.circuit_threshold }} 次 / {{ pool.circuit_window_seconds }}s</span>
                  <small>打开 {{ pool.circuit_open_seconds }}s</small>
                </td>
                <td>v{{ pool.policy_version }}</td>
                <td class="row-actions">
                  <t-button
                    size="small"
                    :loading="savingID === pool.id"
                    :data-testid="`resource-pool-save-${pool.id}`"
                    @click="savePool(pool)"
                  >保存</t-button>
                  <t-dropdown
                    :options="poolActions(pool)"
                    trigger="click"
                    @click="(item: any) => runPoolAction(pool, item.value)"
                  >
                    <t-button size="small" variant="text">更多</t-button>
                  </t-dropdown>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </t-tab-panel>

      <t-tab-panel value="bindings" label="模型绑定">
        <div class="binding-tools">
          <t-input v-model="bindingFilter" clearable placeholder="按模型 ID 或资源池筛选" />
          <span>显示 {{ filteredBindings.length }} / {{ bindings.length }}</span>
        </div>
        <div class="table-scroll">
          <table data-testid="resource-binding-table">
            <thead>
              <tr>
                <th>模型 ID / 租户</th>
                <th>GPU 资源池</th>
                <th>配额池</th>
                <th>网关池</th>
                <th>版本</th>
                <th>操作</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="binding in filteredBindings.slice(0, 200)" :key="`${binding.model_tenant_id}:${binding.model_id}`">
                <td class="pool-name"><strong>{{ binding.model_id }}</strong><small>tenant {{ binding.model_tenant_id }}</small></td>
                <td>
                  <t-select v-model="binding.resource_pool_id" filterable :options="resourcePoolOptions" size="small" />
                </td>
                <td><t-select v-model="binding.quota_pool_id" clearable :options="quotaPoolOptions" size="small" /></td>
                <td><t-select v-model="binding.gateway_pool_id" clearable :options="gatewayPoolOptions" size="small" /></td>
                <td>v{{ binding.binding_version }}</td>
                <td>
                  <t-button size="small" :loading="savingID === binding.model_id" @click="saveBinding(binding)">保存</t-button>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
        <p v-if="filteredBindings.length > 200" class="limit-hint">为避免浏览器卡顿，当前最多展示前 200 条，请用筛选缩小范围。</p>
      </t-tab-panel>

      <t-tab-panel value="templates" label="默认模板">
        <div class="table-scroll">
          <table data-testid="admission-template-table">
            <thead>
              <tr>
                <th>用途</th>
                <th>总并发</th>
                <th>后台并发</th>
                <th>交互预留</th>
                <th>租户突发</th>
                <th>文档突发</th>
                <th>TPM</th>
                <th>版本</th>
                <th>操作</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="template in templates" :key="template.kind">
                <td><strong>{{ template.kind }}</strong></td>
                <td><t-input-number v-model="template.max_inflight" :min="1" :max="1024" size="small" /></td>
                <td><t-input-number v-model="template.max_background_inflight" :min="0" :max="template.max_inflight" size="small" /></td>
                <td><t-input-number v-model="template.interactive_reserve" :min="0" :max="template.max_inflight" size="small" /></td>
                <td><t-input-number v-model="template.tenant_burst" :min="1" :max="template.max_inflight" size="small" /></td>
                <td><t-input-number v-model="template.document_burst" :min="1" :max="template.max_inflight" size="small" /></td>
                <td><t-input-number v-model="template.tpm" :min="0" :max="2000000" :step="1000" size="small" /></td>
                <td>v{{ template.policy_version }}</td>
                <td><t-button size="small" :loading="savingID === template.kind" @click="saveTemplate(template)">保存</t-button></td>
              </tr>
            </tbody>
          </table>
        </div>
      </t-tab-panel>

      <t-tab-panel value="audits" label="审计">
        <div class="audit-list">
          <div v-for="audit in audits.slice(0, 100)" :key="audit.id" class="audit-row">
            <strong>{{ audit.action }}</strong>
            <span>{{ audit.resource_type }} / {{ audit.resource_id }}</span>
            <small>v{{ audit.policy_version }} · {{ formatTime(audit.created_at) }}</small>
          </div>
          <t-empty v-if="!audits.length && !loading" description="暂无配置审计" />
        </div>
      </t-tab-panel>
    </t-tabs>
  </section>
</template>

<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { DialogPlugin, MessagePlugin } from 'tdesign-vue-next'
import {
  drainModelResourcePool,
  listModelAdmissionAudits,
  listModelAdmissionTemplates,
  listModelGatewayPools,
  listModelQuotaPools,
  listModelResourceBindings,
  listModelResourcePools,
  reconcileModelResourcePools,
  resetModelResourcePool,
  updateModelAdmissionTemplate,
  updateModelResourceBinding,
  updateModelResourcePool,
  type ModelAdmissionAudit,
  type ModelAdmissionTemplate,
  type ModelGatewayPool,
  type ModelQuotaPool,
  type ModelResourceBinding,
  type ModelResourcePool,
} from '@/api/derivative-control'

const activeTab = ref('pools')
const loading = ref(false)
const reconciling = ref(false)
const savingID = ref('')
const error = ref('')
const bindingFilter = ref('')
const pools = ref<ModelResourcePool[]>([])
const bindings = ref<ModelResourceBinding[]>([])
const templates = ref<ModelAdmissionTemplate[]>([])
const quotaPools = ref<ModelQuotaPool[]>([])
const gatewayPools = ref<ModelGatewayPool[]>([])
const audits = ref<ModelAdmissionAudit[]>([])

const stateOptions = [
  { label: '启用', value: 'enabled' },
  { label: '排空', value: 'draining' },
  { label: '禁用', value: 'disabled' },
]
const resourcePoolOptions = computed(() => pools.value.map(row => ({ label: `${row.name} (${row.id})`, value: row.id })))
const quotaPoolOptions = computed(() => quotaPools.value.map(row => ({ label: row.name, value: row.id })))
const gatewayPoolOptions = computed(() => gatewayPools.value.map(row => ({ label: row.name, value: row.id })))
const filteredBindings = computed(() => {
  const needle = bindingFilter.value.trim().toLowerCase()
  if (!needle) return bindings.value
  return bindings.value.filter(row =>
    row.model_id.toLowerCase().includes(needle)
    || row.resource_pool_id.toLowerCase().includes(needle)
    || row.quota_pool_id.toLowerCase().includes(needle)
    || row.gateway_pool_id.toLowerCase().includes(needle),
  )
})

async function load() {
  loading.value = true
  error.value = ''
  try {
    const [poolRows, bindingRows, templateRows, quotaRows, gatewayRows, auditRows] = await Promise.all([
      listModelResourcePools(),
      listModelResourceBindings(),
      listModelAdmissionTemplates(),
      listModelQuotaPools(),
      listModelGatewayPools(),
      listModelAdmissionAudits(),
    ])
    pools.value = poolRows
    bindings.value = bindingRows
    templates.value = templateRows
    quotaPools.value = quotaRows
    gatewayPools.value = gatewayRows
    audits.value = auditRows
  } catch (caught: any) {
    error.value = caught?.message || '加载模型资源池失败'
  } finally {
    loading.value = false
  }
}

async function savePool(pool: ModelResourcePool) {
  savingID.value = pool.id
  try {
    const updated = await updateModelResourcePool(pool)
    Object.assign(pool, updated)
    MessagePlugin.success(`资源池“${pool.name}”已热更新`)
    audits.value = await listModelAdmissionAudits()
  } catch (caught: any) {
    MessagePlugin.error(caught?.message || '保存资源池失败')
    await load()
  } finally {
    savingID.value = ''
  }
}

function poolActions(pool: ModelResourcePool) {
  return [
    { content: '进入排空', value: 'drain', disabled: pool.state === 'draining' },
    { content: '恢复默认', value: 'reset' },
  ]
}

function runPoolAction(pool: ModelResourcePool, action: string) {
  const dialog = DialogPlugin.confirm({
    header: action === 'drain' ? '排空资源池' : '恢复默认值',
    body: action === 'drain'
      ? `排空“${pool.name}”后不再接收新请求，但不会中断在途调用。`
      : `确定按 ${pool.resource_kind} 内置模板恢复“${pool.name}”吗？`,
    confirmBtn: action === 'drain' ? '排空' : '恢复',
    cancelBtn: '取消',
    onConfirm: async () => {
      savingID.value = pool.id
      try {
        if (action === 'drain') {
          await drainModelResourcePool(pool)
        } else {
          Object.assign(pool, await resetModelResourcePool(pool))
        }
        await load()
        MessagePlugin.success(action === 'drain' ? '资源池已进入排空' : '资源池已恢复默认')
      } catch (caught: any) {
        MessagePlugin.error(caught?.message || '操作失败')
        await load()
      } finally {
        savingID.value = ''
        dialog.destroy()
      }
    },
  })
}

async function saveBinding(binding: ModelResourceBinding) {
  savingID.value = binding.model_id
  try {
    Object.assign(binding, await updateModelResourceBinding(binding))
    MessagePlugin.success('模型资源绑定已更新')
  } catch (caught: any) {
    MessagePlugin.error(caught?.message || '保存模型绑定失败')
    await load()
  } finally {
    savingID.value = ''
  }
}

async function saveTemplate(template: ModelAdmissionTemplate) {
  savingID.value = template.kind
  try {
    Object.assign(template, await updateModelAdmissionTemplate(template))
    MessagePlugin.success('默认模板已更新并完成资源池重算')
    await load()
  } catch (caught: any) {
    MessagePlugin.error(caught?.message || '保存默认模板失败')
    await load()
  } finally {
    savingID.value = ''
  }
}

async function reconcile() {
  reconciling.value = true
  try {
    await reconcileModelResourcePools()
    await load()
    MessagePlugin.success('模型路由与资源池绑定已重新识别')
  } catch (caught: any) {
    MessagePlugin.error(caught?.message || '重新识别失败')
  } finally {
    reconciling.value = false
  }
}

function formatTime(value: string) {
  return value ? new Date(value).toLocaleString() : '-'
}

onMounted(load)
</script>

<style scoped lang="less">
.admission-panel {
  margin: -4px 0 18px;
  padding: 14px;
  border: 1px solid var(--td-component-stroke);
  border-radius: 10px;
  background: var(--td-bg-color-container);
}

.admission-panel__header,
.admission-panel__actions,
.binding-tools {
  display: flex;
  align-items: center;
  gap: 10px;
}

.admission-panel__header {
  justify-content: space-between;
  margin-bottom: 12px;

  h3 { margin: 0 0 4px; font-size: 15px; }
  p { margin: 0; color: var(--td-text-color-secondary); font-size: 12px; }
}

.admission-panel__actions { flex-wrap: wrap; justify-content: flex-end; }
.admission-panel__tabs { margin-top: 10px; }
.table-scroll { overflow-x: auto; }

table {
  width: 100%;
  min-width: 1120px;
  border-collapse: collapse;
  font-size: 12px;
}

th,
td {
  padding: 8px 6px;
  border-bottom: 1px solid var(--td-component-stroke);
  text-align: left;
  vertical-align: middle;
}

th {
  color: var(--td-text-color-secondary);
  font-weight: 500;
  white-space: nowrap;
}

.pool-name {
  min-width: 180px;

  strong,
  small { display: block; }
  small { margin-top: 3px; color: var(--td-text-color-placeholder); word-break: break-all; }
}

.circuit-cell {
  white-space: nowrap;
  span,
  small { display: block; }
  small { color: var(--td-text-color-placeholder); }
}

.row-actions { white-space: nowrap; }
:deep(.t-input-number) { width: 92px; }
:deep(.t-select) { min-width: 112px; }

.binding-tools {
  justify-content: space-between;
  padding: 10px 0;

  :deep(.t-input) { max-width: 360px; }
  span { color: var(--td-text-color-secondary); font-size: 12px; }
}

.limit-hint { color: var(--td-text-color-placeholder); font-size: 12px; }
.audit-list { max-height: 420px; overflow: auto; }
.audit-row {
  display: grid;
  grid-template-columns: 90px minmax(220px, 1fr) auto;
  gap: 12px;
  padding: 9px 4px;
  border-bottom: 1px solid var(--td-component-stroke);
  font-size: 12px;

  small { color: var(--td-text-color-placeholder); }
}

@media (max-width: 900px) {
  .admission-panel__header { align-items: flex-start; flex-direction: column; }
  .admission-panel__actions { justify-content: flex-start; }
}
</style>
