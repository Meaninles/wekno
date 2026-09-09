import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'
import test from 'node:test'

const here = dirname(fileURLToPath(import.meta.url))
const source = readFileSync(join(here, 'ScheduledTaskList.vue'), 'utf8')
const apiSource = readFileSync(join(here, 'api.ts'), 'utf8')

test('scheduled tasks keep web search state but hide its capability toggle', () => {
  assert.match(source, /const SHOW_SCHEDULED_WEB_SEARCH_TOGGLE = false/)
  assert.match(apiSource, /web_search_enabled:\s*boolean/)
  assert.match(
    source,
    /<t-button\s+v-if="SHOW_SCHEDULED_WEB_SEARCH_TOGGLE"[\s\S]*?网络搜索[\s\S]*?<\/t-button>/,
  )
  assert.match(source, /form\.web_search_enabled = task\.web_search_enabled/)
  assert.match(source, /web_search_enabled: task\.web_search_enabled/)
})
