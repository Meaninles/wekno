import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'
import test from 'node:test'

const here = dirname(fileURLToPath(import.meta.url))
const source = readFileSync(join(here, 'AgentSelector.vue'), 'utf8')

test('agent selector renders one top pinned section only when pins exist', () => {
  assert.match(source, /v-if="pinnedAgents\.length > 0"/)
  assert.match(source, /agent\.selector\.pinnedSection/)
  assert.match(source, /const pinnedAgents = computed<PinnedAgentSelection\[\]>/)
})

test('pinned agents are removed from their original groups', () => {
  assert.match(source, /localizedBuiltinAgents\.value\.filter\(agent => !agentPinned\(agent\)\)/)
  assert.match(source, /!agent\.is_builtin && !agentPinned\(agent\)/)
  assert.match(source, /allSharedAgentsList\.value\.filter\(shared => !sharedAgentPinned\(shared\)\)/)
})

test('top pinned section is ordered only by pin recency, not agent type', () => {
  assert.match(source, /const byKey = new Map<string, PinnedAgentSelection>\(\)/)
  assert.match(source, /Array\.from\(agentPins\.pinnedKeys\.value\)\s*\n\s*\.reverse\(\)/)
  assert.match(source, /\.map\(key => byKey\.get\(key\)\)/)
})
