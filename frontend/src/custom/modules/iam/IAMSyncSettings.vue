<template>
  <div class="iam-sync-settings">
    <div class="section-header">
      <h2>组织人员同步</h2>
      <p class="section-description">配置统一身份认证登录与组织人员同步，支持手动同步和定时同步。</p>
    </div>

    <div class="settings-group">
      <div class="setting-row">
        <div class="setting-info">
          <label>同步频率</label>
          <p class="desc">选择每日或每周指定日期执行同步。</p>
        </div>
        <div class="setting-control setting-control--stacked">
          <t-radio-group v-model="form.schedule_mode">
            <t-radio-button value="daily">每日</t-radio-button>
            <t-radio-button value="weekly">每周</t-radio-button>
          </t-radio-group>
          <div v-if="form.schedule_mode === 'weekly'" class="weekday-row">
            <label v-for="day in weekdayOptions" :key="day.value" class="weekday-item">
              <input v-model="weekdayValues" type="checkbox" :value="day.value" />
              <span>{{ day.label }}</span>
            </label>
          </div>
        </div>
      </div>

      <div class="setting-row">
        <div class="setting-info">
          <label>执行时间</label>
          <p class="desc">24 小时制，格式为 HH:mm。</p>
        </div>
        <div class="setting-control">
          <t-input v-model="form.run_at" class="time-input" placeholder="03:10" />
        </div>
      </div>

      <div class="setting-row setting-row--block">
        <div class="setting-info setting-info--full">
          <label>操作</label>
          <p class="desc">{{ statusText }}</p>
        </div>
        <div class="action-row">
          <t-button theme="primary" :loading="saving" @click="save">
            <template #icon><t-icon name="save" /></template>
            保存配置
          </t-button>
          <t-button variant="outline" :loading="syncButtonLoading" :disabled="syncButtonDisabled" @click="syncNow">
            <template #icon><t-icon name="refresh" /></template>
            {{ syncButtonLoading ? '同步中' : '立即同步' }}
          </t-button>
          <t-button variant="text" :loading="loadingRuns" @click="loadRuns">
            <template #icon><t-icon name="history" /></template>
            刷新记录
          </t-button>
        </div>
        <div class="sync-scope">
          <label>手动同步范围</label>
          <t-radio-group v-model="syncScopeMode" variant="default-filled">
            <t-radio-button value="all">全量同步</t-radio-button>
            <t-radio-button value="organization">按组织同步</t-radio-button>
          </t-radio-group>
          <p class="desc">按组织同步时，只同步所选组织及其所有子组织下的组织和人员。</p>
          <t-loading v-if="syncScopeMode === 'organization'" :loading="syncOrgTreeLoading">
            <div class="iam-sync-org-tree">
              <t-tree :data="syncOrgTreeData" hover lazy :load="loadSyncOrganizationChildren"
                :label="renderSyncOrgTreeLabel" empty="暂无 IAM 组织数据" />
            </div>
          </t-loading>
        </div>
      </div>

      <div class="setting-row setting-row--block">
        <div class="setting-info setting-info--full">
          <label>最近同步记录</label>
        </div>
        <div v-if="runs.length === 0" class="empty-state">暂无同步记录</div>
        <div v-else class="run-list">
          <div v-for="run in runs" :key="run.id" class="run-item">
            <div class="run-main">
              <t-tag :theme="run.status === 'success' ? 'success' : run.status === 'failed' ? 'danger' : 'primary'" variant="light">
                {{ run.status }}
              </t-tag>
              <span>{{ formatTime(run.started_at) }}</span>
              <span>{{ run.triggered_by }}</span>
            </div>
            <div class="run-meta">
              范围 {{ runScopeLabel(run) }}，{{ runCountPrefix(run) }}组织 {{ runStats(run).org_count }}，{{ runCountPrefix(run) }}人员 {{ runStats(run).user_count }}，新建 {{ runStats(run).created_users }}，更新 {{ runStats(run).updated_users }}，禁用 {{ runStats(run).disabled_users }}
            </div>
            <div v-if="run.status === 'running' && run.progress?.last_activity_at" class="run-progress-note">
              最近写入 {{ formatTime(run.progress.last_activity_at) }}
            </div>
            <div v-if="run.message && run.message !== 'ok'" class="run-message">{{ run.message }}</div>
          </div>
        </div>
      </div>

      <div class="setting-row">
        <div class="setting-info">
          <label>启用定时同步</label>
          <p class="desc">关闭后仍可手动同步；默认计划为每日 03:10。</p>
        </div>
        <div class="setting-control">
          <t-switch v-model="form.enabled" />
        </div>
      </div>

      <div class="setting-row">
        <div class="setting-info">
          <label>统一身份认证地址</label>
          <p class="desc">例如 https://iam.example.com.cn，不需要填写结尾斜杠。</p>
        </div>
        <div class="setting-control">
          <t-input v-model="form.base_url" class="setting-input" placeholder="https://iam.example.com.cn" />
        </div>
      </div>

      <div class="setting-row">
        <div class="setting-info">
          <label>登录认证凭证</label>
          <p class="desc">用于用户点击“统一身份认证登录”时进行 OAuth 授权码登录。</p>
        </div>
        <div class="setting-control setting-control--stacked">
          <t-input v-model="form.login_client_id" class="setting-input" placeholder="client_id" />
          <t-input v-model="form.login_client_secret" type="password" class="setting-input" placeholder="client_secret，留空表示不修改" />
        </div>
      </div>

      <div class="setting-row">
        <div class="setting-info">
          <label>组织人员同步凭证</label>
          <p class="desc">用于调用统一身份认证平台的组织和人员拉取接口。</p>
        </div>
        <div class="setting-control setting-control--stacked">
          <t-input v-model="form.sync_client_id" class="setting-input" placeholder="client_id" />
          <t-input v-model="form.sync_client_secret" type="password" class="setting-input" placeholder="client_secret，留空表示不修改" />
        </div>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, h, onMounted, onUnmounted, reactive, ref, resolveComponent, watch } from 'vue'
import { MessagePlugin } from 'tdesign-vue-next'
import {
  getIAMSyncSetting,
  listIAMOrganizations,
  listIAMSyncRuns,
  runIAMSync,
  saveIAMSyncSetting,
  type IAMOrganizationNode,
  type IAMSyncRun,
  type IAMSyncSetting,
} from '@/api/custom-admin'

type IAMOrgTreeNode = {
  value: string
  label: string
  children?: true | IAMOrgTreeNode[]
}
type IAMSyncRunCounters = Pick<IAMSyncRun, 'org_count' | 'user_count' | 'created_users' | 'updated_users' | 'disabled_users'>

const form = reactive<IAMSyncSetting>({
  enabled: false,
  base_url: '',
  login_client_id: '',
  login_client_secret: '',
  sync_client_id: '',
  sync_client_secret: '',
  schedule_mode: 'daily',
  weekdays: '',
  run_at: '03:10',
})

const weekdayOptions = [
  { label: '周日', value: '0' },
  { label: '周一', value: '1' },
  { label: '周二', value: '2' },
  { label: '周三', value: '3' },
  { label: '周四', value: '4' },
  { label: '周五', value: '5' },
  { label: '周六', value: '6' },
]

const saving = ref(false)
const syncing = ref(false)
const loadingRuns = ref(false)
const runs = ref<IAMSyncRun[]>([])
const syncScopeMode = ref<'all' | 'organization'>('all')
const syncScopeOrgIds = ref<string[]>([])
const syncScopePrimaryOrgId = ref('')
const syncOrgTreeLoading = ref(false)
const syncOrgTreeLoaded = ref(false)
const syncOrgTreeData = ref<IAMOrgTreeNode[]>([])
let pollingTimer: ReturnType<typeof setInterval> | null = null

const weekdayValues = computed<string[]>({
  get: () => (form.weekdays || '').split(',').map((item) => item.trim()).filter(Boolean),
  set: (value) => {
    form.weekdays = value.join(',')
  },
})

const hasRunningRun = computed(() => runs.value.some((run) => run.status === 'running'))
const syncButtonLoading = computed(() => syncing.value || hasRunningRun.value)
const selectedSyncRootOrgIds = computed(() => selectedLoadedRootOrgValues(syncScopeOrgIds.value, syncOrgTreeData.value))
const selectedSyncOrgId = computed(() => syncScopeMode.value === 'organization' ? (syncScopePrimaryOrgId.value || selectedSyncRootOrgIds.value[0] || '') : '')
const syncButtonDisabled = computed(() => syncScopeMode.value === 'organization' && !selectedSyncOrgId.value)

const statusText = computed(() => {
  if (hasRunningRun.value) return '同步任务正在后台运行，可以切换到其他页面继续使用系统。'
  if (!form.last_run_at) return '还没有执行过同步。'
  return `上次同步：${formatTime(form.last_run_at)}，状态：${form.last_status || '-'}，触发方式：${form.last_run_triggered_by || '-'}`
})

const assignSetting = (setting: IAMSyncSetting) => {
  Object.assign(form, {
    enabled: setting.enabled === true,
    base_url: setting.base_url || '',
    login_client_id: setting.login_client_id || '',
    login_client_secret: setting.login_client_secret || '',
    sync_client_id: setting.sync_client_id || '',
    sync_client_secret: setting.sync_client_secret || '',
    schedule_mode: setting.schedule_mode || 'daily',
    weekdays: setting.weekdays || '',
    run_at: setting.run_at || '03:10',
    last_run_at: setting.last_run_at,
    last_status: setting.last_status,
    last_message: setting.last_message,
    last_run_triggered_by: setting.last_run_triggered_by,
  })
}

const loadSetting = async () => {
  try {
    const res = await getIAMSyncSetting()
    assignSetting(res.data)
  } catch (error: any) {
    MessagePlugin.error(error?.message || '加载配置失败')
  }
}

const save = async () => {
  saving.value = true
  try {
    const res = await saveIAMSyncSetting({ ...form })
    assignSetting(res.data)
    MessagePlugin.success('配置已保存')
  } catch (error: any) {
    MessagePlugin.error(error?.message || '保存失败')
  } finally {
    saving.value = false
  }
}

function toSyncOrgTreeNode(org: IAMOrganizationNode): IAMOrgTreeNode {
  return {
    value: org.external_id,
    label: org.name || org.external_id,
    children: org.has_children ? true : undefined,
  }
}

async function loadSyncOrganizationRoots() {
  if (syncOrgTreeLoaded.value || syncOrgTreeLoading.value) return
  syncOrgTreeLoading.value = true
  try {
    const res = await listIAMOrganizations({ parentId: '', limit: 200 })
    if (res.success && Array.isArray(res.data)) {
      syncOrgTreeData.value = res.data.map(toSyncOrgTreeNode)
      syncOrgTreeLoaded.value = true
    } else {
      syncOrgTreeData.value = []
      syncOrgTreeLoaded.value = false
    }
  } catch (error: any) {
    syncOrgTreeData.value = []
    syncOrgTreeLoaded.value = false
    MessagePlugin.error(error?.message || '加载 IAM 组织树失败')
  } finally {
    syncOrgTreeLoading.value = false
  }
}

async function loadSyncOrganizationChildren(node: { value?: string }) {
  const data = treeNodeData(node)
  const parentId = String(data?.value || node?.value || '').trim()
  if (!parentId) return []
  const res = await listIAMOrganizations({ parentId, limit: 500 })
  const children = res.success && Array.isArray(res.data) ? res.data.map(toSyncOrgTreeNode) : []
  if (data) data.children = children
  const next = new Set(syncScopeOrgIds.value)
  if (data && next.has(data.value)) {
    collectLoadedTreeValues(children).forEach((value) => next.add(value))
  }
  const normalized = normalizeSyncScopeSelection(Array.from(next), syncScopeOrgIds.value)
  if (!sameStringArray(normalized, syncScopeOrgIds.value)) syncScopeOrgIds.value = normalized
  return children
}

function renderSyncOrgTreeLabel(_createElement: any, nodeModel?: any) {
  const data = treeNodeData(nodeModel)
  if (!data) return ''
  const Checkbox = resolveComponent('t-checkbox')
  return h('span', { class: 'iam-tree-node-label' }, [
    h(Checkbox as any, {
      checked: isSyncTreeNodeChecked(data.value),
      indeterminate: isSyncTreeNodeIndeterminate(data.value),
      onChange: (checked: boolean) => toggleSyncOrgSelection(data.value, checked),
      onClick: (event: Event) => event.stopPropagation(),
    }, {
      default: () => data.label || '',
    }),
  ])
}

function treeNodeData(input: any): IAMOrgTreeNode | undefined {
  return input?.node?.data || input?.data || input
}

function collectLoadedTreeValues(nodes?: true | IAMOrgTreeNode[]) {
  const values: string[] = []
  const visit = (items?: true | IAMOrgTreeNode[]) => {
    if (!Array.isArray(items)) return
    for (const item of items) {
      if (item.value) values.push(item.value)
      visit(item.children)
    }
  }
  visit(nodes)
  return values
}

function collectLoadedSubtreeValues(value: string) {
  const node = findLoadedTreeNode(value, syncOrgTreeData.value)
  if (!node || !Array.isArray(node.children)) return []
  return collectLoadedTreeValues(node.children)
}

function findLoadedTreeNode(value: string, nodes: IAMOrgTreeNode[]): IAMOrgTreeNode | undefined {
  for (const node of nodes) {
    if (node.value === value) return node
    if (Array.isArray(node.children)) {
      const child = findLoadedTreeNode(value, node.children)
      if (child) return child
    }
  }
  return undefined
}

function selectedLoadedRootOrgValues(values: string[], nodes: IAMOrgTreeNode[]) {
  const selected = new Set(values)
  const roots: string[] = []
  const seen = new Set<string>()
  const visit = (items: IAMOrgTreeNode[], selectedAncestor: boolean) => {
    for (const item of items) {
      const isSelected = selected.has(item.value)
      seen.add(item.value)
      if (isSelected && !selectedAncestor) roots.push(item.value)
      if (Array.isArray(item.children)) {
        visit(item.children, selectedAncestor || isSelected)
      }
    }
  }
  visit(nodes, false)
  for (const value of values) {
    if (!seen.has(value) && !roots.includes(value)) roots.push(value)
  }
  return roots
}

function isSyncTreeNodeChecked(value: string) {
  return syncScopeOrgIds.value.includes(value)
}

function isSyncTreeNodeIndeterminate(value: string) {
  if (isSyncTreeNodeChecked(value)) return false
  const descendants = collectLoadedSubtreeValues(value)
  if (descendants.length === 0) return false
  const selected = new Set(syncScopeOrgIds.value)
  return descendants.some((item) => selected.has(item))
}

function toggleSyncOrgSelection(value: string, checked: boolean) {
  const next = new Set(syncScopeOrgIds.value)
  const affectedValues = [value, ...collectLoadedSubtreeValues(value)]
  if (checked) {
    affectedValues.forEach((item) => next.add(item))
    syncScopeOrgIds.value = normalizeSyncScopeSelection(Array.from(next), syncScopeOrgIds.value)
    return
  }
  affectedValues.forEach((item) => next.delete(item))
  syncScopeOrgIds.value = normalizeSyncScopeSelection(Array.from(next), syncScopeOrgIds.value)
}

function normalizeSyncScopeSelection(ids: string[], _oldIds: string[]) {
  const next = new Set(ids.map((value) => value.trim()).filter(Boolean))
  syncLoadedParentSelection(syncOrgTreeData.value, next)
  const normalized = Array.from(next)
  const roots = selectedLoadedRootOrgValues(normalized, syncOrgTreeData.value)
  syncScopePrimaryOrgId.value = roots[0] || normalized[0] || ''
  return normalized
}

function syncLoadedParentSelection(nodes: IAMOrgTreeNode[], selected: Set<string>) {
  for (const node of nodes) {
    if (!Array.isArray(node.children)) continue
    syncLoadedParentSelection(node.children, selected)
    const childValues = node.children.map((child) => child.value).filter(Boolean)
    if (childValues.length === 0) continue
    if (childValues.every((value) => selected.has(value))) {
      selected.add(node.value)
    } else {
      selected.delete(node.value)
    }
  }
}

function sameStringArray(a: string[], b: string[]) {
  if (a.length !== b.length) return false
  const set = new Set(a)
  return b.every((item) => set.has(item))
}

const syncNow = async () => {
  syncing.value = true
  try {
    const scopeOrgId = selectedSyncOrgId.value
    const res = await runIAMSync(scopeOrgId ? { iam_organization_external_id: scopeOrgId } : undefined)
    if (res.success === false) {
      MessagePlugin.error(res.message || res.data?.message || '同步失败')
    } else {
      MessagePlugin.success('同步任务已提交')
      if (res.data) {
        runs.value = [res.data, ...runs.value.filter((run) => run.id !== res.data.id)]
      }
      startPolling()
    }
    await Promise.all([loadSetting(), loadRuns()])
  } catch (error: any) {
    MessagePlugin.error(error?.message || '同步失败')
  } finally {
    syncing.value = false
  }
}

const loadRuns = async (silent = false) => {
  if (!silent) loadingRuns.value = true
  try {
    const res = await listIAMSyncRuns()
    runs.value = res.data || []
    if (hasRunningRun.value) startPolling()
    else stopPolling()
  } catch (error: any) {
    if (!silent) MessagePlugin.error(error?.message || '加载同步记录失败')
  } finally {
    if (!silent) loadingRuns.value = false
  }
}

const startPolling = () => {
  if (pollingTimer) return
  pollingTimer = setInterval(() => {
    loadSetting()
    loadRuns(true)
  }, 3000)
}

const stopPolling = () => {
  if (!pollingTimer) return
  clearInterval(pollingTimer)
  pollingTimer = null
}

const formatTime = (value?: string) => {
  if (!value) return '-'
  return new Date(value).toLocaleString()
}

const runScopeLabel = (run: IAMSyncRun) => {
  if (run.scope_organization_external_id) {
    return run.scope_organization_name || run.scope_organization_external_id
  }
  return '全量'
}

const runStats = (run: IAMSyncRun): IAMSyncRunCounters => {
  if (run.status === 'running' && run.progress) {
    return run.progress
  }
  return run
}

const runCountPrefix = (run: IAMSyncRun) => (run.status === 'running' && run.progress ? '已处理' : '')

watch(syncScopeMode, (mode) => {
  if (mode === 'organization') loadSyncOrganizationRoots()
})

let normalizingSyncScopeSelection = false
watch(syncScopeOrgIds, (ids, oldIds) => {
  if (normalizingSyncScopeSelection) {
    normalizingSyncScopeSelection = false
    return
  }
  const normalized = normalizeSyncScopeSelection(ids, oldIds || [])
  if (!sameStringArray(normalized, ids)) {
    normalizingSyncScopeSelection = true
    syncScopeOrgIds.value = normalized
  }
})

onMounted(() => {
  loadSetting()
  loadRuns()
})

onUnmounted(() => {
  stopPolling()
})
</script>

<style lang="less" scoped>
.iam-sync-settings {
  width: 100%;
}

.section-header {
  margin-bottom: 32px;

  h2 {
    font-size: 20px;
    font-weight: 600;
    color: var(--td-text-color-primary);
    margin: 0 0 8px 0;
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
  padding: 20px 0;
  border-bottom: 1px solid var(--td-component-stroke);

  &:last-child {
    border-bottom: none;
  }
}

.setting-row--block {
  display: block;
}

.setting-info {
  flex: 1;
  max-width: 65%;
  padding-right: 24px;

  label {
    font-size: 15px;
    font-weight: 500;
    color: var(--td-text-color-primary);
    display: block;
    margin-bottom: 4px;
  }

  .desc {
    font-size: 13px;
    color: var(--td-text-color-secondary);
    margin: 0;
    line-height: 1.5;
  }
}

.setting-info--full {
  max-width: 100%;
  padding-right: 0;
  margin-bottom: 14px;
}

.setting-control {
  flex-shrink: 0;
  min-width: 300px;
  display: flex;
  justify-content: flex-end;
  align-items: center;
}

.setting-control--stacked {
  flex-direction: column;
  align-items: flex-end;
  gap: 8px;
}

.setting-input {
  width: 320px;
}

.time-input {
  width: 140px;
}

.weekday-row,
.action-row {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 10px;
}

.weekday-item {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  color: var(--td-text-color-secondary);
  font-size: 13px;
}

.action-row {
  justify-content: flex-end;
}

.sync-scope {
  margin-top: 16px;
  padding: 12px;
  border: 1px solid var(--td-component-stroke);
  border-radius: 8px;
  background: var(--td-bg-color-container);

  > label {
    display: block;
    margin-bottom: 10px;
    font-size: 14px;
    font-weight: 500;
    color: var(--td-text-color-primary);
  }

  .desc {
    margin: 8px 0 0;
    font-size: 12px;
    line-height: 1.5;
    color: var(--td-text-color-secondary);
  }
}

.iam-sync-org-tree {
  margin-top: 10px;
  max-height: 260px;
  overflow-y: auto;
  border: 1px solid var(--td-component-stroke);
  border-radius: 6px;
  padding: 6px 8px;
  background: var(--td-bg-color-container);
}

.iam-tree-node-label {
  min-width: 0;
  display: inline-flex;
  align-items: center;

  :deep(.t-checkbox) {
    max-width: 100%;
  }

  :deep(.t-checkbox__label) {
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
}

.empty-state {
  min-height: 80px;
  border: 1px dashed var(--td-component-stroke);
  border-radius: 8px;
  color: var(--td-text-color-placeholder);
  display: flex;
  align-items: center;
  justify-content: center;
}

.run-list {
  display: flex;
  flex-direction: column;
  gap: 8px;
}

.run-item {
  border: 1px solid var(--td-component-stroke);
  border-radius: 8px;
  padding: 12px;
  background: var(--td-bg-color-container);
}

.run-main {
  display: flex;
  align-items: center;
  gap: 10px;
  color: var(--td-text-color-primary);
  font-size: 13px;
}

.run-meta,
.run-progress-note,
.run-message {
  margin-top: 6px;
  color: var(--td-text-color-secondary);
  font-size: 12px;
  line-height: 1.5;
}

.run-progress-note {
  color: var(--td-text-color-placeholder);
}

.run-message {
  color: var(--td-error-color);
}
</style>
