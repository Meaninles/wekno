type Title = { session_id: string; title: string }
type Options = {
  key: string
  read: () => Promise<Title>
  repair: () => Promise<Title>
  authorized: () => boolean
  publish: (title: Title) => void
  delays?: number[]
}

const pending = new Map<string, { promise: Promise<void>; subscribers: Set<Options> }>()

// The answer stream never waits for naming. Only unfinished titles are read,
// with one shared, bounded waiter per session. Recovery runs once, after the
// server's normal 20s generation + 5s persistence deadline has elapsed.
export function synchronizeTitle(options: Options): Promise<void> {
  const existing = pending.get(options.key)
  if (existing) {
    existing.subscribers.add(options)
    return existing.promise
  }
  const subscribers = new Set([options])
  const authorized = () => [...subscribers].find(value => value.authorized())
  const work = async () => {
    const publish = (value: Title) => {
      if (!value?.title?.trim() || !authorized()) return false
      const callbacks = new Set<Options['publish']>()
      for (const subscriber of subscribers) {
        if (subscriber.authorized()) callbacks.add(subscriber.publish)
      }
      for (const callback of callbacks) callback(value)
      return true
    }
    for (const delay of options.delays ?? [0, 1000, 2000, 4000, 8000, 8000, 8000]) {
      if (delay) await new Promise(resolve => setTimeout(resolve, delay))
      const transport = authorized()
      if (!transport) return
      try {
        if (publish(await transport.read())) return
      } catch (error: any) {
        if ([401, 403, 404].includes(error?.status)) return
      }
    }
    const transport = authorized()
    if (!transport) return
    try { publish(await transport.repair()) } catch { /* A future visit can retry. */ }
  }
  const promise = work().finally(() => pending.delete(options.key))
  pending.set(options.key, { promise, subscribers })
  return promise
}
