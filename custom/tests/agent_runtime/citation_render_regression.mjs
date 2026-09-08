// Render saved live probes through the actual desktop/mobile shared renderer.
import assert from 'node:assert/strict'
import { readFileSync, readdirSync, writeFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { createChatMarkdownRenderer, renderChatMarkdown } from '../../../frontend/src/utils/chatMarkdownRenderer.ts'
import { buildCitedSourceReferenceItems } from '../../../frontend/src/utils/sourceReferences.ts'

const root = resolve('.local-data/agent-runtime-validation')
const results = []
for (const file of readdirSync(root).filter(name => /^citation-json-(qa|general)-.*\.json$/.test(name))) {
  const report = JSON.parse(readFileSync(resolve(root, file), 'utf8'))
  const result = report.run.result
  const items = buildCitedSourceReferenceItems(result.references, result.answer)
  const html = renderChatMarkdown(result.answer, {
    renderer: createChatMarkdownRenderer(), streaming: false,
    escapeMarkdown: text => text, sanitizeHtml: text => text,
    knowledgeReferences: result.references,
  })
  assert.ok(items.length > 0, file)
  assert.deepEqual(items.map(item => item.number), items.map((_, index) => index + 1), file)
  const markers = [...html.matchAll(/data-source-id="(S\d+)" data-citation-number="(\d+)"/g)]
  for (const item of items) {
    assert.ok(markers.some(([, id, number]) => id === item.citationId && Number(number) === item.number), file)
  }
  assert.equal(markers.length, [...result.answer.matchAll(/<src id="S\d+" \/>/g)].length, file)
  const actual = {name: report.name, run_id: report.run.id, markers: markers.length,
    sources: items.map(item => ({id: item.citationId, number: item.number, title: item.title})), passed: true}
  results.push(actual)
  console.log(JSON.stringify(actual))
}
assert.ok(results.length > 0)
writeFileSync(resolve(root, 'citation-render-results.json'), JSON.stringify(results, null, 2))
