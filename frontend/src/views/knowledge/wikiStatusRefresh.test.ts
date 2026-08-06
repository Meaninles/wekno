import assert from 'node:assert/strict'
import test from 'node:test'

import {
  knowledgeHasDerivativeFailure,
  knowledgeIsFullyComplete,
  knowledgeNeedsStatusPolling,
  shouldRefreshWikiStatusAfterKnowledgePoll,
} from './wikiStatusRefresh.ts'

test('refreshes wiki status when a polled document leaves an in-flight state', () => {
  assert.equal(
    shouldRefreshWikiStatusAfterKnowledgePoll(
      { parse_status: 'finalizing', summary_status: 'processing' },
      { parse_status: 'completed', summary_status: 'completed' },
    ),
    true,
  )
})

test('does not refresh wiki status for ordinary in-flight polling updates', () => {
  assert.equal(
    shouldRefreshWikiStatusAfterKnowledgePoll(
      { parse_status: 'pending' },
      { parse_status: 'processing' },
    ),
    false,
  )
})

test('keeps polling when core parse is complete but Wiki is still queued', () => {
  assert.equal(
    knowledgeNeedsStatusPolling({
      parse_status: 'completed',
      summary_status: 'completed',
      enrichment_status: 'completed',
      wiki_status: 'pending',
    }),
    true,
  )
})

test('keeps polling when questions or graph enrichment is still queued', () => {
  assert.equal(
    knowledgeNeedsStatusPolling({
      parse_status: 'completed',
      summary_status: 'completed',
      enrichment_status: 'pending',
      wiki_status: 'completed',
    }),
    true,
  )
})

test('only reports full completion after every enabled derivative completed', () => {
  assert.equal(
    knowledgeIsFullyComplete({
      parse_status: 'completed',
      summary_status: 'completed',
      enrichment_status: 'completed',
      wiki_status: 'completed',
    }),
    true,
  )
  assert.equal(
    knowledgeIsFullyComplete({
      parse_status: 'completed',
      summary_status: 'completed',
      enrichment_status: 'completed',
      wiki_status: 'pending',
    }),
    false,
  )
})

test('distinguishes completed degradation from workflow failure', () => {
  assert.equal(
    knowledgeHasDerivativeFailure({
      parse_status: 'completed',
      summary_status: 'completed',
      enrichment_status: 'degraded',
      wiki_status: 'completed',
    }),
    false,
  )
  assert.equal(
    knowledgeHasDerivativeFailure({
      parse_status: 'completed',
      summary_status: 'completed',
      enrichment_status: 'completed',
      wiki_status: 'failed',
    }),
    true,
  )
})
