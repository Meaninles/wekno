import { get, post, put } from '@/utils/request'

type ApiResponse<T> = {
  success: boolean
  data: T
  message?: string
}

export type AdmissionPoolState = 'enabled' | 'draining' | 'disabled'

export interface ModelResourcePool {
  id: string
  name: string
  resource_kind: string
  chat_max_concurrent: number | null
  chat_max_waiting: number | null
  max_inflight: number
  max_background_inflight: number
  interactive_reserve: number
  tenant_guaranteed: number
  tenant_burst: number
  document_guaranteed: number
  document_burst: number
  rpm: number
  tpm: number
  token_burst: number
  request_timeout_seconds: number
  circuit_threshold: number
  circuit_window_seconds: number
  circuit_open_seconds: number
  state: AdmissionPoolState
  policy_version: number
  created_at: string
  updated_at: string
}

export interface ModelResourceBinding {
  model_id: string
  model_tenant_id: number
  resource_pool_id: string
  quota_pool_id: string
  gateway_pool_id: string
  route_fingerprint: string
  binding_version: number
}

export interface ModelAdmissionTemplate {
  kind: string
  max_inflight: number
  max_background_inflight: number
  interactive_reserve: number
  tenant_burst: number
  document_burst: number
  rpm: number
  tpm: number
  request_timeout_seconds: number
  circuit_threshold: number
  circuit_window_seconds: number
  circuit_open_seconds: number
  policy_version: number
}

export interface ModelQuotaPool {
  id: string
  name: string
  rpm: number
  tpm: number
  token_burst: number
  state: AdmissionPoolState
  policy_version: number
}

export interface ModelGatewayPool {
  id: string
  name: string
  max_inflight: number
  state: AdmissionPoolState
  policy_version: number
}

export interface ModelAdmissionAudit {
  id: number
  actor_id: string
  action: string
  resource_type: string
  resource_id: string
  policy_version: number
  created_at: string
}

export interface CapacityIssue {
  severity: 'error' | 'warning' | 'info'
  code: string
  scope: string
  message: string
}

export interface CapacityModuleGrant {
  module: string
  requested: number
  effective: number
  wait_mode: string
}

export interface CapacityPoolReport {
  id: string
  name: string
  resource_kind: string
  state: AdmissionPoolState
  policy_version: number
  binding_count: number
  route_count: number
  configured: {
    max_inflight: number
    interactive_reserve: number
    tenant_burst: number
    document_burst: number
    rpm: number
    tpm: number
    chat_max_waiting: number | null
  }
  effective: {
    provider_total: number
    interactive_reserved: number
    background_max: number
    tenant_max: number
    document_max: number
    chat_sessions: number
  }
  module_grants: CapacityModuleGrant[]
  issues: CapacityIssue[]
  quota_pool_ids: string[]
  gateway_pool_ids: string[]
}

export interface CapacityEffectiveReport {
  generated_at: string
  healthy: boolean
  source_of_truth: string
  summary: {
    pools: number
    bindings: number
    errors: number
    warnings: number
    information: number
  }
  runtime: {
    scheduler: string
    background_wait_mode: string
    capacity_wait_counts_as_failure: boolean
    background_workers_per_instance: number
    wiki_workers_per_instance: number
    derivative_replicas: number
    wiki_replicas: number
    background_consumer_slots: number
    wiki_consumer_slots: number
    admission: Record<string, number>
  }
  pools: CapacityPoolReport[]
  issues: CapacityIssue[]
}

export interface CapacityValidationResult {
  valid: boolean
  canonical?: ModelResourcePool
  report?: CapacityPoolReport
  issues: CapacityIssue[]
}

const base = '/api/v1/custom/capacity-control'

export async function getCapacityEffectiveReport(): Promise<CapacityEffectiveReport> {
  const response = await get(`${base}/effective`) as unknown as ApiResponse<CapacityEffectiveReport>
  return response.data
}

export async function validateCapacityPool(pool: ModelResourcePool): Promise<CapacityValidationResult> {
  const response = await post(`${base}/validate`, pool) as unknown as ApiResponse<CapacityValidationResult>
  return response.data
}

export async function listModelResourcePools(): Promise<ModelResourcePool[]> {
  const response = await get(`${base}/resource-pools`) as unknown as ApiResponse<ModelResourcePool[]>
  return response.data
}

export async function updateModelResourcePool(pool: ModelResourcePool): Promise<ModelResourcePool> {
  const response = await put(
    `${base}/resource-pools/${encodeURIComponent(pool.id)}`,
    pool,
    { headers: { 'If-Match': String(pool.policy_version) } },
  ) as unknown as ApiResponse<ModelResourcePool>
  return response.data
}

export async function drainModelResourcePool(pool: ModelResourcePool): Promise<void> {
  await post(
    `${base}/resource-pools/${encodeURIComponent(pool.id)}/drain`,
    {},
    { headers: { 'If-Match': String(pool.policy_version) } },
  )
}

export async function resetModelResourcePool(pool: ModelResourcePool): Promise<ModelResourcePool> {
  const response = await post(
    `${base}/resource-pools/${encodeURIComponent(pool.id)}/reset`,
    {},
    { headers: { 'If-Match': String(pool.policy_version) } },
  ) as unknown as ApiResponse<ModelResourcePool>
  return response.data
}

export async function listModelResourceBindings(): Promise<ModelResourceBinding[]> {
  const response = await get(`${base}/bindings`) as unknown as ApiResponse<ModelResourceBinding[]>
  return response.data
}

export async function updateModelResourceBinding(binding: ModelResourceBinding): Promise<ModelResourceBinding> {
  const response = await put(
    `${base}/bindings/${encodeURIComponent(binding.model_id)}`,
    {
      model_tenant_id: binding.model_tenant_id,
      resource_pool_id: binding.resource_pool_id,
      quota_pool_id: binding.quota_pool_id || '',
      gateway_pool_id: binding.gateway_pool_id || '',
    },
    { headers: { 'If-Match': String(binding.binding_version) } },
  ) as unknown as ApiResponse<ModelResourceBinding>
  return response.data
}

export async function listModelAdmissionTemplates(): Promise<ModelAdmissionTemplate[]> {
  const response = await get(`${base}/templates`) as unknown as ApiResponse<ModelAdmissionTemplate[]>
  return response.data
}

export async function updateModelAdmissionTemplate(template: ModelAdmissionTemplate): Promise<ModelAdmissionTemplate> {
  template.max_background_inflight = Math.max(1, template.max_inflight - template.interactive_reserve)
  const response = await put(
    `${base}/templates/${encodeURIComponent(template.kind)}`,
    template,
    { headers: { 'If-Match': String(template.policy_version) } },
  ) as unknown as ApiResponse<ModelAdmissionTemplate>
  return response.data
}

export async function listModelQuotaPools(): Promise<ModelQuotaPool[]> {
  const response = await get(`${base}/quota-pools`) as unknown as ApiResponse<ModelQuotaPool[]>
  return response.data
}

export async function listModelGatewayPools(): Promise<ModelGatewayPool[]> {
  const response = await get(`${base}/gateway-pools`) as unknown as ApiResponse<ModelGatewayPool[]>
  return response.data
}

export async function listModelAdmissionAudits(): Promise<ModelAdmissionAudit[]> {
  const response = await get(`${base}/audits`) as unknown as ApiResponse<ModelAdmissionAudit[]>
  return response.data
}

export async function reconcileModelResourcePools(): Promise<void> {
  await post(`${base}/reconcile`, {})
}
