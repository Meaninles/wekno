import assert from 'node:assert/strict'
import test from 'node:test'
import type { Router } from 'vue-router'

import { openRouteInNewPage, type OpenWindow } from './openRouteInNewPage.ts'

test('opens a resolved internal route in an isolated new page', () => {
  const calls: Parameters<OpenWindow>[] = []
  const router = {
    resolve: () => ({ href: '/mobile/settings/knowledge?kb=kb-1' }),
  } as unknown as Router
  const openWindow: OpenWindow = (...args) => {
    calls.push(args)
    return null
  }

  const href = openRouteInNewPage(router, { name: 'mobile-knowledge' }, openWindow)

  assert.equal(href, '/mobile/settings/knowledge?kb=kb-1')
  assert.deepEqual(calls, [[
    '/mobile/settings/knowledge?kb=kb-1',
    '_blank',
    'noopener,noreferrer',
  ]])
})
