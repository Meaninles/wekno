import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'
import test from 'node:test'

const here = dirname(fileURLToPath(import.meta.url))
const source = readFileSync(join(here, 'views', 'MobileChat.vue'), 'utf8')

test('mobile composer resets textarea height after clearing sent text', () => {
  assert.match(source, /const clearComposerInput = async \(\) => \{\s*inputValue\.value = "";\s*await nextTick\(\);\s*autoGrow\(\);\s*\}/)
  assert.match(source, /await clearComposerInput\(\);\s*saveSessionDraftState/)
})

test('mobile composer auto-grows after DOM value updates', () => {
  assert.match(source, /watch\(inputValue,\s*\(\) => \{\s*void nextTick\(autoGrow\);\s*\},\s*\{ flush: "post" \},?\s*\)/)
  assert.match(source, /const COMPOSER_MIN_HEIGHT = 28/)
})
