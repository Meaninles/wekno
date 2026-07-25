import {
  normalizePreviewFileType,
  resolveDocumentPreviewType,
  type DocumentPreviewType,
} from '../../../utils/documentPreview.ts'

export type OriginalPreviewMode = 'original' | 'paged_chunks'

export interface KnowledgePreviewPolicy {
  mode: OriginalPreviewMode
  reason?: string
  file_type: string
  file_size: number
  chunk_count: number
  max_original_bytes: number
}

export interface PreviewAdmission {
  mode: OriginalPreviewMode
  reason: string
  maxOriginalBytes: number
}

export const MAX_TEXT_RENDER_BYTES = 512 * 1024
export const DEFAULT_LOADER_BLOB_LIMIT = 8 * 1024 * 1024
export const MAX_COMPLEX_DOCUMENT_CHUNKS = 240

const chunkBoundedTypes = new Set<DocumentPreviewType>(['docx', 'excel', 'pptx'])

export function unwrapKnowledgePreviewPolicy(payload: any): KnowledgePreviewPolicy {
  const value = payload?.data ?? payload
  return {
    mode: value?.mode === 'original' ? 'original' : 'paged_chunks',
    reason: String(value?.reason || ''),
    file_type: normalizePreviewFileType(value?.file_type),
    file_size: Number(value?.file_size || 0),
    chunk_count: Number(value?.chunk_count ?? -1),
    max_original_bytes: Number(value?.max_original_bytes || 0),
  }
}

export function evaluatePreviewAdmission(
  policy: KnowledgePreviewPolicy,
  options: {
    fileType?: string
    fileSize?: number | string
    chunkCount?: number | string
  } = {},
): PreviewAdmission {
  const fileType = normalizePreviewFileType(options.fileType || policy.file_type)
  const previewType = resolveDocumentPreviewType(fileType)
  const observedSize = Number(options.fileSize || policy.file_size || 0)
  const chunkCount = Number(options.chunkCount || policy.chunk_count || 0)
  const limit = Number(policy.max_original_bytes || 0)

  if (policy.mode !== 'original') {
    return {
      mode: 'paged_chunks',
      reason: policy.reason || 'server_policy',
      maxOriginalBytes: limit,
    }
  }
  if (limit <= 0 || observedSize <= 0 || observedSize > limit) {
    return {
      mode: 'paged_chunks',
      reason: observedSize > limit && limit > 0 ? 'file_too_large' : 'unknown_size',
      maxOriginalBytes: limit,
    }
  }
  if (
    chunkBoundedTypes.has(previewType)
    && chunkCount > MAX_COMPLEX_DOCUMENT_CHUNKS
  ) {
    return {
      mode: 'paged_chunks',
      reason: 'too_many_chunks',
      maxOriginalBytes: limit,
    }
  }
  return { mode: 'original', reason: '', maxOriginalBytes: limit }
}

export function blobExceedsAdmission(
  blob: Blob,
  maxOriginalBytes = DEFAULT_LOADER_BLOB_LIMIT,
): boolean {
  const limit = maxOriginalBytes > 0 ? maxOriginalBytes : DEFAULT_LOADER_BLOB_LIMIT
  return blob.size <= 0 || blob.size > limit
}

export function boundedTextBlob(blob: Blob): {
  blob: Blob
  truncated: boolean
} {
  if (blob.size <= MAX_TEXT_RENDER_BYTES) {
    return { blob, truncated: false }
  }
  return {
    blob: blob.slice(0, MAX_TEXT_RENDER_BYTES, blob.type),
    truncated: true,
  }
}
