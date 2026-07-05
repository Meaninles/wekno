import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'
import test from 'node:test'

const here = dirname(fileURLToPath(import.meta.url))
const source = readFileSync(join(here, 'AgentStreamDisplay.vue'), 'utf8')

test('terminal error events stop agent activity and render visibly', () => {
  assert.match(source, /event\.type === 'error'/)
  assert.match(source, /agent-error-event/)
  assert.doesNotMatch(source, /console\.log/)
  assert.match(source, /e\.type === 'error'\) return false/)
  assert.match(source, /terminalError/)
})

test('agent progress messages are rendered as visible tool titles', () => {
  assert.match(source, /event\.agent_progress_history/)
  assert.match(source, /getAgentProgressMessage/)
  assert.match(source, /event\?\.agent_progress\?\.message/)
  assert.match(source, /event\?\.tool_data\?\.agent_progress_message/)
  assert.match(source, /if \(agentProgressMessage\)/)
})

test('answer text is not reclassified with operational preamble regexes', () => {
  assert.doesNotMatch(source, /OPERATIONAL_ANSWER_PREAMBLE_RE/)
  assert.doesNotMatch(source, /foldOperationalAnswerPreambles/)
  assert.doesNotMatch(source, /foldOperationalAnswerPreamble/)
  assert.doesNotMatch(source, /splitOperationalAnswerPreamble/)
})

test('event list helpers are hoisted for initial history render', () => {
  assert.match(source, /function buildFullEventList\(/)
  assert.doesNotMatch(source, /const buildFullEventList\s*=/)
})

test('answer display ignores superseded answer events', () => {
  assert.match(source, /e\.type === 'answer' && !e\.superseded/)
  assert.match(source, /const answerEvents = result\.filter\(\(e: any\) => e\.type === 'answer' && !e\.superseded\)/)
})
