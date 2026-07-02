import { del, get, getDown, post, postUpload, put } from "../../utils/request";

// Skill信息
export interface SkillInfo {
  name: string;
  description: string;
  kind?: 'lightweight' | 'professional';
}

export type SkillPermission = 'admin' | 'editor' | 'viewer';
export type SkillShareType = 'organization' | 'user';

export interface ManagedSkill {
  id: string;
  tenant_id: number;
  creator_id: string;
  name: string;
  description: string;
  instructions: string;
  enabled: boolean;
  created_at: string;
  updated_at: string;
  is_mine: boolean;
  share_id?: string;
  share_type?: SkillShareType;
  organization_id?: string;
  organization_name?: string;
  target_user_id?: string;
  target_username?: string;
  shared_by_user_id?: string;
  shared_by_username?: string;
  source_tenant_id?: number;
  permission?: SkillPermission;
  shared_at?: string;
}

export interface ManagedProfessionalSkill {
  id?: string;
  name: string;
  description: string;
  kind: 'professional';
  file_count: number;
  managed?: boolean;
  is_mine?: boolean;
  can_manage?: boolean;
  can_download?: boolean;
  system_reserved?: boolean;
  archive_file_name?: string;
  share_id?: string;
  share_type?: SkillShareType;
  organization_id?: string;
  organization_name?: string;
  target_user_id?: string;
  target_username?: string;
  shared_by_user_id?: string;
  shared_by_username?: string;
  source_tenant_id?: number;
  permission?: SkillPermission;
  shared_at?: string;
  updated_at?: string;
}

export interface SkillPayload {
  name: string;
  description: string;
  instructions: string;
  enabled?: boolean;
}

export interface SkillShareList {
  organization_shares?: ManagedSkill[];
  user_shares?: ManagedSkill[];
}

export interface SkillShareUser {
  id: string;
  username: string;
  avatar?: string;
  tenant_id: number;
}

// 获取预装Skills列表；skills_available 为 false 表示沙箱未启用，前端应隐藏/禁用 Skills 配置
export function listSkills() {
  return get('/api/v1/skills') as Promise<{
    data: SkillInfo[];
    professional_data?: SkillInfo[];
    skills_available?: boolean;
    professional_skills_available?: boolean;
  }>;
}

export function listManagedSkills() {
  return get('/api/v1/custom/skills') as unknown as Promise<{ success: boolean; data: ManagedSkill[]; total?: number }>;
}

export function listManagedProfessionalSkills() {
  return get('/api/v1/custom/skills/professional') as unknown as Promise<{ success: boolean; data: ManagedProfessionalSkill[]; total?: number }>;
}

export function importProfessionalSkill(payload: { name: string; description?: string; package: File }) {
  const form = new FormData();
  form.append('name', payload.name);
  form.append('description', payload.description || '');
  form.append('package', payload.package);
  return postUpload('/api/v1/custom/skills/professional', form) as unknown as Promise<{ success: boolean; data: ManagedProfessionalSkill; message?: string }>;
}

export function updateProfessionalSkill(id: string, payload: { name: string; description?: string; package?: File | null }) {
  const form = new FormData();
  form.append('name', payload.name);
  form.append('description', payload.description || '');
  if (payload.package) form.append('package', payload.package);
  return put(`/api/v1/custom/skills/professional/${id}`, form, {
    headers: { 'Content-Type': 'multipart/form-data' },
  }) as unknown as Promise<{ success: boolean; data: ManagedProfessionalSkill; message?: string }>;
}

export function deleteProfessionalSkill(id: string) {
  return del(`/api/v1/custom/skills/professional/${id}`) as unknown as Promise<{ success: boolean; message?: string }>;
}

export function downloadProfessionalSkill(id: string) {
  return getDown(`/api/v1/custom/skills/professional/${id}/download`);
}

export function listProfessionalSkillShares(id: string) {
  return get(`/api/v1/custom/skills/professional/${id}/shares`) as unknown as Promise<{ success: boolean; data: { organization_shares?: ManagedProfessionalSkill[]; user_shares?: ManagedProfessionalSkill[] }; message?: string }>;
}

export function shareProfessionalSkillToOrganization(id: string, payload: { organization_id: string; permission: SkillPermission }) {
  return post(
    `/api/v1/custom/skills/professional/${id}/shares/organizations`,
    payload,
  ) as unknown as Promise<{ success: boolean; data: ManagedProfessionalSkill; message?: string }>;
}

export function shareProfessionalSkillToUser(id: string, payload: { user_id: string; permission: SkillPermission }) {
  return post(
    `/api/v1/custom/skills/professional/${id}/shares/users`,
    payload,
  ) as unknown as Promise<{ success: boolean; data: ManagedProfessionalSkill; message?: string }>;
}

export function removeProfessionalSkillOrganizationShare(id: string, shareId: string) {
  return del(`/api/v1/custom/skills/professional/${id}/shares/organizations/${shareId}`) as unknown as Promise<{ success: boolean; message?: string }>;
}

export function removeProfessionalSkillUserShare(id: string, shareId: string) {
  return del(`/api/v1/custom/skills/professional/${id}/shares/users/${shareId}`) as unknown as Promise<{ success: boolean; message?: string }>;
}

export function listSkillsByOrganization(organizationId: string) {
  return get(
    `/api/v1/custom/skills/organizations/${organizationId}`,
  ) as unknown as Promise<{ success: boolean; data: ManagedSkill[]; total?: number }>;
}

export function createManagedSkill(payload: SkillPayload) {
  return post('/api/v1/custom/skills', payload) as unknown as Promise<{ success: boolean; data: ManagedSkill; message?: string }>;
}

export function updateManagedSkill(id: string, payload: SkillPayload) {
  return put(`/api/v1/custom/skills/${id}`, payload) as unknown as Promise<{ success: boolean; data: ManagedSkill; message?: string }>;
}

export function deleteManagedSkill(id: string) {
  return del(`/api/v1/custom/skills/${id}`) as unknown as Promise<{ success: boolean; message?: string }>;
}

export function listSkillShares(id: string) {
  return get(`/api/v1/custom/skills/${id}/shares`) as unknown as Promise<{ success: boolean; data: SkillShareList; message?: string }>;
}

export function shareSkillToOrganization(id: string, payload: { organization_id: string; permission: SkillPermission }) {
  return post(
    `/api/v1/custom/skills/${id}/shares/organizations`,
    payload,
  ) as unknown as Promise<{ success: boolean; data: ManagedSkill; message?: string }>;
}

export function shareSkillToUser(id: string, payload: { user_id: string; permission: SkillPermission }) {
  return post(
    `/api/v1/custom/skills/${id}/shares/users`,
    payload,
  ) as unknown as Promise<{ success: boolean; data: ManagedSkill; message?: string }>;
}

export function removeSkillOrganizationShare(id: string, shareId: string) {
  return del(`/api/v1/custom/skills/${id}/shares/organizations/${shareId}`) as unknown as Promise<{ success: boolean; message?: string }>;
}

export function removeSkillUserShare(id: string, shareId: string) {
  return del(`/api/v1/custom/skills/${id}/shares/users/${shareId}`) as unknown as Promise<{ success: boolean; message?: string }>;
}

export function searchSkillShareUsers(query: string, limit = 20) {
  const params = new URLSearchParams();
  if (query) params.set('q', query);
  params.set('limit', String(limit));
  return get(
    `/api/v1/custom/skills/users?${params.toString()}`,
  ) as unknown as Promise<{ success: boolean; data: SkillShareUser[]; message?: string }>;
}
