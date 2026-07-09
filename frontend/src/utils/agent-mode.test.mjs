import assert from 'node:assert/strict'
import test from 'node:test'

const BUILTIN_QUICK_ANSWER_ID = 'builtin-quick-answer'
const BUILTIN_SIMPLE_CHAT_ID = 'builtin-simple-chat'
const BUILTIN_SMART_REASONING_ID = 'builtin-smart-reasoning'
const BUILTIN_WIKI_RESEARCHER_ID = 'builtin-wiki-researcher'
const BUILTIN_DEEP_RESEARCHER_ID = 'builtin-deep-researcher'
const BUILTIN_DATA_ANALYST_ID = 'builtin-data-analyst'
const BUILTIN_TABLE_ANALYST_ID = 'builtin-table-analyst'
const BUILTIN_GENERAL_AGENT_ID = 'builtin-general-agent'
const BUILTIN_DOCUMENT_PROCESSING_ID = 'builtin-document-processing'
const BUILTIN_KNOWLEDGE_GRAPH_EXPERT_ID = 'builtin-knowledge-graph-expert'

const NORMAL_MODE_BUILTIN_AGENT_IDS = new Set([
  BUILTIN_QUICK_ANSWER_ID,
  BUILTIN_SIMPLE_CHAT_ID,
])

const AGENT_STREAM_BUILTIN_AGENT_IDS = new Set([
  BUILTIN_SMART_REASONING_ID,
  BUILTIN_WIKI_RESEARCHER_ID,
  BUILTIN_DEEP_RESEARCHER_ID,
  BUILTIN_DATA_ANALYST_ID,
  BUILTIN_TABLE_ANALYST_ID,
  BUILTIN_GENERAL_AGENT_ID,
  BUILTIN_DOCUMENT_PROCESSING_ID,
  BUILTIN_KNOWLEDGE_GRAPH_EXPERT_ID,
])

function isQuickAnswerAgentId(agentId) {
  return NORMAL_MODE_BUILTIN_AGENT_IDS.has(agentId || BUILTIN_QUICK_ANSWER_ID)
}

function isAgentStreamBuiltinAgentId(agentId) {
  return AGENT_STREAM_BUILTIN_AGENT_IDS.has(agentId || '')
}

function isAgentStreamAgentId(agentId, isAgentEnabled) {
  const id = agentId || BUILTIN_QUICK_ANSWER_ID
  if (NORMAL_MODE_BUILTIN_AGENT_IDS.has(id)) return false
  if (AGENT_STREAM_BUILTIN_AGENT_IDS.has(id)) return true
  return isAgentEnabled
}

function reconcileBuiltinAgentMode(settings) {
  const agentId = settings.selectedAgentId || BUILTIN_QUICK_ANSWER_ID
  if (NORMAL_MODE_BUILTIN_AGENT_IDS.has(agentId) && settings.isAgentEnabled) {
    settings.isAgentEnabled = false
    return true
  }
  if (AGENT_STREAM_BUILTIN_AGENT_IDS.has(agentId) && !settings.isAgentEnabled) {
    settings.isAgentEnabled = true
    return true
  }
  return false
}

test('isQuickAnswerAgentId treats builtin quick-answer as RAG mode', () => {
  assert.equal(isQuickAnswerAgentId('builtin-quick-answer'), true)
  assert.equal(isQuickAnswerAgentId('builtin-simple-chat'), true)
  assert.equal(isQuickAnswerAgentId(undefined), true)
  assert.equal(isQuickAnswerAgentId('builtin-smart-reasoning'), false)
  assert.equal(isQuickAnswerAgentId('builtin-table-analyst'), false)
})

test('isAgentStreamAgentId prefers selectedAgentId for builtins', () => {
  assert.equal(
    isAgentStreamAgentId('builtin-quick-answer', true),
    false,
    'stale isAgentEnabled must not flip quick-answer into agent stream',
  )
  assert.equal(
    isAgentStreamAgentId('builtin-simple-chat', true),
    false,
    'simple chat must use the normal KnowledgeQA stream',
  )
  assert.equal(isAgentStreamAgentId('builtin-smart-reasoning', false), true)
  assert.equal(
    isAgentStreamAgentId('builtin-table-analyst', false),
    true,
    'table analyst must not fall back to quick-answer when the persisted flag is stale',
  )
  assert.equal(isAgentStreamAgentId('builtin-data-analyst', false), true)
  assert.equal(isAgentStreamAgentId('builtin-general-agent', false), true)
  assert.equal(isAgentStreamAgentId('builtin-document-processing', false), true)
  assert.equal(isAgentStreamAgentId('builtin-wiki-researcher', false), true)
  assert.equal(isAgentStreamAgentId('custom-agent', true), true)
  assert.equal(isAgentStreamAgentId('custom-agent', false), false)
})

test('isAgentStreamBuiltinAgentId recognizes smart built-in agents', () => {
  assert.equal(isAgentStreamBuiltinAgentId('builtin-table-analyst'), true)
  assert.equal(isAgentStreamBuiltinAgentId('builtin-quick-answer'), false)
  assert.equal(isAgentStreamBuiltinAgentId('custom-agent'), false)
})

test('reconcileBuiltinAgentMode repairs drifted localStorage flags', () => {
  const quick = { selectedAgentId: 'builtin-quick-answer', isAgentEnabled: true }
  assert.equal(reconcileBuiltinAgentMode(quick), true)
  assert.equal(quick.isAgentEnabled, false)

  const simple = { selectedAgentId: 'builtin-simple-chat', isAgentEnabled: true }
  assert.equal(reconcileBuiltinAgentMode(simple), true)
  assert.equal(simple.isAgentEnabled, false)

  const reasoning = { selectedAgentId: 'builtin-smart-reasoning', isAgentEnabled: false }
  assert.equal(reconcileBuiltinAgentMode(reasoning), true)
  assert.equal(reasoning.isAgentEnabled, true)

  const table = { selectedAgentId: 'builtin-table-analyst', isAgentEnabled: false }
  assert.equal(reconcileBuiltinAgentMode(table), true)
  assert.equal(table.isAgentEnabled, true)

  const custom = { selectedAgentId: 'custom-agent', isAgentEnabled: true }
  assert.equal(reconcileBuiltinAgentMode(custom), false)
})
