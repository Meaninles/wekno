import { get, post } from '@/utils/request'
import { synchronizeTitle } from './sync'

export function synchronizeSessionTitle(sessionId: string, options: {
  prefix?: string
  embedToken?: string
  sessionSig?: string
  visitorId?: string
  active?: () => boolean
  onTitle?: (value: { session_id: string; title: string }) => void
} = {}) {
  const token = options.embedToken || localStorage.getItem('weknora_token')
  if (!token || !sessionId) return Promise.resolve()
  const tenant = localStorage.getItem('weknora_selected_tenant_id')
  const endpoint = `${options.prefix || '/api/v1/custom'}/session-titles/${encodeURIComponent(sessionId)}`
  const headers: Record<string, string> = {
    Authorization: options.embedToken ? `Embed ${token}` : `Bearer ${token}`,
    ...(!options.embedToken && tenant ? { 'X-Tenant-ID': tenant } : {}),
    ...(options.sessionSig ? { 'X-Embed-Session': options.sessionSig } : {}),
    ...(options.visitorId ? { 'X-Embed-Visitor': options.visitorId } : {}),
  }
  return synchronizeTitle({ key: `${endpoint}:${tenant || ''}:${token}:${options.sessionSig || ''}`,
    authorized: () => options.embedToken ? (options.active?.() ?? true) :
      localStorage.getItem('weknora_token') === token && localStorage.getItem('weknora_selected_tenant_id') === tenant,
    read: async () => (await get<any>(endpoint, { headers })).data,
    repair: async () => (await post<any>(endpoint, {}, { headers, timeout: 30000 })).data,
    publish: value => {
      if (!options.embedToken) window.dispatchEvent(new CustomEvent('session-title-updated', {
        detail: { sessionId: value.session_id, title: value.title },
      }))
      if (options.active?.() !== false) options.onTitle?.(value)
    },
  })
}
