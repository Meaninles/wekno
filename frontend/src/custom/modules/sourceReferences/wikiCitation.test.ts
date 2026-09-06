import assert from 'node:assert/strict'
import test from 'node:test'
import { withWikiDirectoryEvidence } from './wikiCitation.ts'
import type { WikiPage } from '@/api/wiki'
import type { SourceReferenceItem } from '@/utils/sourceReferences'

const page = { page_type: 'index', slug: 'index', knowledge_base_id: 'kb-one', content: 'Stored introduction' } as WikiPage
const item = { type: 'wiki', slug: 'index', knowledgeBaseId: 'kb-one', content: '26 entities, showing top 1\nCurrent entry' } as SourceReferenceItem

test('directory preview shows the cited snapshot and preserves the page identity', () => {
  assert.deepEqual(withWikiDirectoryEvidence(page, [item]), { ...page, content: item.content })
  assert.equal(page.content, 'Stored introduction')
})

test('same slug in another knowledge base cannot replace the preview', () => {
  assert.equal(withWikiDirectoryEvidence(page, [{ ...item, knowledgeBaseId: 'kb-two' }]), page)
})

test('navigation to another wiki page uses its actual content', () => {
  const detail = { ...page, slug: 'entity/example', page_type: 'entity' }
  assert.equal(withWikiDirectoryEvidence(detail, [item]), detail)
})

test('missing or conflicting evidence does not invent a directory snapshot', () => {
  assert.equal(withWikiDirectoryEvidence(page, []), page)
  assert.equal(withWikiDirectoryEvidence(page, [{ ...item, content: '' }]), page)
  assert.equal(withWikiDirectoryEvidence(page, [item, { ...item, content: 'Different observation' }]), page)
})
