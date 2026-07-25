import assert from 'node:assert/strict'
import test from 'node:test'
import {
  knowledgeHasDerivativeFailure,
  knowledgeMatchesWorkflowFilter,
  latestSpanGroupDetailStatus,
  latestSpanGroupStatus,
  resolveCoreIndexDetailStatus,
  resolveKnowledgeDetailStatus,
  resolveKnowledgeWorkflowFeatures,
  resolveKnowledgeWorkflowStatus,
} from './status.ts'

test('resolves every user-visible workflow state', () => {
  const cases = [
    [{ parse_status: 'pending' }, 'pending'],
    [{ parse_status: 'processing' }, 'processing'],
    [{ parse_status: 'finalizing' }, 'processing'],
    [{ parse_status: 'cancelling' }, 'cancelling'],
    [{ parse_status: 'deleting' }, 'deleting'],
    [{ parse_status: 'cancelled' }, 'cancelled'],
    [{ parse_status: 'draft' }, 'draft'],
    [{
      parse_status: 'completed',
      summary_status: 'completed',
      enrichment_status: 'completed',
      wiki_status: 'processing',
    }, 'processing'],
    [{
      parse_status: 'completed',
      summary_status: 'completed',
      enrichment_status: 'degraded',
      wiki_status: 'completed',
    }, 'failed'],
    [{
      parse_status: 'completed',
      summary_status: 'completed',
      enrichment_status: 'unexpected',
      wiki_status: 'completed',
    }, 'failed'],
    [{
      parse_status: 'completed',
      summary_status: 'skipped',
      enrichment_status: 'skipped',
      wiki_status: 'skipped',
    }, 'completed'],
    [{
      parse_status: 'completed',
      summary_status: 'completed',
      enrichment_status: 'completed',
      wiki_status: 'completed',
    }, 'completed'],
  ] as const
  for (const [item, expected] of cases) {
    assert.equal(resolveKnowledgeWorkflowStatus(item), expected)
  }
})

test('keeps a mixed active/failed derivative workflow processing', () => {
  const item = {
    parse_status: 'completed',
    summary_status: 'failed',
    enrichment_status: 'completed',
    wiki_status: 'processing',
  }
  assert.equal(knowledgeHasDerivativeFailure(item), false)
  assert.equal(resolveKnowledgeWorkflowStatus(item), 'processing')
})

test('keeps status-filter membership mutually exclusive during live transitions', () => {
  const pending = {
    parse_status: 'pending',
    summary_status: 'none',
    enrichment_status: 'none',
    wiki_status: 'none',
  }
  assert.equal(knowledgeMatchesWorkflowFilter(pending, ''), true)
  assert.equal(knowledgeMatchesWorkflowFilter(pending, 'pending'), true)
  assert.equal(knowledgeMatchesWorkflowFilter(pending, 'failed'), false)

  const mixedActiveFailure = {
    parse_status: 'completed',
    summary_status: 'failed',
    enrichment_status: 'completed',
    wiki_status: 'processing',
  }
  assert.equal(knowledgeMatchesWorkflowFilter(mixedActiveFailure, 'processing'), true)
  assert.equal(knowledgeMatchesWorkflowFilter(mixedActiveFailure, 'failed'), false)
})

test('uses the latest retry for each logical derivative span', () => {
  const status = latestSpanGroupStatus({
    name: 'root',
    children: [
      {
        name: 'postprocess.question',
        status: 'failed',
        finished_at: '2026-07-25T01:00:00Z',
      },
      {
        name: 'postprocess.question',
        status: 'done',
        finished_at: '2026-07-25T01:01:00Z',
      },
    ],
  }, (name) => name.startsWith('postprocess.question'))
  assert.equal(status, 'completed')
})

test('does not render a pre-fanout none status as skipped', () => {
  assert.equal(
    resolveKnowledgeDetailStatus('none', 'pending', true),
    'not_started',
  )
  assert.equal(
    resolveKnowledgeDetailStatus('none', 'processing', true),
    'not_started',
  )
  assert.equal(
    resolveKnowledgeDetailStatus('none', 'completed', false),
    'disabled',
  )
  assert.equal(
    resolveKnowledgeDetailStatus('none', 'completed', true),
    'not_applicable',
  )
  assert.equal(
    resolveKnowledgeDetailStatus('skipped', 'completed', true),
    'skipped',
  )
  assert.equal(
    resolveKnowledgeDetailStatus('none', 'failed', true),
    'blocked',
  )
})

test('derives effective per-document workflow feature switches', () => {
  const source = {
    summary_model_id: 'chat',
    question_generation_config: { enabled: true },
    extract_config: { enabled: true },
    vlm_config: { enabled: true, model_id: 'vlm' },
    asr_config: { enabled: true, model_id: 'asr' },
    indexing_strategy: {
      vector_enabled: true,
      keyword_enabled: false,
      graph_enabled: true,
      wiki_enabled: true,
    },
  }
  assert.deepEqual(resolveKnowledgeWorkflowFeatures(source), {
    embedding: true,
    multimodal: true,
    summary: true,
    question: true,
    graph: true,
    wiki: true,
  })
  assert.deepEqual(
    resolveKnowledgeWorkflowFeatures(source, {
      metadata: {
        process_overrides: {
          enable_multimodel: false,
          question_generation_config: { enabled: false },
          graph_enabled: false,
        },
      },
    }),
    {
      embedding: true,
      multimodal: true,
      summary: true,
      question: false,
      graph: false,
      wiki: true,
    },
  )

  assert.equal(
    resolveKnowledgeWorkflowFeatures({
      summary_model_id: '',
    }).embedding,
    true,
  )
  assert.equal(resolveCoreIndexDetailStatus('completed', true), 'completed')
  assert.equal(resolveCoreIndexDetailStatus('processing', true), 'processing')
  assert.equal(resolveCoreIndexDetailStatus('completed', false), 'disabled')
})

test('preserves an explicit backend skip in detail without hiding failures', () => {
  assert.equal(
    latestSpanGroupDetailStatus({
      name: 'root',
      children: [{
        name: 'postprocess.wiki',
        status: 'skipped',
        output: { skipped: 'insufficient_text_content' },
      }],
    }, (name) => name === 'postprocess.wiki'),
    'skipped',
  )
  assert.equal(
    latestSpanGroupDetailStatus({
      name: 'root',
      children: [{
        name: 'postprocess.graph.batch[0]',
        status: 'failed',
        output: { skipped: '' },
      }],
    }, (name) => name.startsWith('postprocess.graph')),
    'failed',
  )
})
