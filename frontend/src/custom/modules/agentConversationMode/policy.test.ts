import assert from 'node:assert/strict'
import test from 'node:test'
import { resolveAgentEnabledFromMode } from './policy.ts'
test('the one recognized conversation mode always uses the agent stream', () => {
  assert.equal(resolveAgentEnabledFromMode('agent'), true)
  assert.equal(resolveAgentEnabledFromMode(undefined), null)
  assert.equal(resolveAgentEnabledFromMode('unknown'), null)
})
