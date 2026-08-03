import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'
import test from 'node:test'

const here = dirname(fileURLToPath(import.meta.url))
const source = readFileSync(join(here, 'components', 'MobileChatMessage.vue'), 'utf8')

test('mobile uses the same timed projection only for ReAct and Claude SDK runs', () => {
  assert.match(source, /RunWaitingIndicator/)
  assert.match(source, /usesTimedRunWaiting\(props\.message\)/)
  assert.match(source, /!usesTimedWaitingProjection\.value/)
  assert.match(source, /!showRunWaitingIndicator/)
})

