import type { WikiPage } from '@/api/wiki'
import type { SourceReferenceItem } from '@/utils/sourceReferences'

// The live directory is assembled at read time, not stored in page.content.
// Show the cited snapshot, whose counts and entries belong to this answer.
export function withWikiDirectoryEvidence(page: WikiPage, items: SourceReferenceItem[]): WikiPage {
  if (page.page_type !== 'index') return page
  const matches = items.filter(item => item.type === 'wiki'
    && item.knowledgeBaseId === page.knowledge_base_id
    && item.slug === page.slug
    && item.content?.trim())
  // Multiple observations can differ; never silently select a different snapshot.
  const contents = [...new Set(matches.map(item => item.content!))]
  if (contents.length !== 1) return page
  return { ...page, content: contents[0] }
}
