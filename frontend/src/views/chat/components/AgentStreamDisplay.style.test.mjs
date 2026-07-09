import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'
import test from 'node:test'

const here = dirname(fileURLToPath(import.meta.url))
const source = readFileSync(join(here, 'AgentStreamDisplay.vue'), 'utf8')

test('agent steps use compact muted timeline styling', () => {
  assert.match(source, /--agent-step-text-size:\s*14px/)
  assert.match(source, /--agent-step-summary-size:\s*13px/)
  assert.match(source, /--agent-step-icon-color:\s*var\(--td-text-color-placeholder\)/)
  assert.match(source, /max-height:\s*none/)
  assert.match(source, /overflow-y:\s*visible/)
  assert.match(source, /\.tree-root \.action-name\s*\{[\s\S]*font-size:\s*14px/)
  assert.match(source, /\.tree-child \.action-title-icon\s*\{[\s\S]*position:\s*absolute/)
  assert.match(source, /function maskIconStyle\(src: string, size = 18\)/)
  assert.match(source, /\.icon-mask\s*\{[\s\S]*background-color:\s*var\(--agent-step-icon-color\)/)
  assert.doesNotMatch(source, /\.action-title \.action-title-icon,\s*\n\s*\.icon-mask\s*\{/)
})

test('expanded agent step log hides raw thinking narration', () => {
  assert.match(source, /visibleIntermediateEvents\s*=\s*computed/)
  assert.match(source, /e\.type === 'thinking'\)\s*return false/)
  assert.match(source, /e\.type === 'tool_call' && e\.tool_name === 'thinking'\)\s*return false/)
  assert.match(source, /v-for="\(event, index\) in visibleIntermediateEvents"/)
})

test('streaming log also hides raw thinking narration', () => {
  assert.match(source, /if \(!isConversationDone\.value\)\s*\{[\s\S]*return result\.filter/)
  assert.match(source, /e\.type === 'thinking'\) return false/)
  assert.match(source, /e\.type === 'tool_call' && e\.tool_name === 'thinking'\) return false/)
  assert.doesNotMatch(source, /if \(!isConversationDone\.value\)\s*\{\s*return result;\s*\}/)
})

test('transient status rows only render while they are the latest live event', () => {
  assert.match(source, /isTransientStatusEvent/)
  assert.match(source, /event\.transient_status === true/)
  assert.match(source, /event\.agent_progress\?\.transient === true/)
  assert.match(source, /event\.tool_name === 'assistant_status'/)
  assert.match(source, /if \(isTransientStatusEvent\(e\)\) return false/)
  assert.match(source, /if \(isTransientStatusEvent\(e\)\) return index === lastRenderableIndex/)
})

test('agent progress text is clamped with a fade mask', () => {
  assert.match(source, /agent-progress-title/)
  assert.match(source, /agent-progress-history-message/)
  assert.match(source, /-webkit-line-clamp:\s*2/)
  assert.match(source, /mask-image:\s*linear-gradient/)
})

test('live intermediate answer text is clamped to two lines', () => {
  assert.match(source, /isLiveIntermediateAnswerEvent/)
  assert.match(source, /answer-event--intermediate/)
  assert.match(source, /event\.final_answer === true/)
  assert.match(source, /-webkit-line-clamp:\s*2/)
  assert.match(source, /line-clamp:\s*2/)
})

test('streaming tool log uses the same timeline structure', () => {
  assert.match(source, /'is-streaming-timeline': showStreamingTimeline/)
  assert.match(source, /'tree-child': isStreamingTimelineEvent\(event\)/)
  assert.match(source, /class="tree-child tree-child-last streaming-loading-node"/)
  assert.match(source, /chat-timeline-loading\.less/)
  assert.match(source, /lastStreamingTimelineEventIndex\s*=\s*computed/)
})

test('final done row uses an existing common translation key', () => {
  assert.match(source, /t\('common\.finish'\)/)
  assert.doesNotMatch(source, /\$t\('common\.done'\)/)
  assert.match(source, /'tree-child-last': !isConversationDone && index === visibleIntermediateEvents\.length - 1/)
})

test('tool rows use line icon names instead of legacy asset masks', () => {
  assert.match(source, /getAgentToolIconName/)
  assert.match(source, /:name="getToolIconName\(event\.tool_name\)"/)
  assert.match(source, /wiki_search: 'agentEditor\.tools\.wikiSearch'/)
  assert.match(source, /wiki_read_page: 'agentEditor\.tools\.wikiReadPage'/)
  assert.match(source, /wiki_read_source_doc: 'agentStream\.tools\.wikiReadSourceDoc'/)
  assert.match(source, /toolName === 'get_document_content' \|\| toolName === 'wiki_read_source_doc'/)
  assert.doesNotMatch(source, /getToolIcon\(event\.tool_name\)/)
})

test('rag mode delegates pre-answer loading to pipeline and keeps dots while answer streams', () => {
  assert.match(source, /if \(props\.ragMode\) return hasAnswerStarted\.value \|\| hasNonRagToolEvents\.value/)
  assert.match(source, /v-if="!ragMode \|\| displayEvents\.length > 0 \|\| showAgentActivityIndicator"/)
})

test('rag mode keeps delegated pipeline rows out of the answer stream component', () => {
  assert.match(source, /isRagDelegatedEvent/)
  assert.match(source, /if \(props\.ragMode && !hasNonRagToolEvents\.value\)\s*\{[\s\S]*e\.type === 'answer'/)
  assert.match(source, /if \(isRagDelegatedEvent\(e\)\) return false/)
  assert.doesNotMatch(
    source,
    /if \(props\.ragMode\)\s*\{[\s\S]*e\.type === 'answer' \|\| e\.type === 'thinking'/,
  )
})

test('only the collapsed root summary shows an expand chevron', () => {
  assert.match(source, /tree-root-summary[\s\S]*class="action-show-icon"/)
  assert.match(source, /showIntermediateSteps \? 'chevron-down' : 'chevron-right'/)
  assert.doesNotMatch(source, /isEventExpanded\(event\.tool_call_id\) \? 'chevron/)
  assert.doesNotMatch(source, /isEventExpanded\(event\.event_id\) \? 'chevron/)
})

test('pending tool rows do not render an extra axis dot', () => {
  assert.doesNotMatch(source, /&\.action-pending\s*\{[\s\S]*&::after/)
})

test('agent mode keeps gray timeline dots for the full turn', () => {
  assert.match(source, /if \(isConversationDone\.value\) return false/)
  assert.match(source, /if \(props\.ragMode && !hasNonRagToolEvents\.value\) return false/)
  assert.match(source, /return true;\s*\}\);/)
  assert.match(source, /chat-timeline-loading\.less/)
})
