import assert from 'node:assert/strict'
import test from 'node:test'

import type { ModelConfig } from '@/api/model'

import {
  derivativeModels,
  interactiveModels,
  isDerivativeModel,
  modelAllowedForUsage,
} from './modelPolicy.ts'

function model(
  id: string,
  workloadScope?: ModelConfig['workload_scope'],
): ModelConfig {
  return {
    id,
    name: id,
    type: 'KnowledgeQA',
    source: 'remote',
    status: 'active',
    workload_scope: workloadScope,
  } as ModelConfig
}

test('legacy and explicit interactive models stay in interactive selectors', () => {
  const legacy = model('legacy')
  const interactive = model('interactive', 'interactive')

  assert.equal(isDerivativeModel(legacy), false)
  assert.deepEqual(
    interactiveModels([legacy, interactive]).map(item => item.id),
    ['legacy', 'interactive'],
  )
  assert.equal(modelAllowedForUsage(legacy, 'interactive'), true)
  assert.equal(modelAllowedForUsage(interactive, 'derivative'), false)
})

test('derivative-only models cannot leak into chat or agent selectors', () => {
  const chat = model('chat', 'interactive')
  const derivative = model('derivative', 'derivative_only')

  assert.deepEqual(interactiveModels([chat, derivative]).map(item => item.id), ['chat'])
  assert.deepEqual(derivativeModels([chat, derivative]).map(item => item.id), ['derivative'])
  assert.equal(modelAllowedForUsage(derivative, 'interactive'), false)
  assert.equal(modelAllowedForUsage(derivative, 'derivative'), true)
})
