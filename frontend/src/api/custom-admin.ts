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
  scope_organization_external_id?: string
  scope_organization_name?: string
  org_count: number
  user_count: number
  created_users: number
  updated_users: number
  disabled_users: number
  started_at: string
  finished_at?: string
  progress?: IAMSyncRunProgress
}

export interface IAMSyncRunProgress {
  org_count: number
  user_count: number
  created_users: number
  updated_users: number
  disabled_users: number
  last_activity_at?: string
}

export interface IAMOrganizationNode {
  external_id: string
  name: string
  parent_external_id?: string
  has_children?: boolean
  user_count: number
  tenant_count: number
  node_type?: 'organization' | 'user'
  iam_external_id?: string
  username?: string
  display_name?: string
  has_local_user?: boolean
  access_enabled?: boolean
}

export interface AdminSpaceSummary {
  kind: 'tenant' | 'organization'
  id: string
  tenant_id?: number
  name: string
  description?: string
  status?: string
  owner_user_id?: string
  owner_username?: string
  owner_tenant_id?: number
  member_count: number
  created_at: string
  updated_at: string
}

export interface AdminManagedUser {
  id: string
  username: string
  display_name?: string
  tenant_id: number
  tenant_name?: string
  is_active: boolean
  is_system_admin: boolean
  has_local_user?: boolean
  access_enabled?: boolean
  iam_external_id?: string
  iam_username?: string
  iam_display_name?: string
  iam_organization_external_id?: string
  iam_organization_name?: string
  created_at: string
  updated_at: string
}

export interface AdminBulkUserActiveResult {
  active: boolean
  matched_users: number
  updated_local_users: number
  updated_iam_users: number
  revoked_tokens: number
  skipped_self: number
  skipped_system_admins: number
}

export interface AdminCreatedLocalAccount {
  user: AdminManagedUser
  temporary_password: string
  warnings?: string[]
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

export function runIAMSync(params?: { iam_organization_external_id?: string }): Promise<{ data: IAMSyncRun; message?: string; success: boolean }> {
  return post('/api/v1/custom/iam/sync', params || {}) as unknown as Promise<{ data: IAMSyncRun; message?: string; success: boolean }>
}

export function listIAMSyncRuns(): Promise<{ data: IAMSyncRun[] }> {
  return get('/api/v1/custom/iam/sync-runs')
}

export function listIAMOrganizations(params: {
  parentId?: string
  query?: string
  limit?: number
  includeUsers?: boolean
} = {}): Promise<{ success: boolean; data: IAMOrganizationNode[]; message?: string }> {
  const search = new URLSearchParams()
  if (typeof params.parentId === 'string') search.set('parent_id', params.parentId)
  if (params.query) search.set('q', params.query)
  if (params.includeUsers) search.set('include_users', 'true')
  search.set('limit', String(params.limit ?? 100))
  return get(`/api/v1/custom/iam/organizations?${search.toString()}`) as unknown as Promise<{ success: boolean; data: IAMOrganizationNode[]; message?: string }>
}

export function searchAdminSpaces(params: {
  query: string
  limit?: number
}): Promise<{ success: boolean; data: AdminSpaceSummary[]; message?: string }> {
  const search = new URLSearchParams()
  search.set('q', params.query)
  search.set('limit', String(params.limit ?? 30))
  return get(`/api/v1/custom/admin/spaces?${search.toString()}`) as unknown as Promise<{ success: boolean; data: AdminSpaceSummary[]; message?: string }>
}

export function searchAdminUsers(params: {
  query?: string
  iamOrgIds?: string[]
  iamExternalIds?: string[]
  direct?: boolean
  limit?: number
}): Promise<{ success: boolean; data: AdminManagedUser[]; message?: string }> {
  const search = new URLSearchParams()
  if (params.query) search.set('q', params.query)
  if (params.iamOrgIds?.length) search.set('iam_org_ids', params.iamOrgIds.join(','))
  if (params.iamExternalIds?.length) search.set('iam_external_ids', params.iamExternalIds.join(','))
  if (params.direct) search.set('direct', 'true')
  search.set('limit', String(params.limit ?? 100))
  return get(`/api/v1/custom/admin/users?${search.toString()}`) as unknown as Promise<{ success: boolean; data: AdminManagedUser[]; message?: string }>
}

export function createAdminLocalAccount(input: {
  username: string
  display_name?: string
}): Promise<{ success: boolean; data?: AdminCreatedLocalAccount; message?: string }> {
  return post('/api/v1/custom/admin/users', input) as unknown as Promise<{
    success: boolean
    data?: AdminCreatedLocalAccount
    message?: string
  }>
}

export function setAdminUserActive(userId: string, active: boolean): Promise<{ success: boolean; data: AdminManagedUser; message?: string }> {
  return put(`/api/v1/custom/admin/users/${encodeURIComponent(userId)}/active`, { active }) as unknown as Promise<{ success: boolean; data: AdminManagedUser; message?: string }>
}

export function batchSetAdminUsersActive(params: {
  active: boolean
  query?: string
  iamOrgIds?: string[]
  iamExternalIds?: string[]
  direct?: boolean
}): Promise<{ success: boolean; data: AdminBulkUserActiveResult; message?: string }> {
  return put('/api/v1/custom/admin/users-active', {
    active: params.active,
    query: params.query || '',
    iam_org_ids: params.iamOrgIds || [],
    iam_external_ids: params.iamExternalIds || [],
    direct: params.direct === true,
  }) as unknown as Promise<{ success: boolean; data: AdminBulkUserActiveResult; message?: string }>
}
