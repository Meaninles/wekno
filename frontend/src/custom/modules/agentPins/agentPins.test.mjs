import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'
import test from 'node:test'

const here = dirname(fileURLToPath(import.meta.url))
const source = readFileSync(join(here, 'agentPins.ts'), 'utf8')

test('chat agent pins are scoped as a per-user UI preference', () => {
  assert.match(source, /per-user UI preference/)
  assert.match(source, /readUserId\(\)/)
  assert.match(source, /WeKnora_\$\{readUserId\(\)\}_/)
  assert.doesNotMatch(source, /updateAgent\(|createAgent\(|put\(|post\(/)
})

test('pin sorting only moves pinned entries ahead and preserves original order inside buckets', () => {
  assert.match(source, /stablePinnedFirst/)
  assert.match(source, /if \(a\.pinned !== b\.pinned\) return a\.pinned \? -1 : 1/)
  assert.match(source, /return a\.index - b\.index/)
})
