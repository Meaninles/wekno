import { get, put } from '@/utils/request'

export interface WikiAccessUser {
  id: string
  username: string
  display_name?: string
  tenant_id: number
  tenant_name?: string
  is_active: boolean
  is_system_admin: boolean
  wiki_enabled: boolean
}

export interface WikiAccessUserPage {
  users: WikiAccessUser[]
  total: number
  page: number
  page_size: number
}

type ApiResponse<T> = {
  success: boolean
  data: T
  message?: string
}

export function getCurrentWikiAccess(): Promise<ApiResponse<{ wiki_enabled: boolean }>> {
  return get('/api/v1/custom/wiki-access/me') as unknown as Promise<ApiResponse<{ wiki_enabled: boolean }>>
}

export function listWikiAccessUsers(
  query = '',
  page = 1,
  pageSize = 20,
): Promise<ApiResponse<WikiAccessUserPage>> {
  const search = new URLSearchParams({
    page: String(page),
    page_size: String(pageSize),
  })
  if (query.trim()) search.set('q', query.trim())
  return get(`/api/v1/custom/wiki-access/users?${search.toString()}`) as unknown as Promise<ApiResponse<WikiAccessUserPage>>
}

export function setWikiAccessUser(userId: string, wikiEnabled: boolean): Promise<ApiResponse<WikiAccessUser>> {
  return put(`/api/v1/custom/wiki-access/users/${encodeURIComponent(userId)}`, {
    wiki_enabled: wikiEnabled,
  }) as unknown as Promise<ApiResponse<WikiAccessUser>>
}
