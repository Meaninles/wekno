import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'
import test from 'node:test'

const here = dirname(fileURLToPath(import.meta.url))
const source = readFileSync(join(here, 'SourceReferenceTimeline.vue'), 'utf8')

test('source references are collapsed by default and can still be toggled', () => {
  assert.match(source, /const expanded = ref\(false\)/)
  assert.match(source, /v-show="expanded"/)
  assert.match(source, /@click="toggleExpanded"/)
  assert.match(source, /expanded \? 'chevron-down' : 'chevron-right'/)
})
