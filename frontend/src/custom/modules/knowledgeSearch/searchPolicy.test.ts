import assert from 'node:assert/strict'
import test from 'node:test'

import {
  filterKnowledgeBasesByName,
  KNOWLEDGE_DOCUMENT_SEARCH_POLICY,
  normalizeKnowledgeSearchQuery,
} from './searchPolicy.ts'

test('shares a bounded incremental document-search policy across clients', () => {
  assert.deepEqual(KNOWLEDGE_DOCUMENT_SEARCH_POLICY, {
    minLength: 2,
    pageSize: 20,
    debounceMs: 360,
  })
})

test('normalizes surrounding whitespace without changing the fuzzy-search text', () => {
  assert.equal(normalizeKnowledgeSearchQuery('  共享资料  '), '共享资料')
  assert.equal(normalizeKnowledgeSearchQuery(undefined), '')
})

test('filters knowledge-base names case-insensitively and returns all rows for an empty query', () => {
  const rows = [{ name: 'Product Docs' }, { name: '共享资料库' }, { name: '研发手册' }]
  assert.deepEqual(filterKnowledgeBasesByName(rows, 'product'), [rows[0]])
  assert.deepEqual(filterKnowledgeBasesByName(rows, '资料'), [rows[1]])
  assert.deepEqual(filterKnowledgeBasesByName(rows, '  '), rows)
})
