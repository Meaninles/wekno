import assert from 'node:assert/strict'
import test from 'node:test'

import {
  buildMobileKnowledgeCatalog,
  mergeMobileChatKnowledgeBases,
  type SharedKnowledgeBaseLike,
} from './knowledgeCatalog.ts'

const ME = 'user-me'

function shared(id: string, permission: string, org = '产品组'): SharedKnowledgeBaseLike {
  return {
    knowledge_base: { id, name: `共享-${id}` },
    permission,
    org_name: org,
    share_id: `share-${id}-${permission}`,
    shared_at: '2026-07-13T00:00:00Z',
    source_tenant_id: 8,
  }
}

test('groups only knowledge bases created by the caller as personal', () => {
  const rows = buildMobileKnowledgeCatalog(
    [
      { id: 'mine', name: '我的库', creator_id: ME },
      { id: 'teammate', name: '同事库', creator_id: 'user-other', creator_name: '小王' },
      { id: 'legacy', name: '历史库' },
    ],
    [],
    { currentUserId: ME, currentTenantRole: 'contributor' },
  )

  assert.equal(rows.find((row) => row.id === 'mine')?.group, 'personal')
  assert.equal(rows.find((row) => row.id === 'mine')?.access, 'manage')
  assert.equal(rows.find((row) => row.id === 'teammate')?.group, 'shared')
  assert.equal(rows.find((row) => row.id === 'teammate')?.access, 'view')
  assert.equal(rows.find((row) => row.id === 'teammate')?.originLabel, '小王 创建')
  assert.equal(rows.find((row) => row.id === 'legacy')?.group, 'shared')
})

test('tenant admins can manage teammate knowledge bases', () => {
  const [row] = buildMobileKnowledgeCatalog(
    [{ id: 'teammate', name: '同事库', creator_id: 'user-other' }],
    [],
    { currentUserId: ME, currentTenantRole: 'admin' },
  )
  assert.equal(row.access, 'manage')
  assert.equal(row.canEditContent, true)
  assert.equal(row.canManage, true)
})

test('maps organization admin/editor/viewer to manage/edit/view', () => {
  const rows = buildMobileKnowledgeCatalog(
    [],
    [shared('view', 'viewer'), shared('edit', 'editor'), shared('admin', 'admin')],
    { currentUserId: ME, currentTenantRole: 'owner' },
  )
  assert.equal(rows.find((row) => row.id === 'admin')?.permissionLabel, '可管理')
  assert.equal(rows.find((row) => row.id === 'edit')?.permissionLabel, '可编辑')
  assert.equal(rows.find((row) => row.id === 'view')?.permissionLabel, '仅查看')
  assert.equal(rows.find((row) => row.id === 'view')?.canEditContent, false)
})

test('local row wins over a share-back duplicate', () => {
  const rows = buildMobileKnowledgeCatalog(
    [{ id: 'same', name: '本地名称', creator_id: ME }],
    [shared('same', 'viewer')],
    { currentUserId: ME, currentTenantRole: 'contributor' },
  )
  assert.deepEqual(rows.map((row) => row.id), ['same'])
  assert.equal(rows[0].group, 'personal')
  assert.equal(rows[0].name, '本地名称')
})

test('repeated organization shares keep the strongest effective permission', () => {
  const rows = buildMobileKnowledgeCatalog(
    [],
    [shared('same', 'viewer', 'A组'), shared('same', 'admin', 'B组'), shared('same', 'editor', 'C组')],
  )
  assert.equal(rows.length, 1)
  assert.equal(rows[0].access, 'manage')
  assert.equal(rows[0].org_name, 'B组')
})

test('chat selector merges direct shares once and keeps a local duplicate', () => {
  const rows = mergeMobileChatKnowledgeBases(
    [{ id: 'local', name: '本地库' }],
    [shared('local', 'viewer'), shared('remote', 'viewer'), shared('remote', 'editor', '编辑组')],
  )
  assert.deepEqual(rows.map((row) => row.id), ['local', 'remote'])
  assert.equal(rows[0].name, '本地库')
  assert.equal(rows[1].permission, 'editor')
  assert.equal(rows[1].org_name, '编辑组')
})
