import { computed, onScopeDispose, ref, shallowRef } from 'vue'
import { del, get, post, postUpload } from '@/utils/request'
import { draftKey, embedDraftScope, fileId, flushDraft, uploadBinding } from '../sessionState/storage'

import { CHAT_UPLOAD_MAX_MB, CHAT_UPLOAD_MAX_BYTES } from './limits'
export { CHAT_UPLOAD_MAX_MB, CHAT_UPLOAD_MAX_BYTES } from './limits'

// Never decode an unprocessed original just to draw a composer thumbnail. A
// compressed image may have a small byte size but an enormous pixel surface.
export function chatImagePlaceholder(): string {
  return 'data:image/svg+xml,' + encodeURIComponent('<svg xmlns="http://www.w3.org/2000/svg" width="80" height="80" viewBox="0 0 80 80"><rect width="80" height="80" rx="8" fill="#edf1f5"/><path d="M20 20h40v40H20zM20 53l13-13 10 9 7-6 10 10" fill="none" stroke="#607080" stroke-width="3"/><circle cx="48" cy="32" r="4" fill="#607080"/></svg>')
}

type Source = { id: string; upload_ids: string[]; input_file_ids?: string[]; input_file_id?: string; file_name: string; ready: boolean; parse_status: string; core_status: string; original_input?: boolean; error?: string }
export type UploadRow = { key: string; fileId?: string; name: string; state: 'queued' | 'uploading' | 'processing' | 'ready' | 'failed' | 'cancelled'; error?: string; source?: Source }
type Context = { sessionId: string; agentId?: string; channelId?: string; token?: string; sessionSig?: string; visitorId?: string; directInput?: boolean }
type Reply<T> = { success: boolean; data: T }
type PreparedUploads = { uploadIds: string[]; inputFileIds: string[] }
const cancelled = () => new DOMException('已取消文件处理，输入内容已保留', 'AbortError')
export const chatUploadLimitMessage = (name = '文件') =>
  `${name}：文件和图片每个最大 ${CHAT_UPLOAD_MAX_MB} MiB，且不能为空。文件过大时，可先上传到知识库，等待解析成功后，在对话中选择知识库文件进行问答。`
function pause(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal.aborted) return reject(cancelled())
    const abort = () => { clearTimeout(timer); signal.removeEventListener('abort', abort); reject(cancelled()) }
    const timer = setTimeout(() => { signal.removeEventListener('abort', abort); resolve() }, ms)
    signal.addEventListener('abort', abort, { once: true })
  })
}

function createUploadWorker() {
  const rows = ref<UploadRow[]>([])
  const preparing = ref(false)
  let controller: AbortController | undefined
  let cancelAccepted: (() => Promise<void>) | undefined
  const retries = new Map<string, () => void>()
  let activeBatch: { key: string; promise: Promise<PreparedUploads> } | undefined
  let completedBatch: { key: string; result: PreparedUploads } | undefined
  const retry = (key: string) => retries.get(key)?.()
  // Leaving a conversation detaches the waiter; durable processing and its
  // source identity remain available when the draft is reopened.
  const detach = () => {
    controller?.abort()
    activeBatch = undefined
    completedBatch = undefined
  }
  const cancel = async () => {
    controller?.abort()
    completedBatch = undefined
    await cancelAccepted?.()
  }

  const fileIdentity = (file: File) => `${file.name}\u0000${file.size}\u0000${file.lastModified}`
  const batchIdentity = (files: File[], ctx: Context) => JSON.stringify({
    // The composer keeps display order, while senders historically grouped
    // images before documents. The upload batch is a set, so both views must
    // await the same in-flight work.
    files: files.map(fileIdentity).sort(),
    sessionId: ctx.sessionId,
    agentId: ctx.agentId || '',
    channelId: ctx.channelId || '',
    sessionSig: ctx.sessionSig || '',
    visitorId: ctx.visitorId || '',
    directInput: ctx.directInput === true,
  })

  async function runPrepare(uniqueFiles: File[], ctx: Context): Promise<PreparedUploads> {
    if (!ctx.sessionId) throw new Error('请先创建对话')
    for (const file of uniqueFiles) {
      if (!file.size || file.size > CHAT_UPLOAD_MAX_BYTES) throw new Error(chatUploadLimitMessage(file.name))
    }
    const endpoint = ctx.channelId
      ? `/api/v1/embed/${encodeURIComponent(ctx.channelId)}/chat-uploads/sessions/${encodeURIComponent(ctx.sessionId)}`
      : `/api/v1/custom/chat-uploads/sessions/${encodeURIComponent(ctx.sessionId)}`
    // Keep normal parsed uploads and Agent direct originals in separate local
    // bindings. A user can switch modes while the same File object is still
    // present in the composer; reusing the parsed source there would silently
    // drop input_file_ids from the Agent request.
    const bindingEndpoint = `${endpoint}#${ctx.directInput ? 'original' : 'knowledge'}`
    const headers = ctx.token ? { Authorization: `Embed ${ctx.token}`, 'X-Embed-Session': ctx.sessionSig || '', 'X-Embed-Visitor': ctx.visitorId || '' } : undefined
    const active = new AbortController()
    controller = active
    const config = { headers, signal: active.signal }
    const key = draftKey(ctx.sessionId, ctx.channelId ? embedDraftScope(ctx.channelId, ctx.visitorId || '') : undefined)
    if (!key) throw new Error('无法确认附件所属会话，请重新登录')
    rows.value = uniqueFiles.map(file => ({ key: fileId(file), name: file.name, state: 'queued' }))
    preparing.value = true
    let bound: Awaited<ReturnType<typeof uploadBinding>>[] = []
    cancelAccepted = async () => {
      // A dropped upload response can still have committed. Recover by the
      // caller's stable ID, then cancel only this batch's accepted sources.
      const known = new Map<string, boolean>()
      try {
        const res = await get<Reply<Source[]>>(endpoint, { headers })
        for (const source of res.data) {
          if (bound.some(b => source.upload_ids.includes(b.uploadId))) known.set(source.id, !!source.original_input)
        }
      } catch { /* Preserve originals if cancellation cannot reach the server. */ }
      await Promise.allSettled([...known].map(([id, original]) =>
        original
          ? del(`${endpoint}/${id}`, undefined, { headers })
          : post(`${endpoint}/${id}/cancel`, {}, { headers }),
      ))
    }
    const waitForRetry = (row: UploadRow) => new Promise<void>((resolve, reject) => {
      if (active.signal.aborted) return reject(cancelled())
      const abort = () => { retries.delete(row.key); reject(cancelled()) }
      active.signal.addEventListener('abort', abort, { once: true })
      retries.set(row.key, () => { retries.delete(row.key); active.signal.removeEventListener('abort', abort); resolve() })
    })
    async function process(index: number): Promise<Source> {
      const row = rows.value[index], file = uniqueFiles[index]
      while (!active.signal.aborted) {
        try {
          row.error = undefined
          row.state = 'processing'
          await flushDraft(key)
          const binding = bound[index] || await uploadBinding(key, file, bindingEndpoint)
          bound[index] = binding
          row.key = binding.uploadId
          if (active.signal.aborted) throw cancelled()
          let source: Source
          if (binding.sourceId) {
            source = (await get<Reply<Source>>(`${endpoint}/${binding.sourceId}`, config)).data
            if (['failed', 'cancelled'].includes(source.parse_status)) source = (await post<Reply<Source>>(`${endpoint}/${source.id}/retry`, {}, config)).data
          } else {
            row.state = 'uploading'
            const form = new FormData()
            form.append('file', file, file.name)
            form.append('upload_id', binding.uploadId)
            form.append('agent_id', ctx.agentId || '')
            if (ctx.directInput) form.append('mode', 'original')
            let attempts = 0
            while (true) {
              try {
                source = (await postUpload(endpoint, form, undefined, { ...config, timeout: 0 })).data
                break
              } catch (error: any) {
                if (active.signal.aborted) throw cancelled()
                if (error?.status && error.status !== 409 && error.status < 500) throw error
                // Both upload and status recovery use the same identity. A
                // retry never creates another parsing job for an accepted file.
                const recovered = await get<Reply<Source[]>>(endpoint, config).catch(() => null)
                const found = recovered?.data.find(s => s.upload_ids.includes(binding.uploadId))
                if (found) { source = found; break }
                if (++attempts >= 4) throw error
                await pause(2000, active.signal)
              }
            }
          }
          binding.sourceId = source.id
          await uploadBinding(key, file, bindingEndpoint, source.id)
          row.source = source
          row.state = 'processing'
          while (!source.ready) {
            if (['failed', 'cancelled'].includes(source.parse_status) || source.core_status === 'failed') throw new Error(source.error || '文件处理未完成，可以重试')
            await pause(2000, active.signal)
            source = (await get<Reply<Source>>(`${endpoint}/${source.id}`, config)).data
            row.source = source
          }
          row.state = 'ready'
          return source
        } catch (error: any) {
          if (active.signal.aborted) { row.state = 'cancelled'; throw cancelled() }
          row.state = 'failed'
          row.error = error?.message || '文件处理失败'
          await waitForRetry(row)
        }
      }
      throw cancelled()
    }
    try {
      let next = 0
      const results: Source[] = new Array(uniqueFiles.length)
      await Promise.all(Array.from({ length: Math.min(2, uniqueFiles.length) }, async () => {
        while (next < uniqueFiles.length) {
          const index = next++
          results[index] = await process(index)
        }
      }))
      return {
        uploadIds: [...new Set(results.filter(source => !source.original_input).map(source => source.id))],
        inputFileIds: [...new Set(results.filter(source => source.original_input).map(source => source.input_file_id || source.id))],
      }
    } finally {
      retries.clear()
      preparing.value = false
      if (active.signal.aborted) rows.value.forEach(row => { if (row.state !== 'ready') row.state = 'cancelled' })
      controller = undefined
      cancelAccepted = undefined
    }
  }

  function prepare(files: File[], ctx: Context): Promise<PreparedUploads> {
    if (!files.length) return Promise.resolve({ uploadIds: [], inputFileIds: [] })
    const uniqueFiles = [...new Set(files)]
    const key = batchIdentity(uniqueFiles, ctx)
    if (activeBatch) {
      if (activeBatch.key === key) return activeBatch.promise
      return Promise.reject(new Error('文件仍在处理中'))
    }
    if (completedBatch?.key === key) return Promise.resolve(completedBatch.result)

    const promise = runPrepare(uniqueFiles, ctx)
      .then((result) => {
        completedBatch = { key, result }
        return result
      })
      .finally(() => {
        if (activeBatch?.key === key) activeBatch = undefined
      })
    activeBatch = { key, promise }
    return promise
  }
  return { rows, preparing, prepare, retry, cancel, detach }
}

// File identity survives IndexedDB draft restoration. A send awaits exactly its
// selected files, while later selections reuse each file's existing operation.
export function useChatUploads() {
  type Task = { id: string; file: File; worker: ReturnType<typeof createUploadWorker>; promise: Promise<PreparedUploads> }
  const tasks = new Map<string, Task>()
  const selected = shallowRef<Task[]>([])
  let epoch = 0
  let latest: { files: File[]; ctx: Context } | undefined
  let resume: typeof latest
  let active = 0
  const waiters: Array<() => void> = []
  const rows = computed(() => selected.value.flatMap(task => task.worker.rows.value.length
    ? task.worker.rows.value.map(row => ({ ...row, fileId: task.id }))
    : [{ key: task.id, fileId: task.id, name: task.file.name, state: 'queued' as const }]))
  const preparing = computed(() => rows.value.some(row => ['queued', 'uploading', 'processing'].includes(row.state)))
  const detach = () => {
    epoch++
    latest = resume = undefined
    for (const task of tasks.values()) task.worker.detach()
    tasks.clear()
    selected.value = []
  }
  const cancel = async () => {
    const current = [...selected.value]
    detach()
    await Promise.all(current.map(task => task.worker.cancel()))
  }
  const visibilityChanged = () => {
    if (document.hidden) {
      const snapshot = latest
      detach()
      resume = snapshot
    } else if (resume) {
      const snapshot = resume
      resume = undefined
      // Restore upload status, never the cancelled send waiter.
      void prepare(snapshot.files, snapshot.ctx).catch(() => {})
    }
  }
  if (typeof document !== 'undefined') document.addEventListener('visibilitychange', visibilityChanged)
  onScopeDispose(() => {
    detach()
    if (typeof document !== 'undefined') document.removeEventListener('visibilitychange', visibilityChanged)
  })
  const retry = (key: string) => {
    for (const task of selected.value) task.worker.retry(key)
  }
  const prepare = (files: File[], ctx: Context): Promise<PreparedUploads> => {
    latest = { files: [...files], ctx: { ...ctx } }
    const generation = epoch
    const chosen = [...new Set(files)].map(file => {
      const id = fileId(file)
      const key = JSON.stringify([id, ctx.sessionId, ctx.agentId, ctx.channelId, ctx.visitorId, ctx.directInput])
      let task = tasks.get(key)
      if (!task) {
        const worker = createUploadWorker()
        const promise = (async () => {
          await Promise.resolve()
          if (active >= 2) await new Promise<void>(resolve => waiters.push(resolve))
          else active++
          try {
            if (epoch !== generation || tasks.get(key)?.worker !== worker) throw cancelled()
            return await worker.prepare([file], ctx)
          } finally {
            const next = waiters.shift()
            if (next) next()
            else active--
          }
        })()
        task = { id, file, worker, promise }
        tasks.set(key, task)
        void promise.catch(() => { if (tasks.get(key)?.promise === promise) tasks.delete(key) })
      }
      return task
    })
    selected.value = chosen
    for (const [key, task] of tasks) {
      if (!chosen.includes(task)) {
        task.worker.detach()
        tasks.delete(key)
      }
    }
    return Promise.all(chosen.map(task => task.promise)).then(results => {
      if (epoch !== generation) throw cancelled()
      return { uploadIds: [...new Set(results.flatMap(r => r.uploadIds))], inputFileIds: [...new Set(results.flatMap(r => r.inputFileIds))] }
    })
  }
  return { rows, preparing, prepare, retry, cancel, detach }
}
