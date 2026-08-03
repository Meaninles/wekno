import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'
import test from 'node:test'

const here = dirname(fileURLToPath(import.meta.url))
const source = readFileSync(join(here, 'Input-field.vue'), 'utf8')

test('selected file titles resolve before mentioned items are emitted', () => {
  const resolveCheck = source.indexOf('if (hasUnresolvedSelectedFiles())')
  const loadCall = source.indexOf('await loadFiles();', resolveCheck)
  const unresolvedWarning = source.indexOf("t('input.messages.fileInfoLoading')", loadCall)
  const mentionedItems = source.indexOf('const mentionedItems: MentionRequestItem[]', unresolvedWarning)

  assert.ok(resolveCheck >= 0, 'send boundary should check unresolved selected files')
  assert.ok(loadCall > resolveCheck, 'send boundary should await file title hydration')
  assert.ok(unresolvedWarning > loadCall, 'send should stop instead of persisting Loading...')
  assert.ok(mentionedItems > unresolvedWarning, 'mentioned items should be built only after titles resolve')
})
