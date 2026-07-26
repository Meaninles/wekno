import assert from 'node:assert/strict'
import test from 'node:test'

import {
  MOBILE_SSO_ENTRY,
  mobileSSOEntryForPath,
} from './authRedirect.ts'

test('routes every mobile history path to the mobile enterprise SSO entry', () => {
  for (const path of [
    '/mobile',
    '/mobile/',
    '/mobile/chat',
    '/mobile/settings/knowledge',
  ]) {
    assert.equal(mobileSSOEntryForPath(path), MOBILE_SSO_ENTRY)
  }
})

test('does not intercept desktop or similarly prefixed routes', () => {
  for (const path of ['/', '/login', '/mobile-old', '/mobile2/chat']) {
    assert.equal(mobileSSOEntryForPath(path), null)
  }
})
