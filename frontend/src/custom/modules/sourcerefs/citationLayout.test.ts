import assert from 'node:assert/strict'
import test from 'node:test'
import { createChatMarkdownRenderer, renderChatMarkdown } from '../../../utils/chatMarkdownRenderer.ts'

// Same source order and placement as the reported project master-data answer.
const ids = ['S3', 'S7', 'S6', 'S1']
const refs = ids.map((id) => ({
  id: `chunk-${id}`, knowledge_id: 'doc-1', knowledge_base_id: 'kb-1',
  knowledge_title: '主数据管理规定.doc', metadata: { citation_id: id },
}))
const options = {
  renderer: createChatMarkdownRenderer(), knowledgeReferences: refs,
  escapeMarkdown: (text: string) => text, sanitizeHtml: (html: string) => html,
}
const citations = '<src id="S3" /><src id="S7" /><src id="S6" />'
const table = '| 字段编码 | 字段名称 | 数据类型 | 允许为空 | 传值说明 |\n'
  + '| --- | --- | --- | --- | --- |\n'
  + '| PROJ_NO | 项目编码 | VARCHAR(32) | 不允许 | 主键 |\n'
  + '| UPD_TIME | 更新时间 | VARCHAR(19) | 不允许 | YYYY-MM-DD HH:mm:SS |'
const answer = `项目主数据字段如下：\n\n${table}\n\n${citations}\n\n说明：由企业发展部管理。<src id="S1" />`

for (const streaming of [false, true]) {
  test(`table citations retain all four source links (streaming=${streaming})`, () => {
    const html = renderChatMarkdown(answer, { ...options, streaming })
    assert.deepEqual([...html.matchAll(/data-source-id="(S\d+)"/g)].map((m) => m[1]), ids)
    assert.deepEqual([...html.matchAll(/data-citation-number="(\d+)"/g)].map((m) => m[1]), ['1', '2', '3', '4'])
    const renderedTable = html.match(/<table>[\s\S]*?<\/table>/)?.[0] || ''
    assert.equal((renderedTable.match(/<td>/g) || []).length, 10)
    assert.doesNotMatch(renderedTable, /citation-source/)
    for (const id of ids) assert.match(html, new RegExp(`chunk_id=chunk-${id}`))
    assert.ok(html.indexOf('</table>') < html.indexOf('data-source-id="S3"'))
  })
}

test('streamed table citations remain visible as each complete tag arrives', () => {
  for (let length = 1; length <= answer.length; length++) {
    const prefix = answer.slice(0, length)
    const html = renderChatMarkdown(prefix, { ...options, streaming: true })
    const expected = [...prefix.matchAll(/<src id="(S\d+)" \/>/g)].map((m) => m[1])
    assert.deepEqual([...html.matchAll(/data-source-id="(S\d+)"/g)].map((m) => m[1]), expected, `frame ${length}`)
  }
})

for (const block of [table, '```text\n示例\n```', '    缩进代码', '---', '标题\n===', '> 引用段落']) {
  test(`standalone citations preserve the preceding block: ${block.split('\n')[0]}`, () => {
    const original = renderChatMarkdown(block, options)
    const html = renderChatMarkdown(`${block}\n\n${citations}`, options)
    assert.ok(html.startsWith(original), 'the preceding block must remain unchanged')
    assert.equal((html.match(/ data-source-id=/g) || []).length, 3)
  })
}

test('paragraph and list citations still attach to their text', () => {
  for (const text of ['已完成。', '- 已完成。', '- 第一项\n- 已完成。']) {
    const html = renderChatMarkdown(`${text}\n\n${citations}`, options)
    assert.match(html, /已完成。 <a class="citation/)
    assert.equal((html.match(/ data-source-id=/g) || []).length, 3)
  }
})

test('citations inside table cells keep their original cells', () => {
  const html = renderChatMarkdown(`| 字段 | 来源 |\n| --- | --- |\n| 编码 | ${citations} |`, options)
  const renderedTable = html.match(/<table>[\s\S]*?<\/table>/)?.[0] || ''
  assert.equal((renderedTable.match(/<td>/g) || []).length, 2)
  assert.equal((renderedTable.match(/ data-source-id=/g) || []).length, 3)
})
