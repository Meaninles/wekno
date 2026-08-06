import assert from 'node:assert/strict'
import test from 'node:test'

import {
  MAX_LIVE_PROCESS_PREVIEWS,
  addLiveInteractiveEvent,
  ensureLiveAgentProjection,
  resolveLiveInteractiveEvent,
  setLiveActiveAnswer,
  upsertLiveProcessPreview,
} from './liveProcessPreview.ts'

test('live process projection is a fixed three-item ring', () => {
  const message: Record<string, unknown> = {}
  for (let index = 0; index < 100; index += 1) {
    upsertLiveProcessPreview(message, `step-${index}`, `处理步骤 ${index}`)
  }

  const projection = ensureLiveAgentProjection(message)
  assert.equal(projection.previews.length, MAX_LIVE_PROCESS_PREVIEWS)
  assert.deepEqual(
    projection.previews.map((item) => item.text),
    ['处理步骤 97', '处理步骤 98', '处理步骤 99'],
  )
})

test('updating a process key replaces and moves only that fixed-size row', () => {
  const message: Record<string, unknown> = {}
  upsertLiveProcessPreview(message, 'search', '正在检索')
  upsertLiveProcessPreview(message, 'write', '正在写入')
  upsertLiveProcessPreview(message, 'search', '检索完成', 'success')

  const projection = ensureLiveAgentProjection(message)
  assert.deepEqual(
    projection.previews.map((item) => [item.key, item.text, item.state]),
    [
      ['write', '正在写入', 'running'],
      ['search', '检索完成', 'success'],
    ],
  )
})

test('large process output is reduced to a single bounded line', () => {
  const message: Record<string, unknown> = {}
  upsertLiveProcessPreview(message, 'large', `第一行\n${'内容'.repeat(500)}`)

  const [item] = ensureLiveAgentProjection(message).previews
  assert.equal(item.text.includes('\n'), false)
  assert.equal(Array.from(item.text).length, 180)
  assert.equal(item.text.endsWith('…'), true)
})

test('answer and interactive projections retain only live references', () => {
  const message: Record<string, unknown> = {}
  const answer = { type: 'answer', content: '最终回答', done: false }
  const approval = { type: 'tool_approval_required', pending_id: 'approval-1' }

  setLiveActiveAnswer(message, answer)
  addLiveInteractiveEvent(message, approval)
  resolveLiveInteractiveEvent(message, 'approval-1')

  const projection = ensureLiveAgentProjection(message)
  assert.equal(projection.activeAnswer, answer)
  assert.equal(projection.answerEverStarted, true)
  assert.equal(projection.interactiveEvents.length, 0)
})

