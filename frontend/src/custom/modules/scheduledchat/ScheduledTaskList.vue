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

        <section class="capability-panel">
          <div class="capability-panel__header">
            <span>对话能力</span>
            <small>与直接对话的选择能力保持一致</small>
          </div>

          <div
            v-if="form.web_search_enabled || selectedKnowledgeBaseIds.length > 0 || selectedKnowledgeFiles.length > 0 || selectedSkillNamesModel.length > 0 || selectedProfessionalSkillNamesModel.length > 0 || selectedImageCount > 0 || selectedAttachmentUploads.length > 0"
            class="selected-tags-inline"
          >
            <span v-if="form.web_search_enabled" class="mention-chip mention-chip--web">
              <span class="mention-chip__icon"><t-icon name="search" /></span>
              <span class="mention-chip__name">网络搜索</span>
              <span class="mention-chip__remove" @click.stop="form.web_search_enabled = false">×</span>
            </span>
            <span v-for="id in selectedKnowledgeBaseIds" :key="`kb-${id}`" class="mention-chip mention-chip--kb">
              <span class="mention-chip__icon"><t-icon name="folder" /></span>
              <span class="mention-chip__name">{{ knowledgeBases.find(kb => kb.id === id)?.name || id }}</span>
              <span class="mention-chip__remove" @click.stop="removeKnowledgeBase(id)">×</span>
            </span>
            <span v-for="file in selectedKnowledgeFiles" :key="`file-${file.id}`" class="mention-chip mention-chip--file">
              <span class="mention-chip__icon"><t-icon name="file" /></span>
              <span class="mention-chip__name" :title="file.kbName">{{ file.name }}</span>
              <span class="mention-chip__remove" @click.stop="removeKnowledgeFile(file.id)">×</span>
            </span>
            <span v-for="name in selectedSkillNamesModel" :key="`skill-${name}`" class="mention-chip mention-chip--skill">
              <span class="mention-chip__icon"><t-icon name="lightbulb" /></span>
              <span class="mention-chip__name">{{ name }}</span>
              <span class="mention-chip__remove" @click.stop="removeSelectedSkill(name)">×</span>
            </span>
            <span v-for="name in selectedProfessionalSkillNamesModel" :key="`professional-${name}`" class="mention-chip mention-chip--professional-skill">
              <span class="mention-chip__icon"><t-icon name="tools" /></span>
              <span class="mention-chip__name">{{ name }}</span>
              <span class="mention-chip__remove" @click.stop="removeSelectedProfessionalSkill(name)">×</span>
            </span>
            <span v-for="(_, index) in form.request_context?.images || []" :key="`image-${index}`" class="mention-chip mention-chip--image">
              <span class="mention-chip__icon"><t-icon name="image" /></span>
              <span class="mention-chip__name">图片 {{ index + 1 }}</span>
              <span class="mention-chip__remove" @click.stop="removeImage(index)">×</span>
            </span>
            <span v-for="(file, index) in selectedAttachmentUploads" :key="`attachment-${index}-${file.file_name}`" class="mention-chip mention-chip--attachment">
              <span class="mention-chip__icon"><t-icon name="attachment" /></span>
              <span class="mention-chip__name">{{ file.file_name }} · {{ formatFileSize(file.file_size) }}</span>
              <span class="mention-chip__remove" @click.stop="removeAttachment(index)">×</span>
            </span>
          </div>

          <div class="capability-actions">
            <label class="capability-select">
              <span>知识库</span>
              <t-select v-model="selectedKnowledgeBaseIds" multiple filterable clearable placeholder="选择知识库">
                <t-option
                  v-for="kb in knowledgeBases"
                  :key="kb.id"
                  :value="kb.id"
                  :label="kb.name"
                />
              </t-select>
            </label>

            <div class="file-search">
              <span>文件</span>
              <div class="file-search__bar">
                <t-input v-model="fileSearchKeyword" placeholder="搜索知识库文件，留空显示最近文件" @enter="searchKnowledgeFiles" />
                <t-button variant="outline" :loading="fileSearching" @click="searchKnowledgeFiles">搜索</t-button>
              </div>
              <div v-if="fileSearchResults.length > 0" class="file-result-list">
                <button
                  v-for="file in fileSearchResults"
                  :key="file.id"
                  type="button"
                  class="file-result"
                  :class="{ selected: isKnowledgeFileSelected(file.id) }"
                  @click="toggleKnowledgeFile(file)"
                >
                  <t-icon name="file" />
                  <span>{{ file.name }}</span>
                  <small>{{ file.kbName || '知识库文件' }}</small>
                </button>
              </div>
            </div>

            <div class="capability-buttons">
              <t-button
                variant="outline"
                :class="{ 'capability-button--active': form.web_search_enabled }"
                @click="form.web_search_enabled = !form.web_search_enabled"
              >
                <template #icon><t-icon name="search" /></template>
                网络搜索
              </t-button>
              <t-button ref="skillButtonRef" variant="outline" @click="showSkillSelector = true">
                <template #icon><t-icon name="lightbulb" /></template>
                Skill
              </t-button>
              <t-button variant="outline" @click="triggerImageUpload">
                <template #icon><t-icon name="image" /></template>
                图片
              </t-button>
              <t-button variant="outline" @click="triggerAttachmentUpload">
                <template #icon><t-icon name="attachment" /></template>
                文件
              </t-button>
            </div>

            <input ref="imageInput" type="file" accept="image/*" multiple class="hidden-input" @change="handleImageFiles" />
            <input ref="attachmentInput" type="file" multiple class="hidden-input" @change="handleAttachmentFiles" />
          </div>
        </section>

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

        <Teleport to="body">
          <SkillSelector
            v-model:visible="showSkillSelector"
            v-model:selected-skill-names="selectedSkillNamesModel"
            v-model:selected-professional-skill-names="selectedProfessionalSkillNamesModel"
            :anchorEl="skillButtonRef"
            :professional-selection-mode="professionalSkillSelectionMode"
            :allowed-professional-skill-names="allowedProfessionalSkillNames"
            @close="showSkillSelector = false"
          />
        </Teleport>
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
import { computed, onMounted, reactive, ref, watch } from 'vue'
import { useRouter } from 'vue-router'
import { DialogPlugin, MessagePlugin } from 'tdesign-vue-next'
import { listAgents, type CustomAgent } from '@/api/agent'
import { listKnowledgeBases, searchKnowledge } from '@/api/knowledge-base'
import { useAuthStore } from '@/stores/auth'
import SkillSelector from '@/custom/modules/skillhub/SkillSelector.vue'
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
  type ScheduledChatRequestContext,
  type ScheduledChatRun,
  type ScheduledChatTask,
  type ScheduledChatTaskPayload,
  type ScheduledChatVariable,
  type ScheduleType,
} from './api'

type SkillSelectionMode = 'all' | 'selected' | 'none'

interface KnowledgeBaseOption {
  id: string
  name: string
  type?: 'document' | 'faq'
  embedding_model_id?: string
  summary_model_id?: string
}

interface SelectedKnowledgeFile {
  id: string
  name: string
  kbId?: string
  kbName?: string
}

const router = useRouter()
const authStore = useAuthStore()
const tasks = ref<ScheduledChatTask[]>([])
const agents = ref<CustomAgent[]>([])
const knowledgeBases = ref<KnowledgeBaseOption[]>([])
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
const showSkillSelector = ref(false)
const skillButtonRef = ref<any | null>(null)
const selectedKnowledgeFiles = ref<SelectedKnowledgeFile[]>([])
const fileSearchKeyword = ref('')
const fileSearchResults = ref<SelectedKnowledgeFile[]>([])
const fileSearching = ref(false)
const imageInput = ref<HTMLInputElement | null>(null)
const attachmentInput = ref<HTMLInputElement | null>(null)

const defaultTimezone = Intl.DateTimeFormat().resolvedOptions().timeZone || 'Asia/Shanghai'

function emptyRequestContext(): ScheduledChatRequestContext {
  return {
    knowledge_base_ids: [],
    knowledge_ids: [],
    skill_names: [],
    professional_skill_names: [],
    mentioned_items: [],
    images: [],
    attachment_uploads: [],
  }
}

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
  request_context: emptyRequestContext(),
})

const firstAgentId = computed(() => agents.value[0]?.id || '')
const selectedAgent = computed(() => agents.value.find(agent => agent.id === form.agent_id))
const professionalSkillSelectionMode = computed<SkillSelectionMode>(() =>
  selectedAgent.value?.config?.professional_skills_selection_mode || 'none',
)
const allowedProfessionalSkillNames = computed(() =>
  Array.from(new Set((selectedAgent.value?.config?.selected_professional_skills || [])
    .map(name => String(name || '').trim())
    .filter(Boolean))),
)
const selectedSkillNamesModel = computed({
  get: () => form.request_context?.skill_names || [],
  set: (names: string[]) => {
    ensureRequestContext().skill_names = normalizeNames(names)
  },
})
const selectedProfessionalSkillNamesModel = computed({
  get: () => form.request_context?.professional_skill_names || [],
  set: (names: string[]) => {
    ensureRequestContext().professional_skill_names = normalizeProfessionalSkillNames(names)
  },
})
const selectedImageCount = computed(() => form.request_context?.images?.length || 0)
const selectedAttachmentUploads = computed(() => form.request_context?.attachment_uploads || [])
const selectedKnowledgeBaseIds = computed({
  get: () => form.request_context?.knowledge_base_ids || [],
  set: (ids: string[]) => {
    ensureRequestContext().knowledge_base_ids = normalizeNames(ids)
  },
})

onMounted(loadAll)

watch(() => form.agent_id, () => {
  selectedProfessionalSkillNamesModel.value = normalizeProfessionalSkillNames(selectedProfessionalSkillNamesModel.value)
})

watch(drawerVisible, (visible) => {
  if (!visible) showSkillSelector.value = false
})

async function loadAll() {
  loading.value = true
  try {
    const [taskRes, agentRes, kbRes, variableRes, templateRes] = await Promise.all([
      listScheduledChatTasks(),
      listAgents(),
      listKnowledgeBases(),
      getScheduledChatVariables(),
      getScheduledChatPromptTemplates(),
    ])
    tasks.value = taskRes.data || []
    agents.value = agentRes.data || []
    knowledgeBases.value = Array.isArray((kbRes as any)?.data) ? (kbRes as any).data : []
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
  form.request_context = emptyRequestContext()
  selectedKnowledgeFiles.value = []
  fileSearchKeyword.value = ''
  fileSearchResults.value = []
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
  form.request_context = normalizeRequestContext(task.request_context)
  selectedKnowledgeFiles.value = filesFromRequestContext(form.request_context)
  fileSearchKeyword.value = ''
  fileSearchResults.value = []
  selectedTemplateId.value = ''
  previewContent.value = ''
  drawerVisible.value = true
}

function ensureRequestContext() {
  if (!form.request_context) form.request_context = emptyRequestContext()
  form.request_context.knowledge_base_ids ||= []
  form.request_context.knowledge_ids ||= []
  form.request_context.skill_names ||= []
  form.request_context.professional_skill_names ||= []
  form.request_context.mentioned_items ||= []
  form.request_context.images ||= []
  form.request_context.attachment_uploads ||= []
  return form.request_context
}

function normalizeRequestContext(ctx?: ScheduledChatRequestContext): ScheduledChatRequestContext {
  return {
    ...emptyRequestContext(),
    ...(ctx || {}),
    knowledge_base_ids: normalizeNames(ctx?.knowledge_base_ids || []),
    knowledge_ids: normalizeNames(ctx?.knowledge_ids || []),
    skill_names: normalizeNames(ctx?.skill_names || []),
    professional_skill_names: normalizeProfessionalSkillNames(ctx?.professional_skill_names || []),
    mentioned_items: Array.isArray(ctx?.mentioned_items) ? ctx!.mentioned_items : [],
    images: Array.isArray(ctx?.images) ? ctx!.images : [],
    attachment_uploads: Array.isArray(ctx?.attachment_uploads) ? ctx!.attachment_uploads : [],
  }
}

function buildRequestContext(): ScheduledChatRequestContext {
  const ctx = normalizeRequestContext(form.request_context)
  ctx.knowledge_ids = normalizeNames(selectedKnowledgeFiles.value.map(file => file.id))
  ctx.mentioned_items = [
    ...selectedKnowledgeBaseIds.value.map(id => {
      const kb = knowledgeBases.value.find(item => item.id === id)
      return {
        id,
        name: kb?.name || id,
        type: 'kb',
        kb_type: kb?.type || 'document',
      }
    }),
    ...selectedKnowledgeFiles.value.map(file => ({
      id: file.id,
      name: file.name,
      type: 'file',
      kb_id: file.kbId,
      kb_name: file.kbName,
    })),
    ...ctx.skill_names!.map(name => ({
      id: name,
      name,
      type: 'skill',
      skill_name: name,
    })),
  ]
  form.request_context = ctx
  return ctx
}

function normalizeNames(names: string[]) {
  const seen = new Set<string>()
  return (names || [])
    .map(name => String(name || '').trim())
    .filter(name => {
      if (!name || seen.has(name)) return false
      seen.add(name)
      return true
    })
}

function normalizeProfessionalSkillNames(names: string[]) {
  const normalized = normalizeNames(names)
  const mode = professionalSkillSelectionMode.value
  if (mode === 'none') return []
  if (mode === 'selected') {
    const allowed = new Set(allowedProfessionalSkillNames.value)
    return normalized.filter(name => allowed.has(name))
  }
  return normalized
}

function filesFromRequestContext(ctx?: ScheduledChatRequestContext): SelectedKnowledgeFile[] {
  const items = Array.isArray(ctx?.mentioned_items) ? ctx!.mentioned_items : []
  const files: SelectedKnowledgeFile[] = items
    .filter(item => item.type === 'file')
    .map(item => ({
      id: item.id,
      name: item.name || item.id,
      kbId: item.kb_id,
      kbName: item.kb_name,
    }))
  const known = new Set(files.map(file => file.id))
  for (const id of ctx?.knowledge_ids || []) {
    if (!known.has(id)) {
      files.push({ id, name: id })
    }
  }
  return files
}

async function searchKnowledgeFiles() {
  fileSearching.value = true
  try {
    const keyword = fileSearchKeyword.value.trim()
    const agentIDForSearch = sharedAgentIDForKnowledgeSearch()
    const res: any = await searchKnowledge(keyword, 0, 20, undefined, {
      agent_id: agentIDForSearch,
      recent: !keyword,
    })
    fileSearchResults.value = Array.isArray(res?.data)
      ? res.data.map((item: any) => ({
        id: item.id,
        name: item.title || item.file_name || item.id,
        kbId: item.knowledge_base_id || item.kb_id,
        kbName: item.knowledge_base_name || '',
      }))
      : []
  } catch (e: any) {
    MessagePlugin.error(e?.message || '搜索文件失败')
  } finally {
    fileSearching.value = false
  }
}

function sharedAgentIDForKnowledgeSearch() {
  const agent = selectedAgent.value
  const agentTenantID = Number(agent?.tenant_id || 0)
  const currentTenantID = Number(authStore.effectiveTenantId || authStore.currentTenantId || 0)
  if (!agent || agent.is_builtin || !agentTenantID || !currentTenantID) return undefined
  return agentTenantID !== currentTenantID ? agent.id : undefined
}

function toggleKnowledgeFile(file: SelectedKnowledgeFile) {
  const index = selectedKnowledgeFiles.value.findIndex(item => item.id === file.id)
  if (index >= 0) {
    selectedKnowledgeFiles.value.splice(index, 1)
  } else {
    selectedKnowledgeFiles.value.push(file)
  }
  ensureRequestContext().knowledge_ids = selectedKnowledgeFiles.value.map(item => item.id)
}

function removeKnowledgeFile(id: string) {
  selectedKnowledgeFiles.value = selectedKnowledgeFiles.value.filter(file => file.id !== id)
  ensureRequestContext().knowledge_ids = selectedKnowledgeFiles.value.map(file => file.id)
}

function isKnowledgeFileSelected(id: string) {
  return selectedKnowledgeFiles.value.some(file => file.id === id)
}

function removeSelectedSkill(name: string) {
  selectedSkillNamesModel.value = selectedSkillNamesModel.value.filter(item => item !== name)
}

function removeSelectedProfessionalSkill(name: string) {
  selectedProfessionalSkillNamesModel.value = selectedProfessionalSkillNamesModel.value.filter(item => item !== name)
}

function removeKnowledgeBase(id: string) {
  selectedKnowledgeBaseIds.value = selectedKnowledgeBaseIds.value.filter(item => item !== id)
}

function triggerImageUpload() {
  if (!selectedAgent.value?.config?.image_upload_enabled) {
    MessagePlugin.warning('当前智能体未启用图片上传')
    return
  }
  imageInput.value?.click()
}

function triggerAttachmentUpload() {
  attachmentInput.value?.click()
}

async function handleImageFiles(event: Event) {
  const input = event.target as HTMLInputElement
  const files = Array.from(input.files || [])
  input.value = ''
  if (files.length === 0) return
  const ctx = ensureRequestContext()
  const current = ctx.images || []
  for (const file of files) {
    if (current.length >= 5) {
      MessagePlugin.warning('最多上传 5 张图片')
      break
    }
    if (!file.type.startsWith('image/')) {
      MessagePlugin.warning(`不是图片文件：${file.name}`)
      continue
    }
    if (file.size > 10 * 1024 * 1024) {
      MessagePlugin.warning(`图片超过 10MB：${file.name}`)
      continue
    }
    current.push({ data: await readFileAsDataURL(file) })
  }
  ctx.images = current
}

async function handleAttachmentFiles(event: Event) {
  const input = event.target as HTMLInputElement
  const files = Array.from(input.files || [])
  input.value = ''
  if (files.length === 0) return
  const ctx = ensureRequestContext()
  const current = ctx.attachment_uploads || []
  for (const file of files) {
    if (current.length >= 5) {
      MessagePlugin.warning('最多上传 5 个文件')
      break
    }
    if (file.size > 20 * 1024 * 1024) {
      MessagePlugin.warning(`文件超过 20MB：${file.name}`)
      continue
    }
    const dataUrl = await readFileAsDataURL(file)
    current.push({
      data: String(dataUrl).split(',')[1] || String(dataUrl),
      file_name: file.name,
      file_size: file.size,
    })
  }
  ctx.attachment_uploads = current
}

function removeImage(index: number) {
  ensureRequestContext().images!.splice(index, 1)
}

function removeAttachment(index: number) {
  ensureRequestContext().attachment_uploads!.splice(index, 1)
}

function readFileAsDataURL(file: File): Promise<string> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader()
    reader.onload = () => resolve(String(reader.result || ''))
    reader.onerror = reject
    reader.readAsDataURL(file)
  })
}

function formatFileSize(bytes: number) {
  if (bytes < 1024) return `${bytes} B`
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`
  return `${(bytes / 1024 / 1024).toFixed(1)} MB`
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
    const payload = {
      ...form,
      request_context: buildRequestContext(),
    }
    if (editingTask.value) {
      await updateScheduledChatTask(editingTask.value.id, payload)
    } else {
      await createScheduledChatTask(payload)
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
      request_context: normalizeRequestContext(task.request_context),
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
      request_context: buildRequestContext(),
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

.capability-panel {
  border: 1px solid var(--td-component-border);
  background: var(--td-bg-color-secondarycontainer);
  padding: 12px;
}

.capability-panel__header {
  display: flex;
  align-items: baseline;
  gap: 8px;
  margin-bottom: 10px;
  font-weight: 600;
}

.capability-panel__header small {
  color: var(--td-text-color-secondary);
  font-weight: 400;
}

.selected-tags-inline {
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
  margin-bottom: 12px;
}

.mention-chip {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  max-width: 220px;
  min-height: 28px;
  padding: 4px 8px;
  border: 1px solid var(--td-component-border);
  border-radius: 6px;
  background: var(--td-bg-color-container);
  font-size: 13px;
}

.mention-chip__icon {
  display: inline-flex;
  color: var(--td-brand-color);
}

.mention-chip__name {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.mention-chip__remove {
  color: var(--td-text-color-secondary);
  cursor: pointer;
}

.mention-chip__remove:hover {
  color: var(--td-error-color);
}

.mention-chip--professional-skill .mention-chip__icon,
.mention-chip--attachment .mention-chip__icon {
  color: var(--td-warning-color);
}

.mention-chip--file .mention-chip__icon,
.mention-chip--image .mention-chip__icon {
  color: var(--td-success-color);
}

.mention-chip--web .mention-chip__icon {
  color: var(--td-brand-color);
}

.capability-actions {
  display: flex;
  flex-direction: column;
  gap: 12px;
}

.capability-select,
.file-search {
  display: flex;
  flex-direction: column;
  gap: 8px;
  font-size: 13px;
  font-weight: 600;
}

.file-search__bar,
.capability-buttons {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}

.capability-button--active {
  border-color: var(--td-brand-color) !important;
  color: var(--td-brand-color) !important;
  background: var(--td-brand-color-light) !important;
}

.file-search__bar > :first-child {
  flex: 1;
}

.file-result-list {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: 8px;
}

.file-result {
  display: grid;
  grid-template-columns: auto minmax(0, 1fr);
  grid-template-rows: auto auto;
  column-gap: 8px;
  row-gap: 2px;
  align-items: center;
  border: 1px solid var(--td-component-border);
  border-radius: 6px;
  background: var(--td-bg-color-container);
  color: var(--td-text-color-primary);
  padding: 8px;
  text-align: left;
  cursor: pointer;
}

.file-result.selected {
  border-color: var(--td-brand-color);
  background: var(--td-brand-color-light);
}

.file-result span,
.file-result small {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.file-result small {
  grid-column: 2;
  color: var(--td-text-color-secondary);
}

.hidden-input {
  display: none;
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
