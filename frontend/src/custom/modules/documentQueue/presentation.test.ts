import assert from 'node:assert/strict'
import test from 'node:test'
import { normalizeKnowledgeIds } from './ids.ts'
import { documentQueueBadgeView } from './presentation.ts'

test('normalizes visible knowledge ids without changing their queue order', () => {
  assert.deepEqual(normalizeKnowledgeIds([' doc-a ', '', 'doc-b', 'doc-a']), ['doc-a', 'doc-b'])
})

test('keeps a large visible page in one normalized queue snapshot', () => {
  const ids = Array.from({ length: 205 }, (_, index) => `doc-${index}`)
  assert.equal(normalizeKnowledgeIds(ids).length, 205)
})

test('only waiting documents with a positive position get a queue badge', () => {
  assert.deepEqual(documentQueueBadgeView({ state: 'active', position: 0 }, 12), {
    visible: false,
    position: 0,
    total: null,
  })
  assert.deepEqual(documentQueueBadgeView({ state: 'waiting', position: 0 }, 12), {
    visible: false,
    position: 0,
    total: null,
  })
})

test('renders a consistent position and global waiting total', () => {
  assert.deepEqual(documentQueueBadgeView({ state: 'waiting', position: 8 }, 12), {
    visible: true,
    position: 8,
    total: 12,
  })
  assert.deepEqual(documentQueueBadgeView({ state: 'waiting', position: 8 }, 3), {
    visible: true,
    position: 8,
    total: 8,
  })
})
