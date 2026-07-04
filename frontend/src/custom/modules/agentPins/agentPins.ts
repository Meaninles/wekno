import { computed, ref } from 'vue'
import { readUserId, safeGetItem, safeSetItem } from '@/composables/preferenceStorage'
import { useAuthStore } from '@/stores/auth'

// Chat agent pins are a per-user UI preference. They intentionally do not
// write to agent config, tenant KV, or any shared backend setting.
const CHAT_AGENT_PINS_SUFFIX = 'chat_agent_pins'

const revision = ref(0)

function tenantSegmentForKey(): string {
  try {
    const authStore = useAuthStore()
    const tenantId = authStore.effectiveTenantId
    return tenantId ? `t${tenantId}_` : ''
  } catch {
    return ''
  }
}

function storageKey(): string {
  return `WeKnora_${readUserId()}_${tenantSegmentForKey()}${CHAT_AGENT_PINS_SUFFIX}`
}

export function agentPinKey(agentId: string, sourceTenantId?: string | null): string {
  const id = String(agentId || '').trim()
  const source = String(sourceTenantId || '').trim()
  if (!id) return ''
  return source ? `shared:${source}:${id}` : `local:${id}`
}

function normalizePinnedKeys(value: unknown): string[] {
  if (!Array.isArray(value)) return []
  const seen = new Set<string>()
  const out: string[] = []
  for (const item of value) {
    const key = String(item || '').trim()
    if (!key || seen.has(key)) continue
    seen.add(key)
    out.push(key)
  }
  return out
}

function readPinnedKeys(): string[] {
  const raw = safeGetItem(storageKey())
  if (!raw) return []
  try {
    return normalizePinnedKeys(JSON.parse(raw))
  } catch {
    return []
  }
}

function writePinnedKeys(keys: string[]): void {
  safeSetItem(storageKey(), JSON.stringify(normalizePinnedKeys(keys)))
  revision.value++
}

export function stablePinnedFirst<T>(
  items: readonly T[],
  pinnedKeys: ReadonlySet<string>,
  keyOf: (item: T) => string,
): T[] {
  return items
    .map((item, index) => ({
      item,
      index,
      pinned: pinnedKeys.has(keyOf(item)),
    }))
    .sort((a, b) => {
      if (a.pinned !== b.pinned) return a.pinned ? -1 : 1
      return a.index - b.index
    })
    .map(({ item }) => item)
}

if (typeof window !== 'undefined') {
  window.addEventListener('storage', (event) => {
    if (event.key?.endsWith(`_${CHAT_AGENT_PINS_SUFFIX}`)) {
      revision.value++
    }
  })
}

export function useChatAgentPins() {
  const pinnedKeys = computed(() => {
    // eslint-disable-next-line @typescript-eslint/no-unused-expressions
    revision.value
    return new Set(readPinnedKeys())
  })

  const isPinned = (agentId: string, sourceTenantId?: string | null): boolean => {
    return pinnedKeys.value.has(agentPinKey(agentId, sourceTenantId))
  }

  const togglePinned = (agentId: string, sourceTenantId?: string | null): boolean => {
    const key = agentPinKey(agentId, sourceTenantId)
    if (!key) return false
    const keys = readPinnedKeys()
    const next = new Set(keys)
    if (next.has(key)) {
      next.delete(key)
      writePinnedKeys([...next])
      return false
    }
    next.add(key)
    writePinnedKeys([...next])
    return true
  }

  const sortPinnedFirst = <T>(items: readonly T[], keyOf: (item: T) => string): T[] =>
    stablePinnedFirst(items, pinnedKeys.value, keyOf)

  return {
    pinnedKeys,
    isPinned,
    togglePinned,
    sortPinnedFirst,
  }
}
