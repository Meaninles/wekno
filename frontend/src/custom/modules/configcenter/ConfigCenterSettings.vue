<template>
  <div class="custom-config-settings">
    <div class="section-header">
      <h2>默认配置</h2>
      <p class="section-description">维护所有用户默认可用的模型与运行组件，并为单个用户追加额外权限。</p>
    </div>

    <div class="settings-group">
      <div class="setting-row">
        <div class="setting-info">
          <label>配置源用户</label>
          <p class="desc">选择用户后，只读取该用户自身登录工作区中的配置，不读取他创建或加入的共享工作区。</p>
        </div>
        <div class="setting-control setting-control--inline">
          <t-select
            v-model="sourceUserId"
            :options="userOptions"
            filterable
            class="wide-select"
            placeholder="选择配置源用户"
            @change="handleSourceUserChange"
          />
          <t-button :loading="loadingUsers || loadingResources" @click="refreshSource">
            <template #icon><t-icon name="refresh" /></template>
            刷新
          </t-button>
        </div>
      </div>

      <div class="setting-row setting-row--block">
        <div class="setting-info setting-info--full">
          <label>所有用户默认配置</label>
          <p class="desc">保存后，新用户登录和现有用户下次刷新用户信息时会自动获得这些配置；也可以立即下发给所有用户。</p>
        </div>
        <ResourceSelector v-model="defaultKeys" :groups="resourceGroups" :loading="loadingResources" />
        <div class="action-row">
          <t-button theme="primary" :loading="savingDefaults" @click="saveDefaults">
            <template #icon><t-icon name="save" /></template>
            保存默认配置
          </t-button>
          <t-button variant="outline" :loading="applyingAll" @click="applyAll">
            <template #icon><t-icon name="send" /></template>
            立即下发给所有用户
          </t-button>
        </div>
      </div>

      <div class="setting-row setting-row--block">
        <div class="setting-info setting-info--full">
          <label>用户额外权限</label>
          <p class="desc">选择某个用户后，为他额外开通默认配置之外的资源。</p>
        </div>
        <div class="toolbar">
          <t-select
            v-model="selectedUserId"
            :options="userOptions"
            filterable
            clearable
            class="wide-select"
            placeholder="选择用户"
            @change="loadSelectedUserGrants"
          />
          <t-button :loading="loadingUsers" @click="loadUsers">
            <template #icon><t-icon name="refresh" /></template>
            刷新用户
          </t-button>
        </div>
        <ResourceSelector v-model="userKeys" :groups="resourceGroups" :loading="loadingUserGrants || loadingResources" />
        <div class="action-row">
          <t-button theme="primary" :disabled="!selectedUserId" :loading="savingUser" @click="saveUser">
            <template #icon><t-icon name="save" /></template>
            保存用户权限
          </t-button>
          <t-button variant="outline" :disabled="!selectedUserId" :loading="applyingUser" @click="applyUser">
            <template #icon><t-icon name="send" /></template>
            立即下发给该用户
          </t-button>
        </div>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { MessagePlugin } from 'tdesign-vue-next'
import { useAuthStore } from '@/stores/auth'
import {
  applyDefaultConfig,
  applyUserConfig,
  getDefaultGrants,
  getUserGrants,
  listConfigResources,
  listCustomUsers,
  saveDefaultGrants,
  saveUserGrants,
  type CustomUserSummary,
  type ResourceRef,
  type ResourceSummary,
} from '@/api/custom-admin'
import ResourceSelector from './ResourceSelector.vue'

const RESOURCE_LABELS: Record<string, string> = {
  model: '模型',
  vector_store: '向量数据库',
  parser_engine: '解析引擎',
  storage_engine: '存储引擎',
  web_search: '网络搜索',
  mcp_service: 'MCP 服务',
}

type ResourceGroup = {
  type: string
  label: string
  items: ResourceSummary[]
}

const authStore = useAuthStore()
const sourceUserId = ref('')
const resources = ref<ResourceSummary[]>([])
const users = ref<CustomUserSummary[]>([])
const defaultRefs = ref<ResourceRef[]>([])
const selectedUserRefs = ref<ResourceRef[]>([])
const defaultKeys = ref<string[]>([])
const userKeys = ref<string[]>([])
const selectedUserId = ref('')
const loadingResources = ref(false)
const loadingUsers = ref(false)
const loadingUserGrants = ref(false)
const savingDefaults = ref(false)
const savingUser = ref(false)
const applyingAll = ref(false)
const applyingUser = ref(false)

const resourceGroups = computed<ResourceGroup[]>(() => {
  return Object.entries(RESOURCE_LABELS)
    .map(([type, label]) => ({
      type,
      label,
      items: resources.value.filter((item) => item.resource_type === type),
    }))
    .filter((group) => group.items.length > 0)
})

const userOptions = computed(() => users.value.map((user) => ({
  label: user.username || user.id,
  value: user.id,
})))

const resourceKey = (item: ResourceSummary) => item.config_key || refKey({
  resource_type: item.resource_type,
  source_tenant_id: item.source_tenant_id,
  source_resource_id: item.id,
})

const refKey = (ref: ResourceRef) => `${ref.resource_type}|${ref.source_tenant_id}|${ref.source_resource_id}`

const resourcesByRefKey = computed(() => {
  const map = new Map<string, ResourceSummary>()
  for (const item of resources.value) {
    map.set(refKey({
      resource_type: item.resource_type,
      source_tenant_id: item.source_tenant_id,
      source_resource_id: item.id,
    }), item)
  }
  return map
})

const resourcesBySelectionKey = computed(() => {
  const map = new Map<string, ResourceSummary>()
  for (const item of resources.value) {
    map.set(resourceKey(item), item)
  }
  return map
})

const refsToKeys = (refs: ResourceRef[]) => {
  return refs
    .map((ref) => resourcesByRefKey.value.get(refKey(ref)))
    .filter((item): item is ResourceSummary => Boolean(item))
    .map(resourceKey)
}

const keysToRefs = (keys: string[]): ResourceRef[] => {
  const seen = new Set<string>()
  const refs: ResourceRef[] = []
  for (const key of keys) {
    const item = resourcesBySelectionKey.value.get(key)
    if (!item) continue
    const ref: ResourceRef = {
      resource_type: item.resource_type,
      source_tenant_id: item.source_tenant_id,
      source_resource_id: item.id,
    }
    const normalized = refKey(ref)
    if (seen.has(normalized)) continue
    seen.add(normalized)
    refs.push(ref)
  }
  return refs
}

const syncSelectionsFromRefs = () => {
  defaultKeys.value = refsToKeys(defaultRefs.value)
  selectedUserRefs.value = selectedUserRefs.value.filter((ref) => resourcesByRefKey.value.has(refKey(ref)))
  userKeys.value = refsToKeys(selectedUserRefs.value)
}

const ensureSourceUser = () => {
  if (sourceUserId.value && users.value.some((user) => user.id === sourceUserId.value)) return
  const currentUserId = String(authStore.currentUserId || authStore.user?.id || '')
  const currentUser = users.value.find((user) => user.id === currentUserId)
  const firstActive = users.value.find((user) => user.active)
  sourceUserId.value = currentUser?.id || firstActive?.id || users.value[0]?.id || ''
}

const loadResources = async () => {
  if (!sourceUserId.value) {
    resources.value = []
    syncSelectionsFromRefs()
    return
  }
  loadingResources.value = true
  try {
    const res = await listConfigResources(sourceUserId.value)
    resources.value = res.data || []
    syncSelectionsFromRefs()
  } catch (error: any) {
    resources.value = []
    syncSelectionsFromRefs()
    MessagePlugin.error(error?.message || '加载资源失败')
  } finally {
    loadingResources.value = false
  }
}

const loadDefaults = async () => {
  try {
    const res = await getDefaultGrants()
    defaultRefs.value = res.data || []
    defaultKeys.value = refsToKeys(defaultRefs.value)
  } catch (error: any) {
    MessagePlugin.error(error?.message || '加载默认配置失败')
  }
}

const loadUsers = async () => {
  loadingUsers.value = true
  try {
    const res = await listCustomUsers()
    users.value = res.data || []
    ensureSourceUser()
  } catch (error: any) {
    MessagePlugin.error(error?.message || '加载用户失败')
  } finally {
    loadingUsers.value = false
  }
}

const refreshSource = async () => {
  await loadUsers()
  await loadResources()
}

const handleSourceUserChange = async () => {
  resources.value = []
  defaultKeys.value = []
  userKeys.value = []
  await loadResources()
}

const loadSelectedUserGrants = async () => {
  if (!selectedUserId.value) {
    selectedUserRefs.value = []
    userKeys.value = []
    return
  }
  loadingUserGrants.value = true
  try {
    const res = await getUserGrants(selectedUserId.value)
    selectedUserRefs.value = res.data || []
    userKeys.value = refsToKeys(selectedUserRefs.value)
  } catch (error: any) {
    MessagePlugin.error(error?.message || '加载用户权限失败')
  } finally {
    loadingUserGrants.value = false
  }
}

const saveDefaults = async () => {
  savingDefaults.value = true
  try {
    const refs = keysToRefs(defaultKeys.value)
    await saveDefaultGrants(refs)
    defaultRefs.value = refs
    defaultKeys.value = refsToKeys(defaultRefs.value)
    MessagePlugin.success('默认配置已保存')
  } catch (error: any) {
    MessagePlugin.error(error?.message || '保存失败')
  } finally {
    savingDefaults.value = false
  }
}

const saveUser = async () => {
  if (!selectedUserId.value) return
  savingUser.value = true
  try {
    const refs = keysToRefs(userKeys.value)
    await saveUserGrants(selectedUserId.value, refs)
    selectedUserRefs.value = refs
    userKeys.value = refsToKeys(selectedUserRefs.value)
    MessagePlugin.success('用户权限已保存')
  } catch (error: any) {
    MessagePlugin.error(error?.message || '保存失败')
  } finally {
    savingUser.value = false
  }
}

const applyAll = async () => {
  applyingAll.value = true
  try {
    const res = await applyDefaultConfig()
    MessagePlugin.success(`已下发 ${res.data?.users_applied || 0} 个用户，${res.data?.resources || 0} 项资源`)
  } catch (error: any) {
    MessagePlugin.error(error?.message || '下发失败')
  } finally {
    applyingAll.value = false
  }
}

const applyUser = async () => {
  if (!selectedUserId.value) return
  applyingUser.value = true
  try {
    const res = await applyUserConfig(selectedUserId.value)
    MessagePlugin.success(`已下发 ${res.data?.resources || 0} 项资源`)
  } catch (error: any) {
    MessagePlugin.error(error?.message || '下发失败')
  } finally {
    applyingUser.value = false
  }
}

onMounted(async () => {
  await loadUsers()
  await Promise.all([loadResources(), loadDefaults()])
})
</script>

<style lang="less" scoped>
.custom-config-settings {
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
  max-width: 60%;
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
  min-width: 380px;
  display: flex;
  justify-content: flex-end;
  align-items: center;
}

.setting-control--inline,
.toolbar,
.action-row {
  display: flex;
  align-items: center;
  justify-content: flex-end;
  gap: 10px;
}

.toolbar {
  justify-content: flex-start;
  margin-bottom: 14px;
}

.action-row {
  margin-top: 16px;
}

.wide-select {
  width: 360px;
}
</style>
