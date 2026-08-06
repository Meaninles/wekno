import assert from 'node:assert/strict'
import test from 'node:test'

import {
  getInitialWikiDisclosureExpanded,
  syncWikiDisclosureExpanded,
} from './disclosureState.ts'

test('keeps Wiki hidden behind the advanced disclosure by default', () => {
  assert.equal(getInitialWikiDisclosureExpanded(false), false)
})

test('opens the disclosure for an existing Wiki-enabled knowledge base', () => {
  assert.equal(getInitialWikiDisclosureExpanded(true), true)
})

test('opening Wiki reveals its settings without collapsing user-opened content', () => {
  assert.equal(syncWikiDisclosureExpanded(false, true), true)
  assert.equal(syncWikiDisclosureExpanded(true, false), true)
})
