import assert from 'node:assert/strict'
import test from 'node:test'

import { resolveAgentEnabledFromMode } from './policy.ts'

test('smart-reasoning enables the agent stream runtime', () => {
  assert.equal(resolveAgentEnabledFromMode('smart-reasoning'), true)
})

test('quick-answer disables the agent stream runtime', () => {
  assert.equal(resolveAgentEnabledFromMode('quick-answer'), false)
})

test('unresolved and unknown agent modes do not overwrite current state', () => {
  assert.equal(resolveAgentEnabledFromMode(''), null)
  assert.equal(resolveAgentEnabledFromMode(undefined), null)
  assert.equal(resolveAgentEnabledFromMode('future-mode'), null)
})
