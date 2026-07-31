import { del, get, post, put } from '@/utils/request'
import type { ModelConfig } from '@/api/model'

export interface DerivativeModel extends ModelConfig {
  id: string
  tenant_id: number
  workload_scope: 'derivative_only'
  is_derivative_default: boolean
}

export interface DerivativeLimiterSnapshot {
  tpm: number
  mode: 'redis' | 'local'
  active: boolean
  acquired: number
  deferred: number
}

export interface DerivativeStatus {
  configured: boolean
  default_model_id: string
  tpm: number
  models: DerivativeModel[]
  limiter: DerivativeLimiterSnapshot
}

export interface DerivativeCandidate extends ModelConfig {
  id: string
  tenant_id: number
  assigned: boolean
  eligible: boolean
  reason?: string
  is_derivative_default: boolean
}

export interface DerivativeAdminConfig extends DerivativeStatus {
  candidates: DerivativeCandidate[]
}

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

export async function listModelResourcePools(): Promise<ModelResourcePool[]> {
  const response = await get('/api/v1/custom/derivative-control/resource-pools') as unknown as ApiResponse<ModelResourcePool[]>
  return response.data
}

export async function updateModelResourcePool(pool: ModelResourcePool): Promise<ModelResourcePool> {
  const response = await put(
    `/api/v1/custom/derivative-control/resource-pools/${encodeURIComponent(pool.id)}`,
    pool,
    { headers: { 'If-Match': String(pool.policy_version) } },
  ) as unknown as ApiResponse<ModelResourcePool>
  return response.data
}

export async function drainModelResourcePool(pool: ModelResourcePool): Promise<void> {
  await post(
    `/api/v1/custom/derivative-control/resource-pools/${encodeURIComponent(pool.id)}/drain`,
    {},
    { headers: { 'If-Match': String(pool.policy_version) } },
  )
}

export async function resetModelResourcePool(pool: ModelResourcePool): Promise<ModelResourcePool> {
  const response = await post(
    `/api/v1/custom/derivative-control/resource-pools/${encodeURIComponent(pool.id)}/reset`,
    {},
    { headers: { 'If-Match': String(pool.policy_version) } },
  ) as unknown as ApiResponse<ModelResourcePool>
  return response.data
}

export async function listModelResourceBindings(): Promise<ModelResourceBinding[]> {
  const response = await get('/api/v1/custom/derivative-control/bindings') as unknown as ApiResponse<ModelResourceBinding[]>
  return response.data
}

export async function updateModelResourceBinding(
  binding: ModelResourceBinding,
): Promise<ModelResourceBinding> {
  const response = await put(
    `/api/v1/custom/derivative-control/bindings/${encodeURIComponent(binding.model_id)}`,
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
  const response = await get('/api/v1/custom/derivative-control/templates') as unknown as ApiResponse<ModelAdmissionTemplate[]>
  return response.data
}

export async function updateModelAdmissionTemplate(
  template: ModelAdmissionTemplate,
): Promise<ModelAdmissionTemplate> {
  const response = await put(
    `/api/v1/custom/derivative-control/templates/${encodeURIComponent(template.kind)}`,
    template,
    { headers: { 'If-Match': String(template.policy_version) } },
  ) as unknown as ApiResponse<ModelAdmissionTemplate>
  return response.data
}

export async function listModelQuotaPools(): Promise<ModelQuotaPool[]> {
  const response = await get('/api/v1/custom/derivative-control/quota-pools') as unknown as ApiResponse<ModelQuotaPool[]>
  return response.data
}

export async function listModelGatewayPools(): Promise<ModelGatewayPool[]> {
  const response = await get('/api/v1/custom/derivative-control/gateway-pools') as unknown as ApiResponse<ModelGatewayPool[]>
  return response.data
}

export async function listModelAdmissionAudits(): Promise<ModelAdmissionAudit[]> {
  const response = await get('/api/v1/custom/derivative-control/audits') as unknown as ApiResponse<ModelAdmissionAudit[]>
  return response.data
}

export async function reconcileModelResourcePools(): Promise<void> {
  await post('/api/v1/custom/derivative-control/reconcile', {})
}

export async function getDerivativeStatus(): Promise<DerivativeStatus> {
  const response = await get('/api/v1/custom/derivative-control/status') as unknown as ApiResponse<DerivativeStatus>
  return response.data
}
export async function getDerivativeAdminConfig(): Promise<DerivativeAdminConfig> {
  const response = await get('/api/v1/custom/derivative-control/config') as unknown as ApiResponse<DerivativeAdminConfig>
  return response.data
}

export async function publishDerivativeModel(
  modelId: string,
  modelTenantId: number,
): Promise<DerivativeAdminConfig> {
  const response = await post('/api/v1/custom/derivative-control/models', {
    model_id: modelId,
    model_tenant_id: modelTenantId,
  }) as unknown as ApiResponse<DerivativeAdminConfig>
  return response.data
}

export async function unpublishDerivativeModel(modelId: string): Promise<DerivativeAdminConfig> {
  const response = await del(
    `/api/v1/custom/derivative-control/models/${encodeURIComponent(modelId)}`,
  ) as unknown as ApiResponse<DerivativeAdminConfig>
  return response.data
}

export async function setDefaultDerivativeModel(modelId: string): Promise<DerivativeAdminConfig> {
  const response = await put('/api/v1/custom/derivative-control/default', {
    model_id: modelId,
  }) as unknown as ApiResponse<DerivativeAdminConfig>
  return response.data
}

export async function updateDerivativeTPM(tpm: number): Promise<number> {
  const response = await put('/api/v1/custom/derivative-control/tpm', {
    tpm,
  }) as unknown as ApiResponse<{ tpm: number }>
  return response.data.tpm
}

export async function testDerivativeModel(
  modelId: string,
  prompt = '只回复 OK',
): Promise<{
  ok: boolean
  elapsed_ms: number
  model_id: string
  content: string
}> {
  const response = await post(
    `/api/v1/custom/derivative-control/models/${encodeURIComponent(modelId)}/test`,
    { prompt },
  ) as unknown as ApiResponse<{
    ok: boolean
    elapsed_ms: number
    model_id: string
    content: string
  }>
  return response.data
}
