import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'
import test from 'node:test'

const here = dirname(fileURLToPath(import.meta.url))
const source = readFileSync(join(here, 'useChatStreamHandler.ts'), 'utf8')

test('general-agent artifact tool calls do not supersede streamed answer text', () => {
  assert.match(source, /shouldSupersedeAgentAnswersForToolCall/)
  assert.match(source, /dataPayload\.preserve_answer === true\)\s*return false/)
  assert.match(source, /dataPayload\.tool_name !== 'create_artifact'/)
  assert.match(source, /shouldSupersedeAgentAnswersForToolCall\(dataPayload\) && message\.agentEventStream/)
})

test('terminal agent errors are appended to the agent event stream', () => {
  assert.match(source, /appendAgentErrorEvent/)
  assert.match(source, /type: 'error'/)
  assert.match(source, /appendAgentErrorEvent\(message, errorMsg, dataPayload\)/)
  assert.match(source, /appendAgentErrorEvent\(message, errorMsg\)/)
})

test('agent progress events are routed into visible tool cards', () => {
  assert.match(source, /data\.response_type === 'agent_progress'/)
  assert.match(source, /case 'agent_progress':/)
  assert.match(source, /agent_progress_history/)
  assert.match(source, /agent_progress:/)
  assert.match(source, /const transientStatus = dataPayload\?\.transient === true/)
  assert.match(source, /transient_status: transientStatus/)
})

test('agent answer chunks do not seed new event content from aggregate message content', () => {
  assert.doesNotMatch(source, /answerEvent\.content\s*=\s*message\.content/)
  assert.match(source, /answerEvent\.content = String\(answerEvent\.content \|\| ''\) \+ String\(data\.content\)/)
})

test('transient agent progress supersedes temporary answer narration', () => {
  assert.match(source, /shouldSupersedeAgentAnswersForProgress/)
  assert.match(source, /return dataPayload\.transient === true/)
  assert.match(
    source,
    /case 'agent_progress': \{[\s\S]*shouldSupersedeAgentAnswersForProgress\(dataPayload\)[\s\S]*supersedeAgentAnswers\(message\)/,
  )
})

test('agent complete normalizes live answers to backend final answer', () => {
  assert.match(source, /normalizeFinalAnswerFromComplete/)
  assert.match(source, /dataPayload\?\.final_answer/)
  assert.match(source, /ev\.type === 'answer'[\s\S]*ev\.superseded = true/)
  assert.match(source, /finalEvent\.final_answer = true/)
  assert.match(source, /message\.content = finalAnswer/)
  assert.match(
    source,
    /case 'complete': \{[\s\S]*normalizeFinalAnswerFromComplete\(message, dataPayload\)/,
  )
})
