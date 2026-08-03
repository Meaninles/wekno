import assert from 'node:assert/strict'
import test from 'node:test'

import {
  formatRunWaiting,
  runWaitingElapsedSeconds,
  usesTimedRunWaiting,
} from './runWaiting.ts'

test('timed waiting is limited to ReAct and Claude SDK runs', () => {
  assert.equal(usesTimedRunWaiting({ agent_mode: true }), true)
  assert.equal(usesTimedRunWaiting({ _usesClaudeSDKTerminalDelivery: true }), true)
  assert.equal(usesTimedRunWaiting({ isAgentMode: true, agent_mode: false }), false)
  assert.equal(usesTimedRunWaiting({ agent_mode: false }), false)
  assert.equal(usesTimedRunWaiting({}), false)
})

test('waiting text changes only on whole five-second boundaries', () => {
  assert.equal(runWaitingElapsedSeconds(-1), 0)
  assert.equal(formatRunWaiting(0), '正在处理，请稍候')
  assert.equal(formatRunWaiting(4_999), '正在处理，请稍候')
  assert.equal(formatRunWaiting(5_000), '正在处理，请稍候 · 已用 5 秒')
  assert.equal(formatRunWaiting(9_999), '正在处理，请稍候 · 已用 5 秒')
  assert.equal(formatRunWaiting(65_900), '正在处理，请稍候 · 已用 65 秒')
})

