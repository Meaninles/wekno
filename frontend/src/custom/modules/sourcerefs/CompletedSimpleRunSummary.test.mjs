import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'
import test from 'node:test'

const here = dirname(fileURLToPath(import.meta.url))
const source = readFileSync(join(here, 'CompletedSimpleRunSummary.vue'), 'utf8')

test('completed simple conversation renders duration and no zero-state telemetry', () => {
  assert.match(source, /isSimpleCompletedConversation/)
  assert.match(source, /formatCompletedRunDuration/)
  assert.match(source, /agent\.durationSuffix/)
  assert.doesNotMatch(source, /未引用|未检索|未调用/)
})
