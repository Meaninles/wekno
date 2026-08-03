import assert from 'node:assert/strict'
import test from 'node:test'

import {
  agentToolCountFromMessage,
  formatCompletedRunDuration,
  isSimpleCompletedConversation,
  isEvidenceRetrievalToolName,
  retrievalDisplayCount,
  retrievalStatsFromMessage,
  usesDataSourceRetrievalUnit,
} from './retrievalSummary.ts'

test('retrieval stats are read from the backend authority without reference inference', () => {
  const stats = retrievalStatsFromMessage({
    retrieval_stats: { attempted: true, documents: 2, wiki: 1, web: 1, data_sources: 0, total: 4 },
    knowledge_references: [{ knowledge_id: 'only-cited-doc' }],
  })
  assert.deepEqual(stats, {
    attempted: true,
    documents: 2,
    wiki: 1,
    web: 1,
    dataSources: 0,
    total: 4,
    unit: 'documents',
  })
  assert.equal(usesDataSourceRetrievalUnit(stats), false)
  assert.equal(retrievalDisplayCount(stats), 4)
})

test('structured analysis uses the data-source retrieval unit', () => {
  const stats = retrievalStatsFromMessage({
    retrieval_stats: { attempted: true, documents: 0, wiki: 0, web: 0, data_sources: 2, total: 2 },
  })
  assert.equal(usesDataSourceRetrievalUnit(stats), true)
  assert.equal(retrievalDisplayCount(stats), 2)
})

test('mixed telemetry never labels document/wiki/web counts as data sources', () => {
  const stats = retrievalStatsFromMessage({
    retrieval_stats: { attempted: true, documents: 2, wiki: 1, web: 1, data_sources: 2, total: 6 },
  })
  assert.equal(usesDataSourceRetrievalUnit(stats), true)
  assert.equal(retrievalDisplayCount(stats), 2)
})

test('backend unit preserves data-source wording for zero results', () => {
  const stats = retrievalStatsFromMessage({
    retrieval_stats: {
      attempted: false,
      documents: 0,
      wiki: 0,
      web: 0,
      data_sources: 0,
      total: 0,
      unit: 'data_sources',
    },
  })
  assert.equal(usesDataSourceRetrievalUnit(stats), true)
})

test('retrieval tool classification covers native and Claude SDK namespaced tools', () => {
  for (const name of [
    'knowledge_search', 'wiki_read_page', 'web_fetch', 'db_catalog', 'db_schema',
    'table_schema', 'db_query', 'mcp__knowledge_search',
  ]) {
    assert.equal(isEvidenceRetrievalToolName(name), true, name)
  }
  for (const name of ['wiki_search', 'get_document_info', 'thinking', 'mcp__create_ticket']) {
    assert.equal(isEvidenceRetrievalToolName(name), false, name)
  }
})

test('completed duration uses whole units on every response surface', () => {
  assert.equal(formatCompletedRunDuration(0), '')
  assert.equal(formatCompletedRunDuration(842), '842ms')
  assert.equal(formatCompletedRunDuration(1_449), '1s')
  assert.equal(formatCompletedRunDuration(59_600), '1m')
  assert.equal(formatCompletedRunDuration(65_600), '1m 6s')
})

test('completed tool count uses backend authority instead of a partial live stream', () => {
  assert.equal(agentToolCountFromMessage({
    agent_tool_count: 6,
    agentEventStream: [{ type: 'tool_call', tool_name: 'Bash' }],
  }), 6)
})

test('only a completed direct text response is classified as a simple conversation', () => {
  const direct = {
    is_completed: true,
    content: '3+3等于6。',
    agent_tool_count: 0,
    knowledge_references: [],
    retrieval_stats: {
      attempted: false,
      documents: 0,
      wiki: 0,
      web: 0,
      data_sources: 0,
      total: 0,
      unit: 'documents',
    },
  }
  assert.equal(isSimpleCompletedConversation(direct), true)
  assert.equal(isSimpleCompletedConversation({ ...direct, agent_mode: true }), false)
  assert.equal(isSimpleCompletedConversation({ ...direct, content: '' }), false)
  assert.equal(isSimpleCompletedConversation({ ...direct, agent_tool_count: 1 }), false)
  assert.equal(isSimpleCompletedConversation({
    ...direct,
    retrieval_stats: { ...direct.retrieval_stats, attempted: true },
  }), false)
})
