import assert from 'node:assert/strict'
import test from 'node:test'

import { sortPinnedFirstByRecency } from './pinOrdering.ts'

type Row = { id: string }

const keyOf = (row: Row) => `local:${row.id}`

test('sortPinnedFirstByRecency orders pinned rows by latest pin first', () => {
  const rows = [{ id: 'a' }, { id: 'b' }, { id: 'c' }, { id: 'd' }]

  const result = sortPinnedFirstByRecency(rows, ['local:b', 'local:d'], keyOf)

  assert.deepEqual(result.map((row) => row.id), ['d', 'b', 'a', 'c'])
})

test('sortPinnedFirstByRecency ignores stale pin keys and preserves unpinned order', () => {
  const rows = [{ id: 'a' }, { id: 'b' }, { id: 'c' }]

  const result = sortPinnedFirstByRecency(rows, ['local:missing', 'local:a'], keyOf)

  assert.deepEqual(result.map((row) => row.id), ['a', 'b', 'c'])
})
