import { computed, onBeforeUnmount, shallowRef, toValue, watch, type MaybeRefOrGetter } from 'vue'
import { getDocumentQueueStatus } from './api'
import { normalizeKnowledgeIds } from './ids'
import type { DocumentQueueItemStatus } from './types'

const DEFAULT_POLL_INTERVAL_MS = 2_000

/**
 * A single page-level poller shared by grid and list views. Requests are
 * serialized and generation-fenced so navigation or filter changes cannot
 * paint a late response onto a different set of cards.
 */
export function useDocumentQueueStatus(
  knowledgeIds: MaybeRefOrGetter<readonly string[]>,
  enabled: MaybeRefOrGetter<boolean>,
  pollIntervalMs = DEFAULT_POLL_INTERVAL_MS,
) {
  const waitingTotal = shallowRef(0)
  const itemsById = shallowRef<Record<string, DocumentQueueItemStatus>>({})
  const idsKey = computed(() => normalizeKnowledgeIds(toValue(knowledgeIds)).join(','))
  const pollingEnabled = computed(() => Boolean(toValue(enabled)))

  let timer: ReturnType<typeof setTimeout> | null = null
  let generation = 0
  let stopped = false

  const clearTimer = () => {
    if (timer !== null) {
      clearTimeout(timer)
      timer = null
    }
  }

  const clearSnapshot = () => {
    waitingTotal.value = 0
    itemsById.value = {}
  }

  const schedule = (requestGeneration: number) => {
    clearTimer()
    if (stopped || !pollingEnabled.value || requestGeneration !== generation) return
    timer = setTimeout(() => { void refresh() }, Math.max(500, pollIntervalMs))
  }

  const refresh = async () => {
    clearTimer()
    const ids = idsKey.value ? idsKey.value.split(',') : []
    if (!pollingEnabled.value || ids.length === 0 || stopped) {
      clearSnapshot()
      return
    }

    const requestGeneration = generation
    try {
      const snapshot = await getDocumentQueueStatus(ids)
      if (stopped || requestGeneration !== generation) return
      waitingTotal.value = snapshot.waiting_total
      itemsById.value = snapshot.items
    } catch {
      // Preserve the last good snapshot across a transient API failure. A
      // subsequent poll will reconcile it, while the document lifecycle
      // status independently hides stale badges after completion/cancel.
    } finally {
      schedule(requestGeneration)
    }
  }

  watch(
    [idsKey, pollingEnabled],
    ([key, isEnabled]) => {
      generation += 1
      clearTimer()
      if (!key || !isEnabled) {
        clearSnapshot()
        return
      }
      void refresh()
    },
    { immediate: true },
  )

  onBeforeUnmount(() => {
    stopped = true
    generation += 1
    clearTimer()
  })

  return {
    waitingTotal,
    itemsById,
    refresh,
  }
}
