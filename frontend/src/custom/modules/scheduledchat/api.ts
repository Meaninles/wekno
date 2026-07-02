import { get, post, put, del } from '@/utils/request'

export type ScheduleType = 'hourly' | 'daily' | 'weekly' | 'monthly'
export type RunStatus = 'running' | 'success' | 'failed' | 'skipped'

export interface ScheduledChatTask {
  id: string
  tenant_id: number
  created_by: string
  run_as_user_id: string
  name: string
  description?: string
  enabled: boolean
  agent_id: string
  agent_name_snapshot?: string
  schedule_type: ScheduleType
  timezone: string
  minute: number
  hour: number
  weekday: number
  day_of_month: number
  prompt_template: string
  web_search_enabled: boolean
  next_run_at?: string
  last_run_at?: string
  last_success_at?: string
  last_status?: RunStatus
  last_message?: string
  last_session_id?: string
  created_at?: string
  updated_at?: string
}

export interface ScheduledChatRun {
  id: string
  task_id: string
  scheduled_at: string
  triggered_by: 'schedule' | 'manual'
  status: RunStatus
  session_id?: string
  user_message_id?: string
  assistant_message_id?: string
  rendered_prompt?: string
  error_message?: string
  started_at?: string
  finished_at?: string
  created_at?: string
}

export interface ScheduledChatTaskPayload {
  name: string
  description?: string
  enabled: boolean
  agent_id: string
  schedule_type: ScheduleType
  timezone: string
  minute: number
  hour: number
  weekday: number
  day_of_month: number
  prompt_template: string
  web_search_enabled: boolean
}

export interface ScheduledChatVariable {
  name: string
  label: string
  description: string
}

export interface ScheduledChatPromptTemplate {
  id: string
  name: string
  description: string
  content: string
}

const base = '/api/v1/custom/scheduled-chat'

export function listScheduledChatTasks() {
  return get(`${base}/tasks`) as unknown as Promise<{ success: boolean; data: ScheduledChatTask[] }>
}

export function createScheduledChatTask(payload: ScheduledChatTaskPayload) {
  return post(`${base}/tasks`, payload) as unknown as Promise<{ success: boolean; data: ScheduledChatTask }>
}

export function updateScheduledChatTask(id: string, payload: ScheduledChatTaskPayload) {
  return put(`${base}/tasks/${id}`, payload) as unknown as Promise<{ success: boolean; data: ScheduledChatTask }>
}

export function deleteScheduledChatTask(id: string) {
  return del(`${base}/tasks/${id}`) as unknown as Promise<{ success: boolean }>
}

export function runScheduledChatTaskNow(id: string) {
  return post(`${base}/tasks/${id}/run-now`) as unknown as Promise<{ success: boolean; data: ScheduledChatRun }>
}

export function listScheduledChatRuns(id: string, limit = 20) {
  return get(`${base}/tasks/${id}/runs?limit=${limit}`) as unknown as Promise<{ success: boolean; data: ScheduledChatRun[] }>
}

export function getScheduledChatVariables() {
  return get(`${base}/variables`) as unknown as Promise<{ success: boolean; data: ScheduledChatVariable[] }>
}

export function getScheduledChatPromptTemplates() {
  return get(`${base}/prompt-templates`) as unknown as Promise<{ success: boolean; data: ScheduledChatPromptTemplate[] }>
}

export function renderScheduledChatPreview(payload: {
  prompt_template: string
  task_name?: string
  agent_id?: string
  timezone?: string
}) {
  return post(`${base}/render-preview`, payload) as unknown as Promise<{ success: boolean; data: { content: string } }>
}
