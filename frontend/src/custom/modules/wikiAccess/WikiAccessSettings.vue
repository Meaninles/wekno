<template>
  <div class="wiki-access-settings">
    <div class="section-header">
      <div>
        <h2>Wiki 权限</h2>
        <p class="section-description">Wiki 默认对所有用户关闭。仅被单独授权的用户可以在知识库中选择 Wiki 索引。</p>
      </div>
    </div>

    <div class="permission-note">
      <t-icon name="info-circle" size="16px" />
      <span>关闭权限不会删除或停用该用户已有的 Wiki 知识库，只会阻止其新开启 Wiki。</span>
    </div>

    <div class="users-group">
      <div class="users-header">
        <div class="users-search">
          <t-input v-model="query" clearable placeholder="搜索用户名、姓名、用户 ID 或空间名">
            <template #prefix-icon><t-icon name="search" /></template>
          </t-input>
        </div>
        <span class="result-count">共 {{ total }} 位用户</span>
      </div>

      <t-loading :loading="loading" class="user-list-loading">
        <div class="user-list">
          <div v-for="user in users" :key="user.id" class="user-item">
            <div class="user-avatar">
              <t-icon name="user" size="20px" />
            </div>
            <div class="user-info">
              <div class="user-title">
                <span>{{ user.display_name || user.username }}</span>
                <t-tag v-if="user.is_system_admin" size="small" theme="warning" variant="light">系统管理员</t-tag>
                <t-tag v-if="!user.is_active" size="small" theme="danger" variant="light">已禁用</t-tag>
              </div>
              <div class="user-secondary">
                <span v-if="user.display_name && user.display_name !== user.username">{{ user.username }}</span>
                <span>{{ user.tenant_name || `tenant#${user.tenant_id}` }}</span>
                <span class="user-id">{{ user.id }}</span>
              </div>
            </div>
            <div class="permission-control">
              <span>{{ user.wiki_enabled ? '已开放' : '未开放' }}</span>
              <t-switch
                :model-value="user.wiki_enabled"
                :loading="updatingUserIds.has(user.id)"
                @change="(value: string | number | boolean) => updatePermission(user, value === true)"
              />
            </div>
          </div>
          <div v-if="users.length === 0 && !loading" class="empty-state">
            没有匹配的用户
          </div>
        </div>
      </t-loading>

      <div v-if="total > 0" class="users-pagination">
        <t-pagination
          v-model="page"
          v-model:page-size="pageSize"
          :total="total"
          size="small"
          show-jumper
          show-page-number
          show-page-size
          :page-size-options="PAGE_SIZE_OPTIONS"
          @change="onPageChange"
        />
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { MessagePlugin } from 'tdesign-vue-next'

import {
  listWikiAccessUsers,
  setWikiAccessUser,
  type WikiAccessUser,
} from './api'

const query = ref('')
const appliedQuery = ref('')
const loading = ref(false)
const users = ref<WikiAccessUser[]>([])
const total = ref(0)
const page = ref(1)
const pageSize = ref(20)
const updatingUserIds = ref(new Set<string>())
const PAGE_SIZE_OPTIONS = [20, 50, 100]
let searchDebounceTimer: ReturnType<typeof setTimeout> | null = null
let requestSequence = 0

async function loadUsers() {
  const currentRequest = ++requestSequence
  loading.value = true
  try {
    const response = await listWikiAccessUsers(appliedQuery.value, page.value, pageSize.value)
    if (currentRequest !== requestSequence) return
    const result = response.data
    const resultTotal = result?.total ?? 0
    const resultPageSize = result?.page_size || pageSize.value
    const maxPage = Math.max(1, Math.ceil(resultTotal / Math.max(1, resultPageSize)))
    if (page.value > maxPage) {
      page.value = maxPage
      void loadUsers()
      return
    }
    users.value = result?.users || []
    total.value = resultTotal
    if (result?.page > 0) page.value = result.page
    if (result?.page_size > 0) pageSize.value = result.page_size
  } catch (error: any) {
    if (currentRequest !== requestSequence) return
    users.value = []
    total.value = 0
    MessagePlugin.error(error?.message || 'Wiki 权限列表加载失败')
  } finally {
    if (currentRequest === requestSequence) loading.value = false
  }
}

function onPageChange() {
  void loadUsers()
}

async function updatePermission(user: WikiAccessUser, enabled: boolean) {
  if (updatingUserIds.value.has(user.id) || user.wiki_enabled === enabled) return
  const previous = user.wiki_enabled
  user.wiki_enabled = enabled
  updatingUserIds.value.add(user.id)
  updatingUserIds.value = new Set(updatingUserIds.value)
  try {
    const response = await setWikiAccessUser(user.id, enabled)
    Object.assign(user, response.data)
    MessagePlugin.success(enabled ? '已开放 Wiki 权限' : '已关闭 Wiki 权限')
  } catch (error: any) {
    user.wiki_enabled = previous
    MessagePlugin.error(error?.message || 'Wiki 权限更新失败')
  } finally {
    updatingUserIds.value.delete(user.id)
    updatingUserIds.value = new Set(updatingUserIds.value)
  }
}

watch(query, (value) => {
  if (searchDebounceTimer) clearTimeout(searchDebounceTimer)
  searchDebounceTimer = setTimeout(() => {
    searchDebounceTimer = null
    appliedQuery.value = value.trim()
    page.value = 1
    void loadUsers()
  }, 320)
})

onMounted(loadUsers)

onBeforeUnmount(() => {
  requestSequence += 1
  if (searchDebounceTimer) clearTimeout(searchDebounceTimer)
})
</script>

<style scoped lang="less">
.wiki-access-settings {
  width: 100%;
}

.section-header {
  margin-bottom: 20px;

  h2 {
    margin: 0 0 8px;
    color: var(--td-text-color-primary);
    font-size: 20px;
  }
}

.section-description {
  margin: 0;
  color: var(--td-text-color-secondary);
  font-size: 14px;
  line-height: 1.6;
}

.permission-note {
  display: flex;
  align-items: flex-start;
  gap: 8px;
  margin: 0 0 16px;
  padding: 10px 12px;
  border-radius: 6px;
  color: var(--td-text-color-secondary);
  background: var(--td-bg-color-secondarycontainer);
  font-size: 13px;
  line-height: 20px;

  :deep(.t-icon) {
    flex-shrink: 0;
    margin-top: 2px;
  }
}

.users-group {
  display: flex;
  flex-direction: column;
}

.users-header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 16px;
  margin-bottom: 16px;
}

.users-search {
  flex: 1;
}

.result-count {
  flex-shrink: 0;
  color: var(--td-text-color-secondary);
  font-size: 13px;
}

.user-list-loading {
  min-height: 160px;
}

.user-list {
  display: flex;
  flex-direction: column;
  gap: 8px;
  min-height: 80px;
  max-height: 400px;
  overflow-y: auto;
}

.user-item {
  display: flex;
  align-items: center;
  gap: 12px;
  padding: 12px 16px;
  border-radius: 8px;
  background: var(--td-bg-color-container);
  transition: background 0.2s;

  &:hover {
    background: var(--td-bg-color-component);
  }
}

.user-avatar {
  display: flex;
  flex: 0 0 36px;
  align-items: center;
  justify-content: center;
  width: 36px;
  height: 36px;
  border-radius: 50%;
  color: var(--td-text-color-secondary);
  background: var(--td-bg-color-secondarycontainer);
}

.user-info {
  flex: 1;
  min-width: 0;
}

.user-title,
.user-secondary,
.permission-control {
  display: flex;
  align-items: center;
}

.user-title {
  gap: 8px;
  color: var(--td-text-color-primary);
  font-size: 14px;
  font-weight: 500;
}

.user-secondary {
  flex-wrap: wrap;
  gap: 6px 14px;
  margin-top: 3px;
  color: var(--td-text-color-secondary);
  font-size: 12px;
}

.user-id {
  font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
}

.permission-control {
  flex-shrink: 0;
  gap: 10px;
  color: var(--td-text-color-secondary);
  font-size: 13px;
}

.empty-state {
  padding: 48px 12px;
  color: var(--td-text-color-placeholder);
  text-align: center;
}

.users-pagination {
  display: flex;
  justify-content: flex-end;
  padding-top: 16px;
}

@media (max-width: 720px) {
  .users-header,
  .user-item {
    align-items: flex-start;
    flex-direction: column;
  }

  .users-search {
    width: 100%;
  }

  .user-avatar {
    display: none;
  }

  .permission-control {
    width: 100%;
    justify-content: space-between;
  }
}
</style>
