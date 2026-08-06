import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'
import test from 'node:test'

const here = dirname(fileURLToPath(import.meta.url))
const source = readFileSync(join(here, 'AgentStreamDisplay.vue'), 'utf8')

test('terminal error events stop agent activity and render visibly', () => {
  assert.match(source, /event\.type === 'error'/)
  assert.match(source, /agent-error-event/)
  assert.doesNotMatch(source, /console\.log/)
  assert.match(source, /e\.type === 'error'\) return false/)
  assert.match(source, /terminalError/)
})

test('agent progress messages are rendered as visible tool titles', () => {
  assert.match(source, /event\.agent_progress_history/)
  assert.match(source, /getAgentProgressMessage/)
  assert.match(source, /event\?\.agent_progress\?\.message/)
  assert.match(source, /event\?\.tool_data\?\.agent_progress_message/)
  assert.match(source, /if \(agentProgressMessage\)/)
})

test('rag mode still renders non-rag tool calls while delegating rag pipeline rows', () => {
  assert.match(source, /RAG_PIPELINE_TOOL_NAMES/)
  assert.match(source, /hasNonRagToolEvents/)
  assert.match(source, /props\.ragMode && props\.session\?\.agent_mode !== true/)
  assert.match(source, /if \(props\.ragMode && !hasNonRagToolEvents\.value\)/)
  assert.match(source, /if \(isRagDelegatedEvent\(e\)\) return false/)
  assert.match(source, /RAG_PIPELINE_TOOL_NAMES\.has\(event\.tool_name\)/)
})

test('answer text is not reclassified with operational preamble regexes', () => {
  assert.doesNotMatch(source, /OPERATIONAL_ANSWER_PREAMBLE_RE/)
  assert.doesNotMatch(source, /foldOperationalAnswerPreambles/)
  assert.doesNotMatch(source, /foldOperationalAnswerPreamble/)
  assert.doesNotMatch(source, /splitOperationalAnswerPreamble/)
})

test('event list helpers are hoisted for initial history render', () => {
  assert.match(source, /function buildFullEventList\(/)
  assert.doesNotMatch(source, /const buildFullEventList\s*=/)
})

test('answer display ignores superseded answer events', () => {
  assert.match(source, /e\.type === 'answer' && !e\.superseded/)
  assert.match(source, /const answerEvents = result\.filter\(\(e: any\) => e\.type === 'answer' && !e\.superseded\)/)
})

test('collapsed agent summary uses inspected-source counts for every agent runtime', () => {
  assert.match(source, /retrievalStatsFromMessage/)
  assert.match(source, /if \(retrievalStats\.value\)/)
  assert.match(source, /agent\.retrievedDocuments/)
  assert.match(source, /agent\.noRetrievedDocuments/)
  assert.match(source, /agent\.noToolCalls/)
  assert.match(source, /hasSteps \|\| retrievalStats\.value !== null/)
  assert.match(source, /isEvidenceRetrievalToolName\(e\.tool_name\)/)
  assert.doesNotMatch(source, /knowledgeSearchCallsCount/)
  assert.doesNotMatch(source, /agentStream\.summary\.searchKb/)
})

test('zero-result knowledge retrieval renders as neutral completion', () => {
  assert.match(source, /getKnowledgeSearchSummaryHtml/)
  assert.match(source, /agentStream\.ragPipeline\.searchDone/)
  assert.match(source, /getSearchResultsSummary\(event\)/)
})

test('Claude SDK live process output uses a bounded projection without rebuilding history', () => {
  assert.match(source, /LiveProcessPreview/)
  assert.match(source, /usesClaudeSDKTerminalDelivery/)
  assert.match(source, /liveProjection\.value\.interactiveEvents/)
  assert.match(
    source,
    /if \(\s*usesClaudeSDKTerminalDelivery\.value[\s\S]*!isConversationDone\.value[\s\S]*liveProjection\.value[\s\S]*\) \{[\s\S]*return interactive;/,
  )
  assert.match(source, /const result = buildFullEventList\(stream\);/)
})
