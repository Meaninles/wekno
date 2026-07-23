export function normalizeKnowledgeIds(ids: readonly string[]): string[] {
  return [...new Set(ids.map((id) => String(id || '').trim()).filter(Boolean))]
}

export function splitKnowledgeIds(ids: readonly string[], maxPerRequest = 100): string[][] {
  const normalized = normalizeKnowledgeIds(ids)
  const size = Number.isFinite(maxPerRequest) ? Math.max(1, Math.trunc(maxPerRequest)) : 100
  const batches: string[][] = []
  for (let offset = 0; offset < normalized.length; offset += size) {
    batches.push(normalized.slice(offset, offset + size))
  }
  return batches
}
