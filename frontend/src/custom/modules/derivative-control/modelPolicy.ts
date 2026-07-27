import type { ModelConfig } from '@/api/model'

export type ModelUsageScope = 'interactive' | 'derivative'
export type ModelManagementType = 'chat' | 'embedding' | 'rerank' | 'vllm' | 'asr' | 'derivative'

const backendTypeToManagementType: Record<ModelConfig['type'], Exclude<ModelManagementType, 'derivative'>> = {
  KnowledgeQA: 'chat',
  Embedding: 'embedding',
  Rerank: 'rerank',
  VLLM: 'vllm',
  ASR: 'asr',
}

export function isDerivativeModel(model?: Pick<ModelConfig, 'workload_scope'> | null): boolean {
  return model?.workload_scope === 'derivative_only'
}

export function modelAllowedForUsage(
  model: ModelConfig,
  usage: ModelUsageScope,
): boolean {
  return usage === 'derivative'
    ? isDerivativeModel(model)
    : !isDerivativeModel(model)
}

export function interactiveModels(models: ModelConfig[]): ModelConfig[] {
  return models.filter(model => !isDerivativeModel(model))
}

export function derivativeModels(models: ModelConfig[]): ModelConfig[] {
  return models.filter(isDerivativeModel)
}

export function modelManagementType(model: ModelConfig): ModelManagementType {
  return isDerivativeModel(model) ? 'derivative' : backendTypeToManagementType[model.type]
}
