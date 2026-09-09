import { onScopeDispose, ref } from 'vue'
import { del, get, post, postUpload } from '@/utils/request'
import { draftKey, embedDraftScope, flushDraft, uploadBinding } from '../sessionState/storage'

import { CHAT_UPLOAD_MAX_MB, CHAT_UPLOAD_MAX_BYTES } from './limits'
export { CHAT_UPLOAD_MAX_MB, CHAT_UPLOAD_MAX_BYTES } from './limits'

// Never decode an unprocessed original just to draw a composer thumbnail. A
// compressed image may have a small byte size but an enormous pixel surface.
export function chatImagePlaceholder(): string {
  return 'data:image/svg+xml,' + encodeURIComponent('<svg xmlns="http://www.w3.org/2000/svg" width="80" height="80" viewBox="0 0 80 80"><rect width="80" height="80" rx="8" fill="#edf1f5"/><path d="M20 20h40v40H20zM20 53l13-13 10 9 7-6 10 10" fill="none" stroke="#607080" stroke-width="3"/><circle cx="48" cy="32" r="4" fill="#607080"/></svg>')
}

type Source = { id: string; upload_ids: string[]; input_file_ids?: string[]; input_file_id?: string; file_name: string; ready: boolean; parse_status: string; core_status: string; original_input?: boolean; error?: string }
export type UploadRow = { key: string; name: string; state: 'queued' | 'uploading' | 'processing' | 'ready' | 'failed' | 'cancelled'; error?: string; source?: Source }
type Context = { sessionId: string; agentId?: string; channelId?: string; token?: string; sessionSig?: string; visitorId?: string; directInput?: boolean }
type Reply<T> = { success: boolean; data: T }
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

export function useChatUploads() {
  const rows = ref<UploadRow[]>([])
  const preparing = ref(false)
  let controller: AbortController | undefined
  let cancelAccepted: (() => Promise<void>) | undefined
  const retries = new Map<string, () => void>()
  const retry = (key: string) => retries.get(key)?.()
  // Leaving a conversation detaches the waiter; durable processing and its
  // source identity remain available when the draft is reopened.
  const detach = () => controller?.abort()
  const cancel = async () => {
    controller?.abort()
    await cancelAccepted?.()
  }
  onScopeDispose(detach)

  async function prepare(files: File[], ctx: Context): Promise<{ uploadIds: string[]; inputFileIds: string[] }> {
    if (!files.length) return { uploadIds: [], inputFileIds: [] }
    if (preparing.value) throw new Error('文件仍在处理中')
    if (!ctx.sessionId) throw new Error('请先创建对话')
    for (const file of files) {
      if (!file.size || file.size > CHAT_UPLOAD_MAX_BYTES) throw new Error(chatUploadLimitMessage(file.name))
    }
    const uniqueFiles = [...new Set(files)]
    const endpoint = ctx.channelId
      ? `/api/v1/embed/${encodeURIComponent(ctx.channelId)}/chat-uploads/sessions/${encodeURIComponent(ctx.sessionId)}`
      : `/api/v1/custom/chat-uploads/sessions/${encodeURIComponent(ctx.sessionId)}`
    const headers = ctx.token ? { Authorization: `Embed ${ctx.token}`, 'X-Embed-Session': ctx.sessionSig || '', 'X-Embed-Visitor': ctx.visitorId || '' } : undefined
    const active = new AbortController()
    controller = active
    const config = { headers, signal: active.signal }
    const key = draftKey(ctx.sessionId, ctx.channelId ? embedDraftScope(ctx.channelId, ctx.visitorId || '') : undefined)
    if (!key) throw new Error('无法确认附件所属会话，请重新登录')
    rows.value = uniqueFiles.map((file, i) => ({ key: String(i), name: file.name, state: 'queued' }))
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
      const row = rows.value[index], binding = bound[index], file = uniqueFiles[index]
      while (!active.signal.aborted) {
        try {
          row.error = undefined
          row.state = 'processing'
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
          await uploadBinding(key, file, endpoint, source.id)
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
      await flushDraft(key)
      bound = await Promise.all(uniqueFiles.map(file => uploadBinding(key, file, endpoint)))
      if (active.signal.aborted) throw cancelled()
      rows.value.forEach((row, i) => { row.key = bound[i].uploadId })
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
  return { rows, preparing, prepare, retry, cancel, detach }
}
