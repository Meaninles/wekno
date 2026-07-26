import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'
import test from 'node:test'

const here = dirname(fileURLToPath(import.meta.url))
const source = readFileSync(join(here, 'index.vue'), 'utf8')

test('every existing session restores the agent and model that produced its history', () => {
  assert.match(
    source,
    /const lastState = sessionRes\?\.data\?\.last_request_state;[\s\S]*?if \(lastState\) \{[\s\S]*?applyLastRequestState\(lastState\)/,
  )
  assert.doesNotMatch(source, /isScheduledChatSession/)
})
