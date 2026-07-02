import assert from 'node:assert/strict'
import test from 'node:test'

import { dedupeChatModelOptions, type ModelOptionLike } from './dedupe.ts'

function chatModel(overrides: Partial<ModelOptionLike>): ModelOptionLike {
  return {
    id: 'model-1',
    name: 'deepseek-v4-flash',
    type: 'KnowledgeQA',
    source: 'remote',
    status: 'active',
    ...overrides,
  }
}

test('dedupeChatModelOptions collapses visually identical chat models', () => {
  const models = [
    chatModel({ id: 'first' }),
    chatModel({ id: 'second' }),
    chatModel({ id: 'third' }),
  ]

  const result = dedupeChatModelOptions(models)

  assert.deepEqual(result.map((model) => model.id), ['first'])
})

test('dedupeChatModelOptions keeps the selected duplicate when provided', () => {
  const models = [
    chatModel({ id: 'first' }),
    chatModel({ id: 'selected' }),
    chatModel({ id: 'third' }),
  ]

  const result = dedupeChatModelOptions(models, 'selected')

  assert.deepEqual(result.map((model) => model.id), ['selected'])
})

test('dedupeChatModelOptions keeps options that render differently', () => {
  const result = dedupeChatModelOptions([
    chatModel({ id: 'remote' }),
    chatModel({ id: 'named', display_name: 'Fast model' }),
    chatModel({ id: 'local', source: 'local', parameters: { parameter_size: '7B' } }),
  ])

  assert.deepEqual(result.map((model) => model.id), ['remote', 'named', 'local'])
})

test('dedupeChatModelOptions hides managed builtin-agent clone behind normal model', () => {
  const result = dedupeChatModelOptions([
    chatModel({ id: 'normal' }),
    chatModel({
      id: 'managed-clone',
      managed_by: 'builtin_agent_defaults',
      description: 'source clone may have a different endpoint',
    }),
  ])

  assert.deepEqual(result.map((model) => model.id), ['normal'])
})

test('dedupeChatModelOptions keeps selected managed builtin-agent clone', () => {
  const result = dedupeChatModelOptions([
    chatModel({ id: 'normal' }),
    chatModel({
      id: 'managed-clone',
      managed_by: 'builtin_agent_defaults',
      description: 'source clone may have a different endpoint',
    }),
  ], 'managed-clone')

  assert.deepEqual(result.map((model) => model.id), ['managed-clone'])
})
