export const KNOWLEDGE_DOCUMENT_SEARCH_POLICY = {
  minLength: 2,
  pageSize: 20,
  debounceMs: 360,
} as const

export function normalizeKnowledgeSearchQuery(value: unknown): string {
  return String(value ?? '').trim()
}

export function filterKnowledgeBasesByName<T extends { name?: unknown }>(
  items: readonly T[],
  query: unknown,
): T[] {
  const keyword = normalizeKnowledgeSearchQuery(query).toLocaleLowerCase()
  if (!keyword) return [...items]
  return items.filter((item) => String(item.name ?? '').toLocaleLowerCase().includes(keyword))
}
