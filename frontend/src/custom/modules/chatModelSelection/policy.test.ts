import assert from 'node:assert/strict'
import test from 'node:test'

import { resolveChatModelSelection } from './policy.ts'

const catalog = ['qwen-agent-default', 'deepseek-user-choice', 'fallback']

test('explicit user model wins over the selected agent default at send time', () => {
  assert.deepEqual(resolveChatModelSelection({
    currentModelId: 'deepseek-user-choice',
    agentModelId: 'qwen-agent-default',
    lastUserModelId: 'deepseek-user-choice',
    availableModelIds: catalog,
    catalogReady: true,
  }), {
    modelId: 'deepseek-user-choice',
    source: 'explicit-user',
  })
})

test('explicit choice survives component remount while the model catalog hydrates', () => {
  assert.deepEqual(resolveChatModelSelection({
    currentModelId: 'deepseek-user-choice',
    agentModelId: 'qwen-agent-default',
    lastUserModelId: 'deepseek-user-choice',
    availableModelIds: [],
    catalogReady: false,
  }), {
    modelId: 'deepseek-user-choice',
    source: 'explicit-user',
  })
})

test('agent default wins after an agent switch replaces the current model', () => {
  assert.deepEqual(resolveChatModelSelection({
    currentModelId: 'qwen-agent-default',
    agentModelId: 'qwen-agent-default',
    lastUserModelId: 'deepseek-user-choice',
    availableModelIds: catalog,
    catalogReady: true,
  }), {
    modelId: 'qwen-agent-default',
    source: 'agent-default',
  })
})

test('removed explicit model is rejected once the catalog is authoritative', () => {
  assert.deepEqual(resolveChatModelSelection({
    currentModelId: 'removed-model',
    agentModelId: 'qwen-agent-default',
    lastUserModelId: 'removed-model',
    availableModelIds: catalog,
    catalogReady: true,
  }), {
    modelId: 'qwen-agent-default',
    source: 'agent-default',
  })
})

test('a model-free agent falls back deterministically to current then catalog', () => {
  assert.equal(resolveChatModelSelection({
    currentModelId: 'fallback',
    lastUserModelId: 'deepseek-user-choice',
    availableModelIds: catalog,
    catalogReady: true,
  }).modelId, 'fallback')

  assert.deepEqual(resolveChatModelSelection({
    availableModelIds: [' fallback ', 'second'],
    catalogReady: true,
  }), {
    modelId: 'fallback',
    source: 'catalog-default',
  })
})
