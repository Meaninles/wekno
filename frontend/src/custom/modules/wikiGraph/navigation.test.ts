import assert from 'node:assert/strict'
import test from 'node:test'

import {
  clampGraphPage,
  navigateGraphCenterState,
  popGraphCenterState,
} from './navigation.ts'

test('clicking a related node pivots center and preserves a bounded back path', () => {
  let state = { center: 'entity/a', history: [] as string[] }
  state = navigateGraphCenterState(state, 'entity/b')
  state = navigateGraphCenterState(state, 'entity/c')
  state = navigateGraphCenterState(state, 'entity/d', true, 2)

  assert.deepEqual(state, {
    center: 'entity/d',
    history: ['entity/b', 'entity/c'],
  })
  state = popGraphCenterState(state)
  assert.deepEqual(state, {
    center: 'entity/c',
    history: ['entity/b'],
  })
})

test('same-center and non-history navigation do not duplicate history', () => {
  const state = { center: 'entity/a', history: ['entity/root'] }
  assert.deepEqual(navigateGraphCenterState(state, 'entity/a'), state)
  assert.deepEqual(navigateGraphCenterState(state, 'entity/b', false), {
    center: 'entity/b',
    history: ['entity/root'],
  })
})

test('neighbor pages are always clamped to a valid bounded page', () => {
  assert.equal(clampGraphPage(-10, 8), 1)
  assert.equal(clampGraphPage(99, 8), 8)
  assert.equal(clampGraphPage(3, 8), 3)
  assert.equal(clampGraphPage(3, 0), 1)
})
