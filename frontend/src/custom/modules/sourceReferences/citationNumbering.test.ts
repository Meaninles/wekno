import assert from 'node:assert/strict'
import test from 'node:test'
import { buildCitedSourceReferenceItems } from '../../../utils/sourceReferences.ts'
import { extractCitationHtmlPlaceholders } from '../../../utils/citationMarkdown.ts'

test('filtered citations receive consecutive visible numbers while source identities stay stable', () => {
  const refs = [1, 2, 3, 4, 8, 9, 10].map(n => ({
    id: `chunk-${n}`, knowledge_id: 'doc', knowledge_base_id: 'kb',
    chunk_type: 'text', content: `片段 ${n}`, knowledge_title: '制度',
    metadata: { citation_id: `S${n}` },
  }))
  // This is the final insertion result: hidden sources must not occupy numbers,
  // even when the source registry still contains all retrieved evidence.
  const answer = '甲<src id="S8" /><src id="S10" /><src id="S2" />。乙<src id="S9" />。再次引用<src id="S8" />。'
  for (const sources of [refs, JSON.parse(JSON.stringify(refs))]) {
    const items = buildCitedSourceReferenceItems(sources, answer)
    assert.deepEqual(items.map(i => [i.citationId, i.number]), [['S8', 1], ['S10', 2], ['S2', 3], ['S9', 4]])
    const { htmlSnippets } = extractCitationHtmlPlaceholders(answer, sources)
    assert.deepEqual(htmlSnippets.map(html => html.match(/data-citation-number="(\d+)"/)?.[1]), ['1', '2', '3', '4', '1'])
  }
})
