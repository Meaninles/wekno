<template>
  <div class="iam-sync-settings">
    <div class="section-header">
      <h2>组织人员同步</h2>
      <p class="section-description">配置统一身份认证登录与组织人员同步，支持手动同步和定时同步。</p>
    </div>

    <div class="settings-group">
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
          <t-button variant="outline" :loading="syncButtonLoading" @click="syncNow">
            <template #icon><t-icon name="refresh" /></template>
            {{ syncButtonLoading ? '同步中' : '立即同步' }}
          </t-button>
          <t-button variant="text" :loading="loadingRuns" @click="loadRuns">
            <template #icon><t-icon name="history" /></template>
            刷新记录
          </t-button>
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
              组织 {{ run.org_count }}，人员 {{ run.user_count }}，新建 {{ run.created_users }}，更新 {{ run.updated_users }}，禁用 {{ run.disabled_users }}
            </div>
            <div v-if="run.message && run.message !== 'ok'" class="run-message">{{ run.message }}</div>
          </div>
        </div>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, onUnmounted, reactive, ref } from 'vue'
import { MessagePlugin } from 'tdesign-vue-next'
import {
  getIAMSyncSetting,
  listIAMSyncRuns,
  runIAMSync,
  saveIAMSyncSetting,
  type IAMSyncRun,
  type IAMSyncSetting,
} from '@/api/custom-admin'

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
let pollingTimer: ReturnType<typeof setInterval> | null = null

const weekdayValues = computed<string[]>({
  get: () => (form.weekdays || '').split(',').map((item) => item.trim()).filter(Boolean),
  set: (value) => {
    form.weekdays = value.join(',')
  },
})

const hasRunningRun = computed(() => runs.value.some((run) => run.status === 'running'))
const syncButtonLoading = computed(() => syncing.value || hasRunningRun.value)

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

const syncNow = async () => {
  syncing.value = true
  try {
    const res = await runIAMSync()
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
.run-message {
  margin-top: 6px;
  color: var(--td-text-color-secondary);
  font-size: 12px;
  line-height: 1.5;
}

.run-message {
  color: var(--td-error-color);
}
</style>
