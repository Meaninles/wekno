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
  origin?: string
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
