<template>
  <div class="admin-governance">
    <div class="section-header">
      <h2>空间与用户权限</h2>
      <p class="section-description">系统管理员按需搜索空间，或按组织树和关键词管理用户可用性。</p>
    </div>

    <section class="admin-section">
      <div class="admin-section__title">
        <div>
          <h3>空间管理</h3>
          <p>输入空间名、空间 ID 或描述后搜索；不会默认加载全部空间。</p>
        </div>
      </div>
      <div class="search-row">
        <t-input v-model="spaceQuery" clearable placeholder="搜索个人空间或共享空间" @enter="loadSpaces" />
        <t-button theme="primary" :loading="spaceLoading" @click="loadSpaces">
          <template #icon><t-icon name="search" /></template>
          搜索
        </t-button>
      </div>
      <div v-if="spaceSearched && spaces.length === 0" class="empty-state">没有匹配的空间</div>
      <div v-else class="space-results">
        <div v-for="space in spaces" :key="`${space.kind}-${space.id}`" class="result-row">
          <div class="result-main">
            <t-tag :theme="space.kind === 'tenant' ? 'primary' : 'success'" variant="light">
              {{ space.kind === 'tenant' ? '个人空间' : '共享空间' }}
            </t-tag>
            <div class="result-text">
              <div class="result-title">{{ space.name || space.id }}</div>
              <div class="result-meta">
                <span>ID: {{ space.id }}</span>
                <span v-if="space.owner_username">所有者: {{ space.owner_username }}</span>
                <span>成员: {{ space.member_count || 0 }}</span>
              </div>
            </div>
          </div>
          <div class="result-actions">
            <t-button v-if="space.kind === 'tenant'" variant="outline" @click="enterTenant(space)">
              <template #icon><t-icon name="login" /></template>
              进入
            </t-button>
            <t-button v-else variant="outline" @click="openOrganization(space.id)">
              <template #icon><t-icon name="setting" /></template>
              管理
            </t-button>
          </div>
        </div>
      </div>
    </section>

    <section class="admin-section">
      <div class="admin-section__title">
        <div>
          <h3>用户可用性</h3>
          <p>可按组织树限定范围，也可直接搜索组织名、用户名或空间名。</p>
        </div>
      </div>
      <div class="user-tools">
        <div class="org-tree-panel">
          <div class="panel-title">
            <span>组织范围</span>
            <t-button size="small" variant="text" :loading="orgTreeLoading" @click="loadOrgRoots(true)">
              <template #icon><t-icon name="refresh" /></template>
              刷新
            </t-button>
          </div>
          <t-loading :loading="orgTreeLoading">
            <div class="org-tree">
              <t-tree
                :data="orgTreeData"
                hover
                lazy
                :load="loadOrgChildren"
                :label="renderTreeLabel"
                empty="暂无组织数据"
              />
            </div>
          </t-loading>
          <t-checkbox v-model="directOnly" class="direct-check">只看直接挂在所选组织下的人员</t-checkbox>
        </div>
        <div class="user-search-panel">
          <div class="search-row">
            <t-input v-model="userQuery" clearable placeholder="搜索用户名、姓名、组织名或空间名" @enter="loadUsers" />
            <t-button theme="primary" :loading="userLoading" @click="loadUsers">
              <template #icon><t-icon name="search" /></template>
              查询用户
            </t-button>
          </div>
          <div class="bulk-action-row">
            <span>批量操作当前组织范围或搜索结果内的用户。</span>
            <t-button
              size="small"
              variant="outline"
              :disabled="!hasUserFilterScope"
              :loading="bulkUpdating === 'enable'"
              @click="bulkUpdateUsers(true)"
            >
              批量启用
            </t-button>
            <t-button
              size="small"
              theme="danger"
              variant="outline"
              :disabled="!hasUserFilterScope"
              :loading="bulkUpdating === 'disable'"
              @click="bulkUpdateUsers(false)"
            >
              批量禁用
            </t-button>
          </div>
          <div v-if="userSearched && users.length === 0" class="empty-state">没有匹配的用户</div>
          <div v-else class="user-results">
            <div v-for="user in users" :key="user.id" class="result-row">
              <div class="result-main">
                <t-tag :theme="user.is_active ? 'success' : 'danger'" variant="light">
                  {{ user.is_active ? '启用' : '禁用' }}
                </t-tag>
                <div class="result-text">
                  <div class="result-title">
                    <span>{{ userDisplayName(user) }}</span>
                    <t-tooltip v-if="user.has_local_user === false" content="用户尚未登录过" placement="top">
                      <t-icon name="info-circle" class="mirror-user-icon" size="14px" />
                    </t-tooltip>
                    <t-tag v-if="user.is_system_admin" size="small" theme="warning" variant="light">系统管理员</t-tag>
                  </div>
                  <div class="result-meta">
                    <span v-if="user.username && user.username !== user.display_name">用户名: {{ user.username }}</span>
                    <span>{{ user.tenant_name || (user.tenant_id ? `tenant#${user.tenant_id}` : '尚未登录') }}</span>
                    <span v-if="user.iam_display_name">{{ user.iam_display_name }}</span>
                    <span v-if="user.iam_organization_name">{{ user.iam_organization_name }}</span>
                  </div>
                </div>
              </div>
              <div class="result-actions">
                <t-switch
                  :model-value="user.is_active"
                  :loading="updatingUsers[user.id]"
                  :disabled="user.id === authStore.currentUserId"
                  @change="(value: unknown) => updateUserActive(user, switchValueToBool(value))"
                />
              </div>
            </div>
          </div>
        </div>
      </div>
    </section>

    <OrganizationSettingsModal v-model:visible="orgModalVisible" :org-id="activeOrgId" mode="edit" />
  </div>
</template>

<script setup lang="ts">
import { computed, h, onMounted, reactive, ref, resolveComponent, watch } from 'vue'
import { useRouter } from 'vue-router'
import { MessagePlugin } from 'tdesign-vue-next'
import OrganizationSettingsModal from '@/views/organization/OrganizationSettingsModal.vue'
import {
  batchSetAdminUsersActive,
  listIAMOrganizations,
  searchAdminSpaces,
  searchAdminUsers,
  setAdminUserActive,
  type AdminBulkUserActiveResult,
  type AdminManagedUser,
  type AdminSpaceSummary,
  type IAMOrganizationNode,
} from '@/api/custom-admin'
import { useAuthStore } from '@/stores/auth'
import { useUIStore } from '@/stores/ui'

type OrgTreeNode = {
  value: string
  label: string
  nodeType: 'organization' | 'user'
  externalId?: string
  iamExternalId?: string
  username?: string
  displayName?: string
  hasLocalUser?: boolean
  children?: true | OrgTreeNode[]
}

const router = useRouter()
const authStore = useAuthStore()
const uiStore = useUIStore()

const spaceQuery = ref('')
const spaceLoading = ref(false)
const spaceSearched = ref(false)
const spaces = ref<AdminSpaceSummary[]>([])

const userQuery = ref('')
const userLoading = ref(false)
const userSearched = ref(false)
const users = ref<AdminManagedUser[]>([])
const bulkUpdating = ref<'' | 'enable' | 'disable'>('')
const selectedOrgIds = ref<string[]>([])
const directOnly = ref(false)
const orgTreeLoading = ref(false)
const orgTreeLoaded = ref(false)
const orgTreeData = ref<OrgTreeNode[]>([])
const updatingUsers = reactive<Record<string, boolean>>({})

const orgModalVisible = ref(false)
const activeOrgId = ref('')

const selectedOrgExternalIds = computed(() =>
  selectedOrgIds.value
    .filter((value) => value.startsWith('org:'))
    .map((value) => value.slice(4))
    .filter(Boolean)
)

const selectedIAMExternalIds = computed(() =>
  selectedOrgIds.value
    .filter((value) => value.startsWith('iam:'))
    .map((value) => value.slice(4))
    .filter(Boolean)
)

const hasUserFilterScope = computed(() =>
  userQuery.value.trim() !== '' || selectedOrgExternalIds.value.length > 0 || selectedIAMExternalIds.value.length > 0
)

function toOrgTreeNode(org: IAMOrganizationNode): OrgTreeNode {
  if (org.node_type === 'user') {
    const iamExternalId = org.iam_external_id || org.external_id
    const label = displayNameWithUsername(org.display_name || org.name, org.username || iamExternalId, iamExternalId)
    return {
      value: `iam:${iamExternalId}`,
      label,
      nodeType: 'user',
      externalId: org.external_id,
      iamExternalId,
      username: org.username,
      displayName: org.display_name || org.name,
      hasLocalUser: org.has_local_user === true,
    }
  }
  return {
    value: `org:${org.external_id}`,
    label: `${org.name || org.external_id} (${org.user_count || 0})`,
    nodeType: 'organization',
    externalId: org.external_id,
    children: org.has_children ? true : undefined,
  }
}

async function loadOrgRoots(force = false) {
  if (!force && (orgTreeLoaded.value || orgTreeLoading.value)) return
  orgTreeLoading.value = true
  try {
    const res = await listIAMOrganizations({ parentId: '', limit: 200, includeUsers: true })
    if (res.success && Array.isArray(res.data)) {
      orgTreeData.value = res.data.map(toOrgTreeNode)
      orgTreeLoaded.value = true
    } else {
      orgTreeData.value = []
      orgTreeLoaded.value = false
    }
  } catch (error: any) {
    orgTreeData.value = []
    orgTreeLoaded.value = false
    MessagePlugin.error(error?.message || '加载组织树失败')
  } finally {
    orgTreeLoading.value = false
  }
}

async function loadOrgChildren(input: any) {
  const data = treeNodeData(input)
  const parentId = nodeOrgExternalId(data?.value || input?.value)
  if (!parentId) return []
  const res = await listIAMOrganizations({ parentId, limit: 500, includeUsers: true })
  const children = res.success && Array.isArray(res.data) ? res.data.map(toOrgTreeNode) : []
  if (data) data.children = children
  const next = new Set(selectedOrgIds.value)
  if (data && next.has(data.value)) {
    collectLoadedTreeValues(children).forEach((value) => next.add(value))
  }
  const normalized = normalizeLoadedCascadeSelection(Array.from(next), selectedOrgIds.value)
  if (!sameStringArray(normalized, selectedOrgIds.value)) selectedOrgIds.value = normalized
  return children
}

function nodeOrgExternalId(value?: string) {
  const raw = String(value || '').trim()
  if (!raw.startsWith('org:')) return ''
  return raw.slice(4)
}

function renderTreeLabel(_createElement: any, nodeModel?: any) {
  const data = treeNodeData(nodeModel)
  if (!data) return ''
  const Checkbox = resolveComponent('t-checkbox')
  const Tooltip = resolveComponent('t-tooltip')
  const Icon = resolveComponent('t-icon')
  const children = [
    h('span', { class: 'iam-tree-node-text' }, data.label),
  ]
  if (data.nodeType === 'user' && !data.hasLocalUser) {
    children.push(h(Tooltip as any, { content: '用户尚未登录过', placement: 'top' }, {
      default: () => h(Icon as any, { name: 'info-circle', size: '14px', class: 'mirror-user-icon' }),
    }) as any)
  }
  return h('span', { class: 'iam-tree-node-label' }, [
    h(Checkbox as any, {
      checked: isTreeNodeChecked(data),
      indeterminate: isTreeNodeIndeterminate(data),
      onChange: (checked: boolean) => toggleTreeNodeSelection(data, checked),
      onClick: (event: Event) => event.stopPropagation(),
    }, {
      default: () => children,
    }),
  ])
}

function treeNodeData(input: any): OrgTreeNode | undefined {
  return input?.node?.data || input?.data || input
}

function collectLoadedTreeValues(nodes?: true | OrgTreeNode[]) {
  const values: string[] = []
  const visit = (items?: true | OrgTreeNode[]) => {
    if (!Array.isArray(items)) return
    for (const item of items) {
      values.push(item.value)
      visit(item.children)
    }
  }
  visit(nodes)
  return values
}

function collectLoadedSubtreeValues(value: string, nodes: OrgTreeNode[]) {
  const node = findLoadedTreeNode(value, nodes)
  if (!node || !Array.isArray(node.children)) return []
  return collectLoadedTreeValues(node.children)
}

function findLoadedTreeNode(value: string, nodes: OrgTreeNode[]): OrgTreeNode | undefined {
  for (const node of nodes) {
    if (node.value === value) return node
    if (Array.isArray(node.children)) {
      const child = findLoadedTreeNode(value, node.children)
      if (child) return child
    }
  }
  return undefined
}

function isTreeNodeChecked(data: OrgTreeNode) {
  return selectedOrgIds.value.includes(data.value)
}

function isTreeNodeIndeterminate(data: OrgTreeNode) {
  if (data.nodeType !== 'organization' || isTreeNodeChecked(data)) return false
  const descendants = collectLoadedSubtreeValues(data.value, orgTreeData.value)
  if (descendants.length === 0) return false
  const selected = new Set(selectedOrgIds.value)
  return descendants.some((value) => selected.has(value))
}

function toggleTreeNodeSelection(data: OrgTreeNode, checked: boolean) {
  const next = new Set(selectedOrgIds.value)
  const affectedValues = [
    data.value,
    ...(data.nodeType === 'organization' ? collectLoadedSubtreeValues(data.value, orgTreeData.value) : []),
  ]
  if (checked) {
    affectedValues.forEach((value) => next.add(value))
    selectedOrgIds.value = normalizeLoadedCascadeSelection(Array.from(next), selectedOrgIds.value)
    return
  }
  affectedValues.forEach((value) => next.delete(value))
  selectedOrgIds.value = normalizeLoadedCascadeSelection(Array.from(next), selectedOrgIds.value)
}

function normalizeLoadedCascadeSelection(ids: string[], _oldIds: string[]) {
  const next = new Set(ids)
  syncLoadedParentSelection(orgTreeData.value, next)
  return Array.from(next)
}

function syncLoadedParentSelection(nodes: OrgTreeNode[], selected: Set<string>) {
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

async function loadSpaces() {
  const query = spaceQuery.value.trim()
  spaceSearched.value = true
  if (!query) {
    spaces.value = []
    MessagePlugin.warning('请输入空间搜索关键词')
    return
  }
  spaceLoading.value = true
  try {
    const res = await searchAdminSpaces({ query, limit: 30 })
    spaces.value = res.success ? (res.data || []) : []
    if (res.success === false) MessagePlugin.error(res.message || '搜索失败')
  } catch (error: any) {
    spaces.value = []
    MessagePlugin.error(error?.message || '搜索失败')
  } finally {
    spaceLoading.value = false
  }
}

function enterTenant(space: AdminSpaceSummary) {
  const tenantId = Number(space.tenant_id || space.id)
  if (!tenantId) {
    MessagePlugin.error('空间缺少租户 ID')
    return
  }
  authStore.setSelectedTenant(tenantId, space.name || `tenant#${tenantId}`)
  uiStore.closeSettings()
  router.push('/platform/knowledge-bases')
}

function openOrganization(orgId: string) {
  activeOrgId.value = orgId
  orgModalVisible.value = true
}

async function loadUsers() {
  const query = userQuery.value.trim()
  userSearched.value = true
  if (!query && selectedOrgExternalIds.value.length === 0 && selectedIAMExternalIds.value.length === 0) {
    users.value = []
    MessagePlugin.warning('请输入用户搜索关键词，或先选择组织范围')
    return
  }
  userLoading.value = true
  try {
    const res = await searchAdminUsers({
      query,
      iamOrgIds: selectedOrgExternalIds.value,
      iamExternalIds: selectedIAMExternalIds.value,
      direct: directOnly.value,
      limit: selectedOrgExternalIds.value.length > 0 ? 1000 : 100,
    })
    users.value = res.success ? (res.data || []) : []
    if (res.success === false) MessagePlugin.error(res.message || '查询失败')
  } catch (error: any) {
    users.value = []
    MessagePlugin.error(error?.message || '查询失败')
  } finally {
    userLoading.value = false
  }
}

function displayNameWithUsername(displayName?: string, username?: string, fallback?: string) {
  const name = (displayName || '').trim()
  const account = (username || '').trim()
  if (name && account && name !== account) return `${name}（${account}）`
  return name || account || fallback || '-'
}

function userDisplayName(user: AdminManagedUser) {
  return displayNameWithUsername(user.display_name || user.iam_display_name, user.username || user.iam_username, user.id)
}

function switchValueToBool(value: unknown): boolean {
  if (typeof value === 'object' && value && 'value' in value) {
    return switchValueToBool((value as { value: unknown }).value)
  }
  return value === true || value === 1 || value === 'true' || value === '1'
}

async function updateUserActive(user: AdminManagedUser, active: boolean) {
  if (user.id === authStore.currentUserId && !active) {
    MessagePlugin.warning('不能禁用当前登录账号')
    return
  }
  const previousActive = user.is_active
  const previousAccessEnabled = user.access_enabled
  updatingUsers[user.id] = true
  try {
    const res = await setAdminUserActive(user.id, active)
    if (res.success && res.data) {
      Object.assign(user, res.data)
      user.is_active = active
      user.access_enabled = active
      MessagePlugin.success(active ? '已启用用户' : '已禁用用户')
    } else {
      user.is_active = previousActive
      user.access_enabled = previousAccessEnabled
      MessagePlugin.error(res.message || '更新失败')
    }
  } catch (error: any) {
    user.is_active = previousActive
    user.access_enabled = previousAccessEnabled
    MessagePlugin.error(error?.message || '更新失败')
  } finally {
    updatingUsers[user.id] = false
  }
}

function bulkResultText(result?: AdminBulkUserActiveResult) {
  if (!result) return ''
  const updated = Number(result.updated_local_users || 0) + Number(result.updated_iam_users || 0)
  const skipped = Number(result.skipped_self || 0) + Number(result.skipped_system_admins || 0)
  const parts = [`更新 ${updated} 项`]
  if (skipped > 0) parts.push(`跳过 ${skipped} 个受保护账号`)
  return parts.join('，')
}

async function bulkUpdateUsers(active: boolean) {
  if (!hasUserFilterScope.value) {
    MessagePlugin.warning('请输入用户搜索关键词，或先选择组织范围')
    return
  }
  const action = active ? '启用' : '禁用'
  if (!window.confirm(`确认批量${action}当前筛选范围内的用户？`)) return
  bulkUpdating.value = active ? 'enable' : 'disable'
  try {
    const res = await batchSetAdminUsersActive({
      active,
      query: userQuery.value.trim(),
      iamOrgIds: selectedOrgExternalIds.value,
      iamExternalIds: selectedIAMExternalIds.value,
      direct: directOnly.value,
    })
    if (res.success) {
      MessagePlugin.success(`已批量${action}${bulkResultText(res.data) ? `：${bulkResultText(res.data)}` : ''}`)
      userSearched.value = true
      await loadUsers()
    } else {
      MessagePlugin.error(res.message || `批量${action}失败`)
    }
  } catch (error: any) {
    MessagePlugin.error(error?.message || `批量${action}失败`)
  } finally {
    bulkUpdating.value = ''
  }
}

let normalizingOrgSelection = false
watch(selectedOrgIds, (ids, oldIds) => {
  if (normalizingOrgSelection) {
    normalizingOrgSelection = false
    if (userSearched.value) loadUsers()
    return
  }
  const normalized = normalizeLoadedCascadeSelection(ids, oldIds || [])
  if (!sameStringArray(normalized, ids)) {
    normalizingOrgSelection = true
    selectedOrgIds.value = normalized
    return
  }
  if (userSearched.value) loadUsers()
})

onMounted(() => {
  loadOrgRoots()
})
</script>

<style lang="less" scoped>
.admin-governance {
  width: 100%;
}

.section-header {
  margin-bottom: 28px;

  h2 {
    font-size: 20px;
    font-weight: 600;
    color: var(--td-text-color-primary);
    margin: 0 0 8px;
  }

  .section-description {
    font-size: 14px;
    color: var(--td-text-color-secondary);
    margin: 0;
  }
}

.admin-section {
  padding: 22px 0;
  border-top: 1px solid var(--td-component-stroke);
}

.admin-section__title {
  display: flex;
  justify-content: space-between;
  gap: 16px;
  margin-bottom: 14px;

  h3 {
    margin: 0 0 4px;
    font-size: 16px;
    font-weight: 600;
    color: var(--td-text-color-primary);
  }

  p {
    margin: 0;
    font-size: 13px;
    color: var(--td-text-color-secondary);
  }
}

.search-row {
  display: grid;
  grid-template-columns: minmax(240px, 1fr) auto;
  gap: 10px;
  align-items: center;
}

.space-results,
.user-results {
  display: flex;
  flex-direction: column;
  gap: 8px;
  margin-top: 12px;
}

.bulk-action-row {
  display: flex;
  align-items: center;
  justify-content: flex-end;
  flex-wrap: wrap;
  gap: 8px;
  margin-top: 10px;
  color: var(--td-text-color-secondary);
  font-size: 12px;

  > span {
    margin-right: auto;
  }
}

.result-row {
  display: flex;
  justify-content: space-between;
  gap: 12px;
  align-items: center;
  border: 1px solid var(--td-component-stroke);
  border-radius: 8px;
  padding: 12px;
  background: var(--td-bg-color-container);
}

.result-main {
  min-width: 0;
  display: flex;
  align-items: center;
  gap: 12px;
}

.result-text {
  min-width: 0;
}

.result-title {
  display: flex;
  align-items: center;
  gap: 8px;
  min-width: 0;
  font-size: 14px;
  font-weight: 500;
  color: var(--td-text-color-primary);
  overflow-wrap: anywhere;
}

.mirror-user-icon {
  color: var(--td-warning-color);
  flex: 0 0 auto;
}

.iam-tree-node-label {
  min-width: 0;
  display: inline-flex;
  align-items: center;
  gap: 4px;

  :deep(.t-checkbox) {
    max-width: 100%;
  }

  :deep(.t-checkbox__label) {
    min-width: 0;
    display: inline-flex;
    align-items: center;
    gap: 4px;
  }
}

.iam-tree-node-text {
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
}

.result-meta {
  display: flex;
  flex-wrap: wrap;
  gap: 10px;
  margin-top: 4px;
  font-size: 12px;
  color: var(--td-text-color-secondary);
}

.result-actions {
  flex: 0 0 auto;
}

.user-tools {
  display: grid;
  grid-template-columns: minmax(260px, 320px) minmax(0, 1fr);
  gap: 16px;
  align-items: start;
}

.org-tree-panel,
.user-search-panel {
  min-width: 0;
}

.panel-title {
  display: flex;
  justify-content: space-between;
  align-items: center;
  margin-bottom: 8px;
  font-size: 14px;
  font-weight: 500;
  color: var(--td-text-color-primary);
}

.org-tree {
  height: 360px;
  overflow: auto;
  border: 1px solid var(--td-component-stroke);
  border-radius: 8px;
  padding: 8px;
  background: var(--td-bg-color-container);
}

.direct-check {
  margin-top: 10px;
}

.empty-state {
  margin-top: 12px;
  min-height: 72px;
  border: 1px dashed var(--td-component-stroke);
  border-radius: 8px;
  display: flex;
  align-items: center;
  justify-content: center;
  color: var(--td-text-color-placeholder);
}

@media (max-width: 900px) {
  .user-tools {
    grid-template-columns: 1fr;
  }
}
</style>
