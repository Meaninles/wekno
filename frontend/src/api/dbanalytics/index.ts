import { get, post, put, del } from '@/utils/request';

export type DatabaseSourceType = 'mysql' | 'postgres';
export type DatabaseSourceSharePermission = 'viewer' | 'editor';
export type OrganizationRole = 'admin' | DatabaseSourceSharePermission;

export interface DatabaseSourceConfig {
  host: string;
  port: number;
  database: string;
  username: string;
  password?: string;
  ssl_mode?: string;
  params?: Record<string, string>;
}

export interface DatabaseSource {
  id: string;
  tenant_id: number;
  name: string;
  description?: string;
  type: DatabaseSourceType;
  status: 'active' | 'error';
  error_message?: string;
  query_mode: 'live' | 'snapshot';
  max_rows: number;
  max_scan_rows: number;
  timeout_seconds: number;
  created_by?: string;
  created_at?: string;
  updated_at?: string;
  config?: DatabaseSourceConfig;
  shared?: boolean;
  share_id?: string;
  organization_id?: string;
  org_name?: string;
  permission?: DatabaseSourceSharePermission;
  source_tenant_id?: number;
  is_mine?: boolean;
  source_from_agent?: DatabaseSourceFromAgent;
}

export interface DatabaseSourceFromAgent {
  agent_id: string;
  agent_name: string;
}

export interface DatabaseSourceShare {
  id: string;
  source_id: string;
  source_name?: string;
  source_type?: string;
  organization_id: string;
  organization_name?: string;
  shared_by_user_id: string;
  shared_by_username?: string;
  source_tenant_id: number;
  permission: DatabaseSourceSharePermission;
  my_role_in_org?: OrganizationRole;
  my_permission?: DatabaseSourceSharePermission;
  created_at: string;
  require_approval?: boolean;
}

export interface SharedDatabaseSource {
  source: DatabaseSource;
  share_id: string;
  organization_id: string;
  org_name: string;
  permission: DatabaseSourceSharePermission;
  source_tenant_id: number;
  shared_at: string;
}

export interface OrganizationSharedDatabaseSourceItem extends SharedDatabaseSource {
  is_mine: boolean;
  source_from_agent?: DatabaseSourceFromAgent;
}

export interface DatabaseColumn {
  id: string;
  table_id: string;
  source_id: string;
  column_name: string;
  data_type: string;
  nullable: boolean;
  ordinal: number;
  description?: string;
  sample_values?: string[];
  semantic_type?: string;
  sensitive_level?: 'none' | 'masked' | 'hidden';
}

export interface DatabaseTable {
  id: string;
  source_id: string;
  schema_name: string;
  table_name: string;
  object_type: 'table' | 'view';
  virtual_name: string;
  enabled: boolean;
  row_estimate?: number;
  description?: string;
  columns?: DatabaseColumn[];
}

export interface DatabaseSourceDetailResponse {
  success: boolean;
  data: DatabaseSource;
  tables: DatabaseTable[];
}

export interface CreateDatabaseSourceRequest {
  name: string;
  description?: string;
  type: DatabaseSourceType;
  config: DatabaseSourceConfig;
  query_mode?: 'live' | 'snapshot';
  max_rows?: number;
  max_scan_rows?: number;
  timeout_seconds?: number;
}

export type UpdateDatabaseSourceRequest = Partial<CreateDatabaseSourceRequest>;

const base = '/api/v1/custom/db-analytics';

export function listDatabaseSources() {
  return get(`${base}/sources`) as unknown as Promise<{ success: boolean; data: DatabaseSource[]; total: number }>;
}

export function listSharedDatabaseSources() {
  return get(`${base}/shared-sources`) as unknown as Promise<{ success: boolean; data: SharedDatabaseSource[]; total: number }>;
}

export function listOrganizationSharedDatabaseSources(orgId: string) {
  return get(`${base}/organizations/${orgId}/shared-sources`) as unknown as Promise<{ success: boolean; data: OrganizationSharedDatabaseSourceItem[]; total: number }>;
}

export function createDatabaseSource(data: CreateDatabaseSourceRequest) {
  return post(`${base}/sources`, data) as unknown as Promise<{ success: boolean; data: DatabaseSource }>;
}

export function testDatabaseSourceConfig(data: CreateDatabaseSourceRequest) {
  return post(`${base}/source-test`, data) as unknown as Promise<{ success: boolean }>;
}

export function getDatabaseSource(id: string) {
  return get(`${base}/sources/${id}`) as unknown as Promise<DatabaseSourceDetailResponse>;
}

export function updateDatabaseSource(id: string, data: UpdateDatabaseSourceRequest) {
  return put(`${base}/sources/${id}`, data) as unknown as Promise<{ success: boolean; data: DatabaseSource }>;
}

export function deleteDatabaseSource(id: string) {
  return del(`${base}/sources/${id}`) as unknown as Promise<{ success: boolean }>;
}

export function testDatabaseSource(id: string) {
  return post(`${base}/sources/${id}/test`) as unknown as Promise<{ success: boolean }>;
}

export function refreshDatabaseMetadata(id: string) {
  return post(`${base}/sources/${id}/refresh-metadata`) as unknown as Promise<{ success: boolean }>;
}

export function setDatabaseTableScope(sourceId: string, tableIds: string[]) {
  return put(`${base}/sources/${sourceId}/tables/scope`, { table_ids: tableIds }) as unknown as Promise<{ success: boolean }>;
}

export function updateDatabaseColumn(columnId: string, data: { description?: string; semantic_type?: string; sensitive_level?: string }) {
  return put(`${base}/columns/${columnId}`, data) as unknown as Promise<{ success: boolean; data: DatabaseColumn }>;
}

export function shareDatabaseSource(sourceId: string, data: { organization_id: string; permission: DatabaseSourceSharePermission }) {
  return post(`${base}/sources/${sourceId}/shares`, data) as unknown as Promise<{ success: boolean; data: DatabaseSourceShare }>;
}

export function listDatabaseSourceShares(sourceId: string) {
  return get(`${base}/sources/${sourceId}/shares`) as unknown as Promise<{ success: boolean; data: { shares: DatabaseSourceShare[]; total: number } }>;
}

export function updateDatabaseSourceSharePermission(sourceId: string, shareId: string, data: { permission: DatabaseSourceSharePermission }) {
  return put(`${base}/sources/${sourceId}/shares/${shareId}`, data) as unknown as Promise<{ success: boolean }>;
}

export function removeDatabaseSourceShare(sourceId: string, shareId: string) {
  return del(`${base}/sources/${sourceId}/shares/${shareId}`) as unknown as Promise<{ success: boolean }>;
}
