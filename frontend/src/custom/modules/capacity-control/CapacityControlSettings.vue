<template>
  <section class="capacity-page" data-testid="capacity-control-settings">
    <header class="page-header">
      <div>
        <h2>容量与调度</h2>
        <p>以实际模型路由为单位统一配置并发、排队和 Token 预算。后台任务等待容量不会计为文档失败。</p>
      </div>
      <div class="header-actions">
        <t-button variant="outline" :loading="reconciling" @click="reconcile">重新识别路由</t-button>
        <t-button variant="text" :loading="loading" @click="load">刷新</t-button>
      </div>
    </header>

    <t-alert v-if="error" theme="error" :message="error" close @close="error = ''" />

    <div v-if="report" class="summary-grid">
      <div class="summary-card">
        <span>配置健康度</span>
        <strong :class="report.healthy ? 'ok' : 'bad'">{{ report.healthy ? '正常' : '存在冲突' }}</strong>
      </div>
      <div class="summary-card"><span>实际资源池</span><strong>{{ report.summary.pools }}</strong></div>
      <div class="summary-card"><span>模型绑定</span><strong>{{ report.summary.bindings }}</strong></div>
      <div class="summary-card"><span>等待 / 在途</span><strong>{{ admissionValue('Waiting') }} / {{ admissionValue('InFlight') }}</strong></div>
      <div class="summary-card"><span>消费槽位（后台 / Wiki）</span><strong>{{ report.runtime.background_consumer_slots }} / {{ report.runtime.wiki_consumer_slots }}</strong></div>
      <div class="summary-card"><span>错误 / 提醒</span><strong>{{ report.summary.errors }} / {{ report.summary.warnings }}</strong></div>
    </div>

    <div v-if="visibleGlobalIssues.length" class="issue-stack">
      <t-alert
        v-for="issue in visibleGlobalIssues"
        :key="`${issue.scope}:${issue.code}`"
        :theme="issue.severity === 'error' ? 'error' : 'warning'"
        :message="issue.message"
      />
    </div>

    <t-tabs v-model="activeTab" class="capacity-tabs">
      <t-tab-panel value="pools" label="容量策略">
        <div class="principle-note">
          <strong>一个并发真源：</strong>
          后台并发 = 总并发 − 交互预留；文档上限 ≤ 租户上限 ≤ 后台/总并发。Wiki、Graph、问题生成和聊天队列自动收敛到这些有效值。
        </div>
        <div class="list-toolbar">
          <t-input v-model="poolFilter" clearable placeholder="筛选模型名称、资源池 ID 或类型" />
          <span>显示 {{ filteredPools.length }} / {{ pools.length }}</span>
        </div>
        <div class="config-card-list" data-testid="capacity-pool-list">
          <article v-for="pool in filteredPools" :key="pool.id" class="config-card" :data-pool-id="pool.id">
            <header class="config-card__header">
              <div class="pool-name">
                  <strong>{{ pool.name }}</strong>
                  <small>{{ pool.resource_kind }} · {{ pool.id }} · v{{ pool.policy_version }}</small>
              </div>
              <div class="card-actions">
                <t-button size="small" :loading="savingID === pool.id" @click="savePool(pool)">保存</t-button>
                <t-dropdown :options="poolActions(pool)" trigger="click" @click="(item: any) => runPoolAction(pool, item.value)">
                  <t-button size="small" variant="text">更多</t-button>
                </t-dropdown>
              </div>
            </header>
            <div class="config-grid">
              <div class="config-field"><span class="field-label">状态</span><t-select v-model="pool.state" size="small" :options="stateOptions" /></div>
              <div class="config-field"><span class="field-label">总并发</span><t-input-number v-model="pool.max_inflight" :min="1" :max="1024" size="small" /></div>
              <div class="config-field"><span class="field-label">交互预留</span><t-input-number v-model="pool.interactive_reserve" :min="0" :max="Math.max(0, pool.max_inflight - 1)" size="small" /></div>
              <div class="config-field"><span class="field-label">后台有效<small>自动计算</small></span><div class="read-only-value">{{ backgroundFor(pool) }}</div></div>
              <div class="config-field"><span class="field-label">租户上限</span><t-input-number v-model="pool.tenant_burst" :min="1" :max="pool.max_inflight" size="small" /></div>
              <div class="config-field"><span class="field-label">文档上限</span><t-input-number v-model="pool.document_burst" :min="1" :max="Math.max(1, Math.min(pool.tenant_burst, backgroundFor(pool)))" size="small" /></div>
              <div class="config-field"><span class="field-label">RPM<small>0 = 不限</small></span><t-input-number v-model="pool.rpm" :min="0" :max="1000000" size="small" /></div>
              <div class="config-field"><span class="field-label">TPM<small>0 = 不限</small></span><t-input-number v-model="pool.tpm" :min="0" :max="2000000" :step="1000" size="small" /></div>
              <div class="config-field">
                <span class="field-label">等待队列<small>仅聊天模型</small></span>
                <t-input-number v-if="pool.resource_kind === 'chat'" v-model="pool.chat_max_waiting" :min="0" :max="100000" placeholder="继承" size="small" />
                <div v-else class="read-only-value read-only-value--muted">不适用</div>
              </div>
            </div>
          </article>
          <t-empty v-if="!filteredPools.length && !loading" description="没有匹配的资源池" />
        </div>
      </t-tab-panel>

      <t-tab-panel value="effective" label="有效值与冲突">
        <div v-if="report" class="effective-list">
          <article v-for="pool in report.pools" :key="pool.id" class="effective-card">
            <div class="effective-card__header">
              <div><strong>{{ pool.name }}</strong><small>{{ pool.id }} · {{ pool.binding_count }} 个绑定 / {{ pool.route_count }} 条实际路由</small></div>
              <t-tag :theme="poolHasErrors(pool) ? 'danger' : 'success'" variant="light">
                {{ poolHasErrors(pool) ? '冲突' : '有效' }}
              </t-tag>
            </div>
            <div class="effective-values">
              <span>模型调用 <b>{{ pool.effective.provider_total }}</b></span>
              <span>后台 <b>{{ pool.effective.background_max }}</b></span>
              <span>租户 <b>{{ pool.effective.tenant_max }}</b></span>
              <span>单文档 <b>{{ pool.effective.document_max }}</b></span>
              <span>聊天会话 <b>{{ pool.effective.chat_sessions }}</b></span>
            </div>
            <div v-if="poolGrants(pool).length" class="grant-list">
              <span v-for="grant in poolGrants(pool)" :key="grant.module">
                {{ moduleLabel(grant.module) }}：{{ grant.requested }} → <b>{{ grant.effective }}</b>
              </span>
            </div>
            <ul v-if="visiblePoolIssues(pool).length" class="pool-issues">
              <li v-for="issue in visiblePoolIssues(pool)" :key="issue.code" :class="issue.severity">{{ issue.message }}</li>
            </ul>
          </article>
        </div>
      </t-tab-panel>

      <t-tab-panel value="templates" label="默认模板">
        <p class="tab-help">模板只影响新识别或恢复默认的资源池；后台并发同样自动计算。</p>
        <div class="template-card-list">
          <article v-for="template in templates" :key="template.kind" class="config-card template-card">
            <header class="config-card__header">
              <div class="pool-name"><strong>{{ template.kind }}</strong><small>默认策略 · v{{ template.policy_version }}</small></div>
              <t-button size="small" :loading="savingID === template.kind" @click="saveTemplate(template)">保存</t-button>
            </header>
            <div class="config-grid">
              <div class="config-field"><span class="field-label">总并发</span><t-input-number v-model="template.max_inflight" :min="1" :max="1024" size="small" /></div>
              <div class="config-field"><span class="field-label">交互预留</span><t-input-number v-model="template.interactive_reserve" :min="0" :max="Math.max(0, template.max_inflight - 1)" size="small" /></div>
              <div class="config-field"><span class="field-label">后台有效<small>自动计算</small></span><div class="read-only-value">{{ Math.max(1, template.max_inflight - template.interactive_reserve) }}</div></div>
              <div class="config-field"><span class="field-label">租户上限</span><t-input-number v-model="template.tenant_burst" :min="1" :max="template.max_inflight" size="small" /></div>
              <div class="config-field"><span class="field-label">文档上限</span><t-input-number v-model="template.document_burst" :min="1" :max="Math.max(1, Math.min(template.tenant_burst, template.max_inflight - template.interactive_reserve))" size="small" /></div>
              <div class="config-field"><span class="field-label">RPM<small>0 = 不限</small></span><t-input-number v-model="template.rpm" :min="0" size="small" /></div>
              <div class="config-field"><span class="field-label">TPM<small>0 = 不限</small></span><t-input-number v-model="template.tpm" :min="0" :step="1000" size="small" /></div>
            </div>
          </article>
        </div>
      </t-tab-panel>

      <t-tab-panel value="advanced" label="高级绑定">
        <p class="tab-help">只有多个逻辑模型确实共用同一物理路由、账户配额或网关时才需要修改。空的配额池/网关池不会增加运行时限制。</p>
        <div class="list-toolbar"><t-input v-model="bindingFilter" clearable placeholder="按模型 ID 或资源池筛选" /><span>显示 {{ Math.min(filteredBindings.length, 200) }} / {{ bindings.length }}</span></div>
        <t-alert v-if="filteredBindings.length > 200" theme="info" message="为保证页面流畅，仅显示前 200 条；可使用上方筛选快速定位模型。" />
        <div class="binding-card-list">
          <article v-for="binding in filteredBindings.slice(0, 200)" :key="`${binding.model_tenant_id}:${binding.model_id}`" class="config-card binding-card">
            <header class="config-card__header">
              <div class="pool-name"><strong>{{ binding.model_id }}</strong><small>tenant {{ binding.model_tenant_id }} · v{{ binding.binding_version }}</small></div>
              <t-button size="small" :loading="savingID === bindingKey(binding)" @click="saveBinding(binding)">保存</t-button>
            </header>
            <div class="binding-grid">
              <div class="config-field"><span class="field-label">实际资源池</span><t-select v-model="binding.resource_pool_id" filterable :options="resourcePoolOptions" size="small" /></div>
              <div class="config-field"><span class="field-label">配额池</span><t-select v-model="binding.quota_pool_id" clearable :options="quotaPoolOptions" placeholder="无" size="small" /></div>
              <div class="config-field"><span class="field-label">网关池</span><t-select v-model="binding.gateway_pool_id" clearable :options="gatewayPoolOptions" placeholder="无" size="small" /></div>
            </div>
          </article>
          <t-empty v-if="!filteredBindings.length && !loading" description="没有匹配的模型绑定" />
        </div>
      </t-tab-panel>

      <t-tab-panel value="audits" label="审计">
        <div class="audit-list">
          <div v-for="audit in audits.slice(0, 100)" :key="audit.id" class="audit-row">
            <strong>{{ audit.action }}</strong><span>{{ audit.resource_type }} / {{ audit.resource_id }}</span><small>v{{ audit.policy_version }} · {{ formatTime(audit.created_at) }}</small>
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
  getCapacityEffectiveReport,
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
  validateCapacityPool,
  type CapacityEffectiveReport,
  type CapacityPoolReport,
  type ModelAdmissionAudit,
  type ModelAdmissionTemplate,
  type ModelGatewayPool,
  type ModelQuotaPool,
  type ModelResourceBinding,
  type ModelResourcePool,
} from '@/api/capacity-control'

const activeTab = ref('pools')
const loading = ref(false)
const reconciling = ref(false)
const savingID = ref('')
const error = ref('')
const poolFilter = ref('')
const bindingFilter = ref('')
const report = ref<CapacityEffectiveReport | null>(null)
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
const visibleGlobalIssues = computed(() => (report.value?.issues || []).filter(issue => issue.severity !== 'info'))
const filteredPools = computed(() => {
  const needle = poolFilter.value.trim().toLowerCase()
  if (!needle) return pools.value
  return pools.value.filter(row =>
    row.name.toLowerCase().includes(needle)
    || row.id.toLowerCase().includes(needle)
    || row.resource_kind.toLowerCase().includes(needle),
  )
})
const filteredBindings = computed(() => {
  const needle = bindingFilter.value.trim().toLowerCase()
  if (!needle) return bindings.value
  return bindings.value.filter(row => row.model_id.toLowerCase().includes(needle) || row.resource_pool_id.toLowerCase().includes(needle))
})

const backgroundFor = (pool: ModelResourcePool) => Math.max(1, pool.max_inflight - pool.interactive_reserve)
const admissionValue = (key: string) => report.value?.runtime?.admission?.[key] ?? report.value?.runtime?.admission?.[key.toLowerCase()] ?? 0
const moduleLabel = (value: string) => ({ question_batch: '问题批次', wiki_citation: 'Wiki 引用', graph_entity: '图谱实体', graph_relation: '图谱关系' }[value] || value)
const poolIssues = (pool: CapacityPoolReport) => pool.issues || []
const poolGrants = (pool: CapacityPoolReport) => pool.module_grants || []
const poolHasErrors = (pool: CapacityPoolReport) => poolIssues(pool).some(issue => issue.severity === 'error')
const visiblePoolIssues = (pool: CapacityPoolReport) => poolIssues(pool).filter(issue => issue.severity !== 'info')
const bindingKey = (binding: ModelResourceBinding) => `${binding.model_tenant_id}:${binding.model_id}`

function normalizeEffectiveReport(value: CapacityEffectiveReport): CapacityEffectiveReport {
  return {
    ...value,
    issues: value.issues || [],
    pools: (value.pools || []).map(pool => ({
      ...pool,
      issues: pool.issues || [],
      module_grants: pool.module_grants || [],
      quota_pool_ids: pool.quota_pool_ids || [],
      gateway_pool_ids: pool.gateway_pool_ids || [],
    })),
  }
}

async function load() {
  loading.value = true
  error.value = ''
  try {
    const [poolRows, bindingRows, templateRows, quotaRows, gatewayRows, auditRows, effective] = await Promise.all([
      listModelResourcePools(), listModelResourceBindings(), listModelAdmissionTemplates(),
      listModelQuotaPools(), listModelGatewayPools(), listModelAdmissionAudits(), getCapacityEffectiveReport(),
    ])
    pools.value = poolRows
    bindings.value = bindingRows
    templates.value = templateRows
    quotaPools.value = quotaRows
    gatewayPools.value = gatewayRows
    audits.value = auditRows
    report.value = normalizeEffectiveReport(effective)
  } catch (caught: any) {
    error.value = caught?.message || '加载容量控制配置失败'
  } finally {
    loading.value = false
  }
}

function canonicalInput(pool: ModelResourcePool): ModelResourcePool {
  return {
    ...pool,
    chat_max_concurrent: null,
    max_background_inflight: backgroundFor(pool),
    tenant_guaranteed: 1,
    document_guaranteed: 1,
    token_burst: 0,
  }
}

async function savePool(pool: ModelResourcePool) {
  savingID.value = pool.id
  try {
    const validation = await validateCapacityPool(canonicalInput(pool))
    if (!validation.valid || !validation.canonical) {
      throw new Error(validation.issues.map(issue => issue.message).join('；') || '容量组合无效')
    }
    Object.assign(pool, await updateModelResourcePool(validation.canonical))
    await load()
    MessagePlugin.success(`资源池“${pool.name}”已热更新并通过有效值校验`)
  } catch (caught: any) {
    MessagePlugin.error(caught?.message || '保存资源池失败')
  } finally {
    savingID.value = ''
  }
}

const poolActions = (pool: ModelResourcePool) => [
  { content: '进入排空', value: 'drain', disabled: pool.state === 'draining' },
  { content: '恢复默认', value: 'reset' },
]

function runPoolAction(pool: ModelResourcePool, action: string) {
  const dialog = DialogPlugin.confirm({
    header: action === 'drain' ? '排空资源池' : '恢复默认值',
    body: action === 'drain' ? `排空“${pool.name}”后不接收新调用，但不会中断在途调用。` : `确定按 ${pool.resource_kind} 默认模板恢复吗？`,
    confirmBtn: action === 'drain' ? '排空' : '恢复', cancelBtn: '取消',
    onConfirm: async () => {
      savingID.value = pool.id
      try {
        if (action === 'drain') await drainModelResourcePool(pool)
        else await resetModelResourcePool(pool)
        await load()
        MessagePlugin.success('操作已生效')
      } catch (caught: any) {
        MessagePlugin.error(caught?.message || '操作失败')
      } finally {
        savingID.value = ''
        dialog.destroy()
      }
    },
  })
}

async function saveBinding(binding: ModelResourceBinding) {
  savingID.value = bindingKey(binding)
  try {
    Object.assign(binding, await updateModelResourceBinding(binding))
    await load()
    MessagePlugin.success('模型资源绑定已更新并重新校验')
  } catch (caught: any) {
    MessagePlugin.error(caught?.message || '保存模型绑定失败')
  } finally { savingID.value = '' }
}

async function saveTemplate(template: ModelAdmissionTemplate) {
  savingID.value = template.kind
  try {
    Object.assign(template, await updateModelAdmissionTemplate(template))
    await load()
    MessagePlugin.success('默认模板已更新')
  } catch (caught: any) {
    MessagePlugin.error(caught?.message || '保存默认模板失败')
  } finally { savingID.value = '' }
}

async function reconcile() {
  reconciling.value = true
  try {
    await reconcileModelResourcePools()
    await load()
    MessagePlugin.success('实际模型路由已重新识别')
  } catch (caught: any) {
    MessagePlugin.error(caught?.message || '重新识别失败')
  } finally { reconciling.value = false }
}

const formatTime = (value: string) => value ? new Date(value).toLocaleString() : '-'
onMounted(load)
</script>

<style scoped lang="less">
.capacity-page { width: 100%; min-width: 0; overflow-x: hidden; }
.page-header, .header-actions, .list-toolbar, .effective-card__header, .config-card__header { display: flex; align-items: center; gap: 10px; }
.page-header { justify-content: space-between; margin-bottom: 16px; h2 { margin: 0 0 5px; font-size: 20px; } p { margin: 0; color: var(--td-text-color-secondary); font-size: 13px; } }
.header-actions { flex-wrap: wrap; justify-content: flex-end; }
.summary-grid { display: grid; grid-template-columns: repeat(3, minmax(140px, 1fr)); gap: 10px; margin: 14px 0; }
.summary-card { padding: 12px 14px; border: 1px solid var(--td-component-stroke); border-radius: 9px; background: var(--td-bg-color-container); span, strong { display: block; } span { color: var(--td-text-color-secondary); font-size: 12px; } strong { margin-top: 6px; font-size: 18px; } .ok { color: var(--td-success-color); } .bad { color: var(--td-error-color); } }
.issue-stack { display: grid; gap: 8px; margin-bottom: 12px; }
.capacity-tabs { margin-top: 8px; }
.principle-note, .tab-help { margin: 10px 0; padding: 10px 12px; border-radius: 7px; background: var(--td-bg-color-secondarycontainer); color: var(--td-text-color-secondary); font-size: 12px; }
.list-toolbar { justify-content: space-between; padding: 10px 0; }
.list-toolbar :deep(.t-input) { width: min(420px, 72%); }
.list-toolbar > span { flex: 0 0 auto; color: var(--td-text-color-secondary); font-size: 12px; }
.config-card-list, .binding-card-list { display: grid; gap: 12px; padding: 2px 0 12px; }
.template-card-list { display: grid; grid-template-columns: repeat(auto-fit, minmax(320px, 1fr)); gap: 12px; padding-bottom: 12px; }
.config-card { min-width: 0; padding: 14px; border: 1px solid var(--td-component-stroke); border-radius: 10px; background: var(--td-bg-color-container); }
.config-card__header { justify-content: space-between; margin-bottom: 13px; }
.pool-name { min-width: 0; strong, small { display: block; } strong { overflow-wrap: anywhere; } small { margin-top: 3px; color: var(--td-text-color-placeholder); font-size: 11px; overflow-wrap: anywhere; } }
.card-actions { display: flex; flex: 0 0 auto; align-items: center; gap: 4px; }
.config-grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(135px, 1fr)); gap: 12px; }
.template-card .config-grid { grid-template-columns: repeat(auto-fit, minmax(125px, 1fr)); }
.binding-grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(180px, 1fr)); gap: 12px; }
.config-field { min-width: 0; }
.field-label { display: block; min-height: 18px; margin-bottom: 6px; color: var(--td-text-color-secondary); font-size: 12px; line-height: 16px; }
.field-label small { display: inline; margin-left: 4px; color: var(--td-text-color-placeholder); font-size: 10px; }
.read-only-value { display: flex; align-items: center; min-height: 32px; padding: 0 10px; border-radius: 6px; background: var(--td-brand-color-light); color: var(--td-brand-color); font-size: 13px; font-weight: 600; }
.read-only-value--muted { background: var(--td-bg-color-secondarycontainer); color: var(--td-text-color-placeholder); font-weight: 400; }
:deep(.config-field .t-input-number), :deep(.config-field .t-select), :deep(.config-field .t-input) { width: 100%; min-width: 0; }
.binding-card-list { margin-top: 10px; }
.binding-card { padding: 12px 14px; }
.effective-list { display: grid; gap: 10px; padding: 12px 0; }
.effective-card { padding: 14px; border: 1px solid var(--td-component-stroke); border-radius: 9px; background: var(--td-bg-color-container); }
.effective-card__header { justify-content: space-between; strong, small { display: block; } small { margin-top: 3px; color: var(--td-text-color-placeholder); font-size: 11px; } }
.effective-values, .grant-list { display: flex; flex-wrap: wrap; gap: 10px 20px; margin-top: 12px; font-size: 12px; }
.grant-list { padding-top: 10px; border-top: 1px dashed var(--td-component-stroke); color: var(--td-text-color-secondary); }
.pool-issues { margin: 10px 0 0; padding-left: 18px; font-size: 12px; .error { color: var(--td-error-color); } .warning { color: var(--td-warning-color); } }
.audit-list { max-height: 480px; overflow: auto; }
.audit-row { display: grid; grid-template-columns: 90px minmax(0, 1fr) auto; gap: 12px; padding: 9px 4px; border-bottom: 1px solid var(--td-component-stroke); font-size: 12px; span { overflow-wrap: anywhere; } small { color: var(--td-text-color-placeholder); white-space: nowrap; } }
@media (max-width: 1100px) { .summary-grid { grid-template-columns: repeat(2, minmax(120px, 1fr)); } }
@media (max-width: 760px) {
  .page-header { align-items: flex-start; flex-direction: column; }
  .summary-grid, .template-card-list { grid-template-columns: 1fr; }
  .config-card__header { align-items: flex-start; }
  .audit-row { grid-template-columns: 1fr; gap: 3px; }
}
</style>
