import { get, post, put } from '@/utils/request'

export interface ResourceRef {
  resource_type: string
  source_tenant_id: number
  source_resource_id: string
}

export interface ResourceSummary {
  resource_type: string
  id: string
  source_tenant_id: number
  config_key?: string
  name: string
  description?: string
  kind?: string
  enabled: boolean
}

export interface CustomUserSummary {
  id: string
  username: string
  tenant_id: number
  active: boolean
}

export interface ApplyResult {
  users_applied: number
  resources: number
  errors?: string[]
}

export interface IAMSyncSetting {
  id?: number
  enabled: boolean
  base_url: string
  login_client_id?: string
  login_client_secret?: string
  sync_client_id: string
  sync_client_secret?: string
  schedule_mode: 'daily' | 'weekly'
  weekdays: string
  run_at: string
  last_run_at?: string
  last_status?: string
  last_message?: string
  last_run_triggered_by?: string
}

export interface IAMSyncRun {
  id: string
  triggered_by: string
  status: string
  message?: string
  org_count: number
  user_count: number
  created_users: number
  updated_users: number
  disabled_users: number
  started_at: string
  finished_at?: string
}

export function listCustomUsers(): Promise<{ data: CustomUserSummary[] }> {
  return get('/api/v1/custom/config-center/users')
}

export function listConfigResources(sourceUserId: string): Promise<{ data: ResourceSummary[] }> {
  return get(`/api/v1/custom/config-center/resources?source_user_id=${encodeURIComponent(sourceUserId)}`)
}

export function getDefaultGrants(): Promise<{ data: ResourceRef[] }> {
  return get('/api/v1/custom/config-center/defaults')
}

export function saveDefaultGrants(grants: ResourceRef[]): Promise<{ success: boolean }> {
  return put('/api/v1/custom/config-center/defaults', { grants }) as unknown as Promise<{ success: boolean }>
}

export function getUserGrants(userId: string): Promise<{ data: ResourceRef[] }> {
  return get(`/api/v1/custom/config-center/users/${userId}/grants`)
}

export function saveUserGrants(userId: string, grants: ResourceRef[]): Promise<{ success: boolean }> {
  return put(`/api/v1/custom/config-center/users/${userId}/grants`, { grants }) as unknown as Promise<{ success: boolean }>
}

export function applyDefaultConfig(): Promise<{ data: ApplyResult }> {
  return post('/api/v1/custom/config-center/apply', {})
}

export function applyUserConfig(userId: string): Promise<{ data: ApplyResult }> {
  return post(`/api/v1/custom/config-center/users/${userId}/apply`, {})
}

export function getIAMSyncSetting(): Promise<{ data: IAMSyncSetting }> {
  return get('/api/v1/custom/iam/settings')
}

export function saveIAMSyncSetting(setting: IAMSyncSetting): Promise<{ data: IAMSyncSetting }> {
  return put('/api/v1/custom/iam/settings', setting)
}

export function runIAMSync(): Promise<{ data: IAMSyncRun; message?: string; success: boolean }> {
  return post('/api/v1/custom/iam/sync', {}) as unknown as Promise<{ data: IAMSyncRun; message?: string; success: boolean }>
}

export function listIAMSyncRuns(): Promise<{ data: IAMSyncRun[] }> {
  return get('/api/v1/custom/iam/sync-runs')
}
