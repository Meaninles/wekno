import { computed, ref } from 'vue'
import { readUserId, safeGetItem, safeSetItem } from '@/composables/preferenceStorage'
import { useAuthStore } from '@/stores/auth'
import { stablePinnedFirst } from '@/custom/modules/agentPins/agentPins'

export type SkillPinKind = 'lightweight' | 'professional'

const CHAT_SKILL_PINS_SUFFIX = 'chat_skill_pins'
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
  return `WeKnora_${readUserId()}_${tenantSegmentForKey()}${CHAT_SKILL_PINS_SUFFIX}`
}

function skillPinKey(kind: SkillPinKind, skillName: string): string {
  const name = String(skillName || '').trim()
  return name ? `${kind}:${name}` : ''
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

if (typeof window !== 'undefined') {
  window.addEventListener('storage', (event) => {
    if (event.key?.endsWith(`_${CHAT_SKILL_PINS_SUFFIX}`)) {
      revision.value++
    }
  })
}

export function useChatSkillPins(kind: SkillPinKind) {
  const pinnedKeys = computed(() => {
    // eslint-disable-next-line @typescript-eslint/no-unused-expressions
    revision.value
    return new Set(readPinnedKeys())
  })

  const isPinned = (skillName: string): boolean => pinnedKeys.value.has(skillPinKey(kind, skillName))

  const togglePinned = (skillName: string): boolean => {
    const key = skillPinKey(kind, skillName)
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
    stablePinnedFirst(items, pinnedKeys.value, (item) => skillPinKey(kind, keyOf(item)))

  return {
    pinnedKeys,
    isPinned,
    togglePinned,
    sortPinnedFirst,
  }
}
