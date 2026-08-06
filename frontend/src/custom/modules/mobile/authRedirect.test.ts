import assert from 'node:assert/strict'
import test from 'node:test'

import {
  MOBILE_SSO_ENTRY,
  mobileReturnPath,
  mobileRouterPathForLocation,
  mobileSSOEntryForPath,
} from './authRedirect.ts'

test('routes every mobile history path through enterprise SSO and preserves the exact return path', () => {
  for (const path of [
    '/mobile',
    '/mobile/',
    '/mobile/chat',
    '/mobile/settings/knowledge',
  ]) {
    const entry = mobileSSOEntryForPath(path)
    assert.ok(entry)
    const parsed = new URL(entry, 'https://knora.example.com')
    assert.equal(parsed.pathname, MOBILE_SSO_ENTRY)
    assert.equal(parsed.searchParams.get('client'), 'mobile')
    assert.equal(parsed.searchParams.get('return_to'), path)
  }
})

test('preserves citation coordinates in SSO and converts browser paths to mobile router paths', () => {
  const pathname = '/mobile/reference'
  const search = '?type=knowledge&knowledge_base_id=kb-1&knowledge_id=doc-1&chunk_id=chunk-1'
  const entry = mobileSSOEntryForPath(pathname, search)
  assert.ok(entry)
  const parsed = new URL(entry, 'https://knora.example.com')
  assert.equal(parsed.searchParams.get('return_to'), pathname + search)
  assert.equal(mobileReturnPath(pathname, search), pathname + search)
  assert.equal(mobileRouterPathForLocation(pathname, search), '/reference' + search)
})

test('does not intercept desktop or similarly prefixed routes', () => {
  for (const path of ['/', '/login', '/mobile-old', '/mobile2/chat']) {
    assert.equal(mobileSSOEntryForPath(path), null)
  }
})
