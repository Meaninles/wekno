<template>
  <main class="scheduled-page">
    <header class="scheduled-header">
      <div>
        <h1>定时任务</h1>
        <p>按小时、每日、每周或每月自动发起智能体对话。</p>
      </div>
      <t-button theme="primary" @click="openCreate">新建任务</t-button>
    </header>

    <section class="scheduled-toolbar">
      <t-button variant="outline" @click="loadAll">刷新</t-button>
      <span class="scheduled-toolbar__meta">共 {{ tasks.length }} 个任务</span>
    </section>

    <section v-if="loading" class="scheduled-state">加载中...</section>
    <section v-else-if="tasks.length === 0" class="scheduled-state">暂无定时任务</section>
    <section v-else class="task-table">
      <div class="task-row task-row--head">
        <span>任务</span>
        <span>智能体</span>
        <span>规则</span>
        <span>下次执行</span>
        <span>状态</span>
        <span>操作</span>
      </div>
      <div v-for="task in tasks" :key="task.id" class="task-row">
        <div class="task-main">
          <strong>{{ task.name }}</strong>
          <span>{{ task.description || '无描述' }}</span>
        </div>
        <span>{{ task.agent_name_snapshot || task.agent_id }}</span>
        <span>{{ scheduleLabel(task) }}</span>
        <span>{{ formatTime(task.next_run_at) || '-' }}</span>
        <div class="task-status">
          <t-switch :value="task.enabled" size="small" @change="(v: unknown) => toggleTask(task, Boolean(v))" />
          <t-tag size="small" :theme="statusTheme(task.last_status)">{{ statusLabel(task.last_status) }}</t-tag>
        </div>
        <div class="task-actions">
          <t-button size="small" variant="text" @click="openRuns(task)">记录</t-button>
          <t-button size="small" variant="text" @click="runNow(task)">立即执行</t-button>
          <t-button
            v-if="task.last_session_id"
            size="small"
            variant="text"
            @click="goSession(task.last_session_id)"
          >
            会话
          </t-button>
          <t-button size="small" variant="text" @click="openEdit(task)">编辑</t-button>
          <t-button size="small" variant="text" theme="danger" @click="confirmDelete(task)">删除</t-button>
        </div>
      </div>
    </section>

    <t-drawer
      v-model:visible="drawerVisible"
      :header="editingTask ? '编辑定时任务' : '新建定时任务'"
      size="720px"
      :close-btn="true"
      :footer="false"
    >
      <div class="task-form">
        <label class="form-field">
          <span>任务名称</span>
          <t-input v-model="form.name" placeholder="例如：每日经营摘要" />
        </label>

        <label class="form-field">
          <span>智能体</span>
          <t-select v-model="form.agent_id" filterable placeholder="选择智能体">
            <t-option v-for="agent in agents" :key="agent.id" :value="agent.id" :label="agent.name" />
          </t-select>
        </label>

        <label class="form-field">
          <span>描述</span>
          <t-input v-model="form.description" placeholder="可选" />
        </label>

        <div class="form-grid">
          <label class="form-field">
            <span>调度方式</span>
            <t-select v-model="form.schedule_type">
              <t-option value="hourly" label="每小时" />
              <t-option value="daily" label="每日" />
              <t-option value="weekly" label="每周" />
              <t-option value="monthly" label="每月" />
            </t-select>
          </label>
          <label class="form-field">
            <span>时区</span>
            <t-input v-model="form.timezone" />
          </label>
        </div>

        <div class="schedule-grid">
          <label v-if="form.schedule_type !== 'hourly'" class="form-field">
            <span>小时</span>
            <t-input-number v-model="form.hour" :min="0" :max="23" />
          </label>
          <label class="form-field">
            <span>分钟</span>
            <t-input-number v-model="form.minute" :min="0" :max="59" />
          </label>
          <label v-if="form.schedule_type === 'weekly'" class="form-field">
            <span>星期</span>
            <t-select v-model="form.weekday">
              <t-option :value="1" label="周一" />
              <t-option :value="2" label="周二" />
              <t-option :value="3" label="周三" />
              <t-option :value="4" label="周四" />
              <t-option :value="5" label="周五" />
              <t-option :value="6" label="周六" />
              <t-option :value="7" label="周日" />
            </t-select>
          </label>
          <label v-if="form.schedule_type === 'monthly'" class="form-field">
            <span>日期</span>
            <t-input-number v-model="form.day_of_month" :min="1" :max="31" />
          </label>
        </div>

        <label class="switch-field">
          <t-switch v-model="form.enabled" />
          <span>启用任务</span>
        </label>

        <label class="switch-field">
          <t-switch v-model="form.web_search_enabled" />
          <span>允许本次对话使用网络搜索</span>
        </label>

        <div class="template-row">
          <t-select v-model="selectedTemplateId" placeholder="选择模板" clearable @change="applySelectedTemplate">
            <t-option
              v-for="tpl in templates"
              :key="tpl.id"
              :value="tpl.id"
              :label="`${tpl.name} - ${tpl.description}`"
            />
          </t-select>
          <t-button variant="outline" @click="previewPrompt">预览</t-button>
        </div>

        <div class="variable-list">
          <button v-for="item in variables" :key="item.name" type="button" @click="insertVariable(item.name)">
            {{ item.label }} <code>{{ variableToken(item.name) }}</code>
          </button>
        </div>

        <label class="form-field">
          <span>固定对话指令</span>
          <textarea
            ref="promptInput"
            v-model="form.prompt_template"
            class="prompt-editor"
            rows="12"
            placeholder="输入任务触发后发送给智能体的固定对话指令"
          />
        </label>

        <div v-if="previewContent" class="prompt-preview">
          <strong>渲染预览</strong>
          <pre>{{ previewContent }}</pre>
        </div>

        <footer class="drawer-actions">
          <t-button variant="outline" @click="drawerVisible = false">取消</t-button>
          <t-button theme="primary" :loading="saving" @click="saveTask">保存</t-button>
        </footer>
      </div>
    </t-drawer>

    <t-drawer
      v-model:visible="runsVisible"
      header="运行记录"
      size="680px"
      :footer="false"
    >
      <section v-if="runs.length === 0" class="scheduled-state">暂无运行记录</section>
      <div v-else class="run-list">
        <article v-for="run in runs" :key="run.id" class="run-row">
          <div>
            <strong>{{ formatTime(run.scheduled_at) }}</strong>
            <span>{{ run.triggered_by === 'manual' ? '手动执行' : '定时触发' }}</span>
          </div>
          <t-tag size="small" :theme="statusTheme(run.status)">{{ statusLabel(run.status) }}</t-tag>
          <button v-if="run.session_id" type="button" @click="goSession(run.session_id)">查看会话</button>
          <p v-if="run.error_message">{{ run.error_message }}</p>
          <pre v-if="run.rendered_prompt">{{ run.rendered_prompt }}</pre>
        </article>
      </div>
    </t-drawer>
  </main>
</template>

<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { useRouter } from 'vue-router'
import { DialogPlugin, MessagePlugin } from 'tdesign-vue-next'
import { listAgents, type CustomAgent } from '@/api/agent'
import {
  createScheduledChatTask,
  deleteScheduledChatTask,
  getScheduledChatPromptTemplates,
  getScheduledChatVariables,
  listScheduledChatRuns,
  listScheduledChatTasks,
  renderScheduledChatPreview,
  runScheduledChatTaskNow,
  updateScheduledChatTask,
  type ScheduledChatPromptTemplate,
  type ScheduledChatRun,
  type ScheduledChatTask,
  type ScheduledChatTaskPayload,
  type ScheduledChatVariable,
  type ScheduleType,
} from './api'

const router = useRouter()
const tasks = ref<ScheduledChatTask[]>([])
const agents = ref<CustomAgent[]>([])
const variables = ref<ScheduledChatVariable[]>([])
const templates = ref<ScheduledChatPromptTemplate[]>([])
const runs = ref<ScheduledChatRun[]>([])
const loading = ref(false)
const saving = ref(false)
const drawerVisible = ref(false)
const runsVisible = ref(false)
const editingTask = ref<ScheduledChatTask | null>(null)
const selectedTemplateId = ref('')
const previewContent = ref('')
const promptInput = ref<HTMLTextAreaElement | null>(null)

const defaultTimezone = Intl.DateTimeFormat().resolvedOptions().timeZone || 'Asia/Shanghai'

const form = reactive<ScheduledChatTaskPayload>({
  name: '',
  description: '',
  enabled: true,
  agent_id: '',
  schedule_type: 'daily',
  timezone: defaultTimezone,
  minute: 0,
  hour: 9,
  weekday: 1,
  day_of_month: 1,
  prompt_template: '',
  web_search_enabled: false,
})

const firstAgentId = computed(() => agents.value[0]?.id || '')

onMounted(loadAll)

async function loadAll() {
  loading.value = true
  try {
    const [taskRes, agentRes, variableRes, templateRes] = await Promise.all([
      listScheduledChatTasks(),
      listAgents(),
      getScheduledChatVariables(),
      getScheduledChatPromptTemplates(),
    ])
    tasks.value = taskRes.data || []
    agents.value = agentRes.data || []
    variables.value = variableRes.data || []
    templates.value = templateRes.data || []
  } catch (e: any) {
    MessagePlugin.error(e?.message || '加载定时任务失败')
  } finally {
    loading.value = false
  }
}

function resetForm() {
  form.name = ''
  form.description = ''
  form.enabled = true
  form.agent_id = firstAgentId.value
  form.schedule_type = 'daily'
  form.timezone = defaultTimezone
  form.minute = 0
  form.hour = 9
  form.weekday = 1
  form.day_of_month = 1
  form.prompt_template = ''
  form.web_search_enabled = false
  selectedTemplateId.value = ''
  previewContent.value = ''
}

function openCreate() {
  editingTask.value = null
  resetForm()
  drawerVisible.value = true
}

function openEdit(task: ScheduledChatTask) {
  editingTask.value = task
  form.name = task.name
  form.description = task.description || ''
  form.enabled = task.enabled
  form.agent_id = task.agent_id
  form.schedule_type = task.schedule_type
  form.timezone = task.timezone || defaultTimezone
  form.minute = task.minute
  form.hour = task.hour
  form.weekday = task.weekday
  form.day_of_month = task.day_of_month
  form.prompt_template = task.prompt_template
  form.web_search_enabled = task.web_search_enabled
  selectedTemplateId.value = ''
  previewContent.value = ''
  drawerVisible.value = true
}

async function saveTask() {
  if (!form.name.trim()) {
    MessagePlugin.warning('请填写任务名称')
    return
  }
  if (!form.agent_id) {
    MessagePlugin.warning('请选择智能体')
    return
  }
  if (!form.prompt_template.trim()) {
    MessagePlugin.warning('请填写固定对话指令')
    return
  }
  saving.value = true
  try {
    if (editingTask.value) {
      await updateScheduledChatTask(editingTask.value.id, { ...form })
    } else {
      await createScheduledChatTask({ ...form })
    }
    MessagePlugin.success('保存成功')
    drawerVisible.value = false
    await loadAll()
  } catch (e: any) {
    MessagePlugin.error(e?.message || '保存失败')
  } finally {
    saving.value = false
  }
}

async function toggleTask(task: ScheduledChatTask, enabled: boolean) {
  try {
    await updateScheduledChatTask(task.id, {
      name: task.name,
      description: task.description || '',
      enabled,
      agent_id: task.agent_id,
      schedule_type: task.schedule_type,
      timezone: task.timezone,
      minute: task.minute,
      hour: task.hour,
      weekday: task.weekday,
      day_of_month: task.day_of_month,
      prompt_template: task.prompt_template,
      web_search_enabled: task.web_search_enabled,
    })
    await loadAll()
  } catch (e: any) {
    MessagePlugin.error(e?.message || '更新失败')
  }
}

function confirmDelete(task: ScheduledChatTask) {
  let dialog: ReturnType<typeof DialogPlugin.confirm> | undefined
  const closeDialog = () => {
    dialog?.destroy()
    dialog = undefined
  }
  dialog = DialogPlugin.confirm({
    header: '删除定时任务',
    body: `确定删除「${task.name}」吗？`,
    confirmBtn: '删除',
    cancelBtn: '取消',
    theme: 'danger',
    onConfirm: async () => {
      try {
        await deleteScheduledChatTask(task.id)
        MessagePlugin.success('已删除')
        await loadAll()
      } finally {
        closeDialog()
      }
    },
    onClose: closeDialog,
  })
}

async function runNow(task: ScheduledChatTask) {
  try {
    await runScheduledChatTaskNow(task.id)
    MessagePlugin.success('已发起执行')
    setTimeout(loadAll, 1200)
  } catch (e: any) {
    MessagePlugin.error(e?.message || '执行失败')
  }
}

async function openRuns(task: ScheduledChatTask) {
  runsVisible.value = true
  const res = await listScheduledChatRuns(task.id, 30)
  runs.value = res.data || []
}

function goSession(sessionId: string) {
  router.push(`/platform/chat/${sessionId}`)
}

function applySelectedTemplate(value: string | number | undefined) {
  const id = String(value || selectedTemplateId.value || '')
  const tpl = templates.value.find(item => item.id === id)
  if (!tpl) return
  if (form.prompt_template.trim()) {
    DialogPlugin.confirm({
      header: '应用模板',
      body: '当前固定指令已有内容，是否用模板覆盖？',
      confirmBtn: '覆盖',
      cancelBtn: '取消',
      onConfirm: () => {
        form.prompt_template = tpl.content
      },
    })
  } else {
    form.prompt_template = tpl.content
  }
}

function insertVariable(name: string) {
  const token = variableToken(name)
  const el = promptInput.value
  if (!el) {
    form.prompt_template += token
    return
  }
  const start = el.selectionStart ?? form.prompt_template.length
  const end = el.selectionEnd ?? start
  form.prompt_template = `${form.prompt_template.slice(0, start)}${token}${form.prompt_template.slice(end)}`
  requestAnimationFrame(() => {
    el.focus()
    el.setSelectionRange(start + token.length, start + token.length)
  })
}

function variableToken(name: string) {
  return `{{${name}}}`
}

async function previewPrompt() {
  try {
    const res = await renderScheduledChatPreview({
      prompt_template: form.prompt_template,
      task_name: form.name,
      agent_id: form.agent_id,
      timezone: form.timezone,
    })
    previewContent.value = res.data?.content || ''
  } catch (e: any) {
    MessagePlugin.error(e?.message || '预览失败')
  }
}

function scheduleLabel(task: ScheduledChatTask) {
  const timeText = `${pad(task.hour)}:${pad(task.minute)}`
  if (task.schedule_type === 'hourly') return `每小时第 ${task.minute} 分钟`
  if (task.schedule_type === 'daily') return `每日 ${timeText}`
  if (task.schedule_type === 'weekly') return `每周${weekdayLabel(task.weekday)} ${timeText}`
  return `每月 ${task.day_of_month} 日 ${timeText}`
}

function weekdayLabel(value: number) {
  return ['一', '二', '三', '四', '五', '六', '日'][Math.max(0, Math.min(6, value - 1))]
}

function pad(value: number) {
  return String(value).padStart(2, '0')
}

function formatTime(value?: string) {
  if (!value) return ''
  return new Date(value).toLocaleString()
}

function statusLabel(status?: string) {
  if (status === 'running') return '运行中'
  if (status === 'success') return '成功'
  if (status === 'failed') return '失败'
  if (status === 'skipped') return '已跳过'
  return '未运行'
}

function statusTheme(status?: string) {
  if (status === 'success') return 'success'
  if (status === 'failed') return 'danger'
  if (status === 'running') return 'primary'
  if (status === 'skipped') return 'warning'
  return 'default'
}
</script>

<style scoped>
.scheduled-page {
  min-height: 100%;
  padding: 24px 32px;
  background: var(--td-bg-color-page);
  color: var(--td-text-color-primary);
}

.scheduled-header {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 16px;
  margin-bottom: 20px;
}

.scheduled-header h1 {
  margin: 0 0 6px;
  font-size: 24px;
  font-weight: 600;
}

.scheduled-header p {
  margin: 0;
  color: var(--td-text-color-secondary);
}

.scheduled-toolbar {
  display: flex;
  align-items: center;
  gap: 12px;
  margin-bottom: 12px;
}

.scheduled-toolbar__meta,
.task-main span,
.run-row span {
  color: var(--td-text-color-secondary);
  font-size: 13px;
}

.scheduled-state {
  padding: 40px 0;
  text-align: center;
  color: var(--td-text-color-secondary);
}

.task-table {
  overflow: hidden;
  border: 1px solid var(--td-component-border);
  background: var(--td-bg-color-container);
}

.task-row {
  display: grid;
  grid-template-columns: minmax(180px, 1.5fr) minmax(140px, 1fr) minmax(150px, 1fr) minmax(160px, 1fr) 130px minmax(260px, 1.5fr);
  gap: 12px;
  align-items: center;
  min-height: 64px;
  padding: 12px 16px;
  border-bottom: 1px solid var(--td-component-border);
}

.task-row:last-child {
  border-bottom: 0;
}

.task-row--head {
  min-height: 42px;
  font-size: 13px;
  font-weight: 600;
  color: var(--td-text-color-secondary);
  background: var(--td-bg-color-secondarycontainer);
}

.task-main {
  display: flex;
  flex-direction: column;
  gap: 4px;
  min-width: 0;
}

.task-status,
.task-actions,
.drawer-actions,
.template-row {
  display: flex;
  align-items: center;
  gap: 8px;
}

.task-actions {
  flex-wrap: wrap;
}

.task-form {
  display: flex;
  flex-direction: column;
  gap: 16px;
  padding-bottom: 16px;
}

.form-grid,
.schedule-grid {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: 12px;
}

.schedule-grid {
  grid-template-columns: repeat(4, minmax(0, 1fr));
}

.form-field {
  display: flex;
  flex-direction: column;
  gap: 8px;
  font-size: 13px;
  font-weight: 600;
}

.switch-field {
  display: flex;
  align-items: center;
  gap: 8px;
  font-size: 14px;
}

.template-row > :first-child {
  flex: 1;
}

.variable-list {
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
}

.variable-list button {
  border: 1px solid var(--td-component-border);
  background: var(--td-bg-color-container);
  color: var(--td-text-color-primary);
  border-radius: 4px;
  padding: 5px 8px;
  cursor: pointer;
}

.variable-list button:hover {
  border-color: var(--td-brand-color);
  color: var(--td-brand-color);
}

.variable-list code {
  color: var(--td-text-color-secondary);
}

.prompt-editor {
  width: 100%;
  resize: vertical;
  box-sizing: border-box;
  border: 1px solid var(--td-component-border);
  border-radius: 4px;
  padding: 10px;
  font: 14px/1.6 var(--td-font-family);
  color: var(--td-text-color-primary);
  background: var(--td-bg-color-container);
}

.prompt-editor:focus {
  outline: none;
  border-color: var(--td-brand-color);
}

.prompt-preview,
.run-row pre {
  border: 1px solid var(--td-component-border);
  background: var(--td-bg-color-secondarycontainer);
}

.prompt-preview {
  padding: 12px;
}

.prompt-preview pre,
.run-row pre {
  white-space: pre-wrap;
  word-break: break-word;
  margin: 8px 0 0;
  font: 13px/1.6 var(--td-font-family);
}

.drawer-actions {
  justify-content: flex-end;
}

.run-list {
  display: flex;
  flex-direction: column;
  gap: 12px;
}

.run-row {
  display: grid;
  grid-template-columns: 1fr auto auto;
  gap: 10px;
  align-items: start;
  border-bottom: 1px solid var(--td-component-border);
  padding-bottom: 12px;
}

.run-row button {
  border: 0;
  background: transparent;
  color: var(--td-brand-color);
  cursor: pointer;
}

.run-row p,
.run-row pre {
  grid-column: 1 / -1;
  margin: 0;
}

@media (max-width: 1100px) {
  .task-row {
    grid-template-columns: 1fr;
  }

  .task-row--head {
    display: none;
  }
}
</style>
