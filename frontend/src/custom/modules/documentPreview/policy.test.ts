import assert from 'node:assert/strict'
import test from 'node:test'

import {
  MAX_COMPLEX_DOCUMENT_CHUNKS,
  MAX_TEXT_RENDER_BYTES,
  blobExceedsAdmission,
  boundedTextBlob,
  evaluatePreviewAdmission,
  unwrapKnowledgePreviewPolicy,
} from './policy.ts'

test('server paged policy cannot be overridden by client file metadata', () => {
  const policy = unwrapKnowledgePreviewPolicy({
    data: {
      mode: 'paged_chunks',
      reason: 'heavy_structure',
      file_type: 'docx',
      file_size: 1024,
      chunk_count: 1,
      max_original_bytes: 4 * 1024 * 1024,
    },
  })
  assert.equal(
    evaluatePreviewAdmission(policy, {
      fileType: 'docx',
      fileSize: 1024,
      chunkCount: 1,
    }).mode,
    'paged_chunks',
  )
})

test('complex documents with too many chunks use bounded preview', () => {
  const admission = evaluatePreviewAdmission(
    {
      mode: 'original',
      file_type: 'xlsx',
      file_size: 1024,
      chunk_count: MAX_COMPLEX_DOCUMENT_CHUNKS + 1,
      max_original_bytes: 2 * 1024 * 1024,
    },
    { chunkCount: MAX_COMPLEX_DOCUMENT_CHUNKS + 1 },
  )
  assert.equal(admission.mode, 'paged_chunks')
  assert.equal(admission.reason, 'too_many_chunks')
})

test('runtime blob and text rendering are bounded', async () => {
  const large = new Blob([new Uint8Array(MAX_TEXT_RENDER_BYTES + 16)])
  assert.equal(blobExceedsAdmission(large, MAX_TEXT_RENDER_BYTES), true)
  const bounded = boundedTextBlob(large)
  assert.equal(bounded.truncated, true)
  assert.equal(bounded.blob.size, MAX_TEXT_RENDER_BYTES)
})
