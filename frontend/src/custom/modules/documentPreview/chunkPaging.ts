export const DOCUMENT_CHUNK_PAGE_SIZE = 25;

function toSafeInteger(value: unknown): number {
  const parsed = Number(value);
  if (!Number.isFinite(parsed) || parsed <= 0) return 0;
  return Math.floor(parsed);
}

export function documentChunkPageCount(
  total: unknown,
  pageSize = DOCUMENT_CHUNK_PAGE_SIZE,
): number {
  const normalizedTotal = toSafeInteger(total);
  const normalizedPageSize = toSafeInteger(pageSize) || DOCUMENT_CHUNK_PAGE_SIZE;
  return normalizedTotal > 0 ? Math.ceil(normalizedTotal / normalizedPageSize) : 0;
}

export function clampDocumentChunkPage(
  page: unknown,
  total: unknown,
  pageSize = DOCUMENT_CHUNK_PAGE_SIZE,
): number {
  const pageCount = documentChunkPageCount(total, pageSize);
  if (pageCount === 0) return 1;
  const normalizedPage = toSafeInteger(page) || 1;
  return Math.min(pageCount, Math.max(1, normalizedPage));
}

export function sliceDocumentChunkPage<T>(
  chunks: readonly T[] | null | undefined,
  page: unknown,
  pageSize = DOCUMENT_CHUNK_PAGE_SIZE,
): T[] {
  if (!Array.isArray(chunks) || chunks.length === 0) return [];
  const normalizedPageSize = toSafeInteger(pageSize) || DOCUMENT_CHUNK_PAGE_SIZE;
  const normalizedPage = Math.max(1, toSafeInteger(page) || 1);
  const start = (normalizedPage - 1) * normalizedPageSize;
  return chunks.slice(start, start + normalizedPageSize);
}

export function shouldUsePagedChunkView(
  total: unknown,
  pageSize = DOCUMENT_CHUNK_PAGE_SIZE,
): boolean {
  return toSafeInteger(total) > (toSafeInteger(pageSize) || DOCUMENT_CHUNK_PAGE_SIZE);
}

export function nextDocumentChunkFetchPage(
  loaded: unknown,
  total: unknown,
  pageSize = DOCUMENT_CHUNK_PAGE_SIZE,
): number | null {
  const normalizedLoaded = toSafeInteger(loaded);
  const normalizedTotal = toSafeInteger(total);
  const normalizedPageSize = toSafeInteger(pageSize) || DOCUMENT_CHUNK_PAGE_SIZE;
  if (normalizedTotal === 0 || normalizedLoaded >= normalizedTotal) return null;
  return Math.floor(normalizedLoaded / normalizedPageSize) + 1;
}

export function appendUniqueDocumentChunks<T extends { id?: unknown }>(
  current: readonly T[] | null | undefined,
  incoming: readonly T[] | null | undefined,
): T[] {
  const merged = Array.isArray(current) ? [...current] : [];
  if (!Array.isArray(incoming) || incoming.length === 0) return merged;

  const knownIDs = new Set(
    merged
      .map((chunk) => String(chunk?.id ?? ''))
      .filter(Boolean),
  );
  for (const chunk of incoming) {
    const id = String(chunk?.id ?? '');
    if (id && knownIDs.has(id)) continue;
    merged.push(chunk);
    if (id) knownIDs.add(id);
  }
  return merged;
}
