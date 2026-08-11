<template>
  <div class="iam-skipped-records">
    <div class="detail-header">
      <t-button variant="text" class="back-button" @click="emit('back')">
        <template #icon><t-icon name="chevron-left" /></template>
        返回同步设置
      </t-button>
      <div class="detail-title-row">
        <div>
          <h2>同步跳过记录</h2>
          <p class="section-description">查看本次同步中无法从 IAM 正常读取的人员条目。</p>
        </div>
        <t-button variant="outline" size="small" :loading="loading" @click="loadRecords">
          <template #icon><t-icon name="refresh" /></template>
          刷新
        </t-button>
      </div>
    </div>

    <t-alert theme="warning" class="skip-alert">
      系统仅跳过下列异常条目，其他可读取人员仍会正常关联已有用户；无法关联的正常记录仍按新用户处理。
    </t-alert>

    <div v-if="run" class="run-summary">
      <t-tag :theme="statusTheme(run.status)" variant="light">{{ statusLabel(run.status) }}</t-tag>
      <span>{{ formatTime(run.started_at) }}</span>
      <span>触发方式：{{ run.triggered_by }}</span>
      <span>共跳过 {{ total }} 条</span>
    </div>

    <div v-if="error" class="state-block">
      <t-alert theme="error" :message="error">
        <template #operation>
          <t-button size="small" @click="loadRecords">重试</t-button>
        </template>
      </t-alert>
    </div>
    <div v-else-if="loading && records.length === 0" class="state-block state-block--plain">
      正在加载跳过记录…
    </div>
    <div v-else-if="total === 0" class="state-block state-block--plain">
      本次同步没有跳过记录。
    </div>
    <div v-else class="data-table-shell data-table-shell--with-footer">
      <div class="data-table-shell__scroll">
        <t-table row-key="id" :data="records" :columns="columns" size="medium" hover stripe :loading="loading">
          <template #position="{ row }">
            <div class="position-cell">
              <span>第 {{ row.absolute_offset + 1 }} 条</span>
              <span class="cell-secondary">原分页第 {{ row.source_page_number + 1 }} 页</span>
            </div>
          </template>
          <template #user_readable_reason="{ row }">
            <div class="wrap-cell">{{ row.user_readable_reason }}</div>
          </template>
          <template #error_code="{ row }">
            <div class="error-code-cell">
              <t-tag theme="warning" variant="light" size="small">{{ row.error_code || `HTTP ${row.http_status}` }}</t-tag>
              <span v-if="row.error_code && row.http_status" class="cell-secondary">HTTP {{ row.http_status }}</span>
              <span v-if="row.error_name" class="cell-secondary">{{ row.error_name }}</span>
            </div>
          </template>
          <template #error_message="{ row }">
            <div class="wrap-cell">{{ row.error_message || '-' }}</div>
          </template>
          <template #created_at="{ row }">{{ formatTime(row.created_at) }}</template>
        </t-table>
      </div>
      <div class="data-table-shell__pager">
        <t-pagination v-model="page" v-model:page-size="pageSize" :total="total" size="small"
          show-jumper show-page-number show-page-size :page-size-options="PAGE_SIZE_OPTIONS"
          @change="loadRecords" />
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref, watch } from 'vue'
import {
  listIAMSyncSkippedRecords,
  type IAMSyncRun,
  type IAMSyncSkippedRecord,
} from '@/api/custom-admin'

const props = defineProps<{ runId: string }>()
const emit = defineEmits<{ back: [] }>()

const PAGE_SIZE_OPTIONS = [10, 20, 50, 100]
const columns = [
  { colKey: 'position', title: 'IAM 查询位置', width: 142 },
  { colKey: 'user_readable_reason', title: '跳过原因', minWidth: 300 },
  { colKey: 'error_code', title: '错误代码', width: 150 },
  { colKey: 'error_message', title: 'IAM 返回信息', minWidth: 210 },
  { colKey: 'created_at', title: '记录时间', width: 180 },
]

const run = ref<IAMSyncRun | null>(null)
const records = ref<IAMSyncSkippedRecord[]>([])
const total = ref(0)
const page = ref(1)
const pageSize = ref(20)
const loading = ref(false)
const error = ref('')

async function loadRecords() {
  if (!props.runId) return
  loading.value = true
  error.value = ''
  try {
    const response = await listIAMSyncSkippedRecords(props.runId, {
      page: page.value,
      pageSize: pageSize.value,
    })
    const data = response.data
    const nextTotal = data?.total ?? 0
    const nextPageSize = Math.max(1, data?.page_size ?? pageSize.value)
    const maxPage = Math.max(1, Math.ceil(nextTotal / nextPageSize))
    if (page.value > maxPage) {
      page.value = maxPage
      loading.value = false
      await loadRecords()
      return
    }
    run.value = data?.run || null
    records.value = data?.records || []
    total.value = nextTotal
    page.value = data?.page || page.value
    pageSize.value = nextPageSize
  } catch (err: any) {
    error.value = err?.message || '加载跳过记录失败'
  } finally {
    loading.value = false
  }
}

function formatTime(value?: string) {
  if (!value) return '-'
  return new Date(value).toLocaleString()
}

function statusLabel(status: string) {
  if (status === 'success') return '成功'
  if (status === 'partial_success') return '部分成功'
  if (status === 'failed') return '失败'
  if (status === 'running') return '同步中'
  return status || '-'
}

function statusTheme(status: string): 'success' | 'warning' | 'danger' | 'primary' | 'default' {
  if (status === 'success') return 'success'
  if (status === 'partial_success') return 'warning'
  if (status === 'failed') return 'danger'
  if (status === 'running') return 'primary'
  return 'default'
}

watch(
  () => props.runId,
  () => {
    page.value = 1
    records.value = []
    total.value = 0
    run.value = null
    void loadRecords()
  },
  { immediate: true },
)
</script>

<style lang="less" scoped>
.iam-skipped-records {
  width: 100%;
}

.detail-header {
  margin-bottom: 18px;
}

.back-button {
  margin: -6px 0 12px -8px;
}

.detail-title-row {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 20px;

  h2 {
    margin: 0 0 8px;
    color: var(--td-text-color-primary);
    font-size: 20px;
    font-weight: 600;
  }
}

.section-description {
  margin: 0;
  color: var(--td-text-color-secondary);
  font-size: 14px;
  line-height: 1.5;
}

.skip-alert {
  margin-bottom: 14px;
}

.run-summary {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 8px 12px;
  margin-bottom: 14px;
  color: var(--td-text-color-secondary);
  font-size: 13px;
}

.state-block {
  min-height: 96px;
}

.state-block--plain {
  display: flex;
  align-items: center;
  justify-content: center;
  border: 1px dashed var(--td-component-stroke);
  border-radius: 8px;
  color: var(--td-text-color-placeholder);
}

.data-table-shell {
  width: 100%;
  border: 1px solid var(--td-component-stroke);
  border-radius: 8px;
  background: var(--td-bg-color-container);
}

.data-table-shell.data-table-shell--with-footer {
  display: flex;
  flex-direction: column;
  overflow: hidden;

  > .data-table-shell__scroll {
    min-width: 0;
    overflow-x: auto;
  }

  > .data-table-shell__pager {
    display: flex;
    flex-shrink: 0;
    align-items: center;
    justify-content: flex-end;
    flex-wrap: wrap;
    gap: 8px 12px;
    padding: 10px 14px;
    border-top: 1px solid var(--td-component-stroke);
    background-color: var(--td-bg-color-container);

    :deep(.t-pagination) {
      justify-content: flex-end;
      flex-wrap: wrap;
      row-gap: 8px;
    }
  }
}

.position-cell,
.error-code-cell {
  display: flex;
  flex-direction: column;
  align-items: flex-start;
  gap: 4px;
}

.cell-secondary {
  color: var(--td-text-color-placeholder);
  font-size: 12px;
}

.wrap-cell {
  min-width: 0;
  color: var(--td-text-color-secondary);
  line-height: 1.55;
  overflow-wrap: anywhere;
  white-space: normal;
}
</style>
