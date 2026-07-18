import assert from 'node:assert/strict';
import test from 'node:test';

import { pickDefaultRerankModelID } from './config.ts';

test('pickDefaultRerankModelID prefers the tenant default rerank model', () => {
  assert.equal(pickDefaultRerankModelID([
    { id: 'chat', type: 'KnowledgeQA' },
    { id: 'rerank-first', type: 'Rerank' },
    { id: 'rerank-default', type: 'Rerank', is_default: true },
  ]), 'rerank-default');
});

test('pickDefaultRerankModelID falls back to the first rerank model', () => {
  assert.equal(pickDefaultRerankModelID([
    { id: 'chat', type: 'KnowledgeQA' },
    { id: 'rerank-first', type: 'Rerank' },
  ]), 'rerank-first');
});

test('pickDefaultRerankModelID returns empty when no rerank model exists', () => {
  assert.equal(pickDefaultRerankModelID([
    { id: 'chat', type: 'KnowledgeQA' },
  ]), '');
});
