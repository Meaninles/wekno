import test from 'node:test'
import assert from 'node:assert/strict'
import { messageFailure, publicFailure } from './failures.ts'

test('public failure categories never disclose arbitrary provider diagnostics', () => {
  assert.equal(publicFailure('Traceback private-token=secret').code, 'unknown')
  assert.equal(publicFailure('workspace input id 8541320, line 429').code, 'unknown')
  assert.equal(publicFailure('HTTP 429 Too Many Requests').code, 'busy')
  assert.equal(publicFailure('deadline exceeded').code, 'timeout')
  assert.equal(publicFailure('Answer must not be empty.').code, 'empty_response')
  assert.equal(publicFailure('private token', 'permission_denied').code, 'permission_denied')
  assert.equal(publicFailure(publicFailure('EOF').message).code, 'connection')
})

test('empty completed answers fail, active and stopped replies do not', () => {
  assert.equal(messageFailure({ role: 'assistant', is_completed: true, content: ' \n ' })?.code, 'empty_response')
  assert.equal(messageFailure({ is_completed: false }), null)
  assert.equal(messageFailure({ role: 'user', is_completed: true }), null)
  assert.equal(messageFailure({ is_completed: true, agentEventStream: [{ type: 'stop' }] }), null)
  assert.equal(messageFailure({ is_completed: true, artifacts: [{}] }), null)
  assert.equal(messageFailure({ is_completed: true, content: '有效回答' }), null)
})

test('persisted and streaming failures use the same presentation', () => {
  const stored = messageFailure({ error_code: 'timeout', content: 'private diagnostic', is_completed: true })
  const live = messageFailure({ agentEventStream: [{ type: 'error', content: 'deadline exceeded', done: true }] })
  assert.deepEqual(stored, live)
  assert.equal(messageFailure({ content: '已恢复并回答', is_completed: true, agentEventStream: [{ type: 'tool_call', success: false, error: 'EOF' }] }), null)
})
