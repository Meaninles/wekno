import assert from 'node:assert/strict';
import test from 'node:test';

import {
  DOCUMENT_CHUNK_PAGE_SIZE,
  appendUniqueDocumentChunks,
  clampDocumentChunkPage,
  documentChunkPageCount,
  nextDocumentChunkFetchPage,
  shouldUsePagedChunkView,
  sliceDocumentChunkPage,
} from './chunkPaging.ts';

test('large documents render one bounded page at a time', () => {
  const chunks = Array.from({ length: 266 }, (_, index) => ({ id: `chunk-${index + 1}` }));

  assert.equal(DOCUMENT_CHUNK_PAGE_SIZE, 25);
  assert.equal(documentChunkPageCount(chunks.length), 11);
  assert.equal(sliceDocumentChunkPage(chunks, 1).length, 25);
  assert.deepEqual(
    sliceDocumentChunkPage(chunks, 2).map((chunk) => chunk.id),
    chunks.slice(25, 50).map((chunk) => chunk.id),
  );
  assert.equal(sliceDocumentChunkPage(chunks, 11).length, 16);
  assert.equal(shouldUsePagedChunkView(chunks.length), true);
  assert.equal(shouldUsePagedChunkView(25), false);
});

test('page navigation clamps invalid values and only fetches missing pages', () => {
  assert.equal(clampDocumentChunkPage(-3, 266), 1);
  assert.equal(clampDocumentChunkPage(99, 266), 11);
  assert.equal(nextDocumentChunkFetchPage(25, 266), 2);
  assert.equal(nextDocumentChunkFetchPage(50, 266), 3);
  assert.equal(nextDocumentChunkFetchPage(266, 266), null);
});

test('replayed chunk pages are idempotent', () => {
  const first = [{ id: 'a' }, { id: 'b' }];
  const replayed = [{ id: 'b' }, { id: 'c' }];
  assert.deepEqual(
    appendUniqueDocumentChunks(first, replayed).map((chunk) => chunk.id),
    ['a', 'b', 'c'],
  );
});
