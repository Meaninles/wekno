import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'
import test from 'node:test'

const here = dirname(fileURLToPath(import.meta.url))
const source = readFileSync(join(here, 'views', 'MobileChat.vue'), 'utf8')

test('mobile composer clears only sent upload payloads', () => {
  assert.match(source, /const clearComposerInput = async \(\) => \{\s*inputValue\.value = "";\s*await nextTick\(\);\s*autoGrow\(\);\s*\}/)
  assert.match(source, /const clearUploadedFiles = \(\) => \{\s*pendingImages\.value = \[\];\s*pendingAttachments\.value = \[\];\s*\}/)
  assert.match(source, /await clearComposerInput\(\);\s*clearUploadedFiles\(\);\s*detachUploads\(\);\s*saveSessionDraftState\(\s*sessionId,\s*requestSettings,\s*\[\],\s*\[\],\s*""/)
})

test('mobile composer auto-grows after DOM value updates', () => {
  assert.match(source, /watch\(inputValue,\s*\(\) => \{\s*void nextTick\(autoGrow\);\s*\},\s*\{ flush: "post" \},?\s*\)/)
  assert.match(source, /const COMPOSER_MIN_HEIGHT = 28/)
})

test('mobile composer keeps knowledge base as the second config shortcut', () => {
  const configRail = source.slice(
    source.indexOf('<div class="config-rail"'),
    source.indexOf('<div class="input-row"'),
  )
  const agentPosition = configRail.indexOf("openSheet('agent')")
  const knowledgePosition = configRail.indexOf("openSheet('context')")
  const webPosition = configRail.indexOf('toggleWebSearch')

  assert.ok(agentPosition >= 0)
  assert.ok(knowledgePosition > agentPosition)
  assert.ok(webPosition > knowledgePosition)
})

test('mobile keeps web search implementation but hides its toggle', () => {
  assert.match(source, /const SHOW_MOBILE_WEB_SEARCH_TOGGLE = false/)
  const configRail = source.slice(
    source.indexOf('<div class="config-rail"'),
    source.indexOf('<div class="input-row"'),
  )
  assert.match(configRail, /v-if="SHOW_MOBILE_WEB_SEARCH_TOGGLE"[\s\S]*@click="toggleWebSearch"/)
})
