import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'
import test from 'node:test'

const here = dirname(fileURLToPath(import.meta.url))
const source = readFileSync(join(here, 'index.vue'), 'utf8')

test('only scheduler-created sessions restore their saved agent selection', () => {
  assert.match(source, /startsWith\('custom:scheduled-chat:'\)/)
  assert.match(
    source,
    /if \(isScheduledChatSession\(sessionRes\?\.data\)\) \{[\s\S]*?applyLastRequestState\(lastState\)[\s\S]*?\} else \{[\s\S]*?applyConversationResourceState\(lastState\)/,
  )
})
