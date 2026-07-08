import { get } from '@/utils/request'

export interface IAMSpaceMemberOrganization {
  external_id: string
  name: string
  parent_external_id?: string
  has_children?: boolean
  user_count: number
  tenant_count: number
  node_type?: 'organization' | 'user'
  iam_external_id?: string
  user_id?: string
  username?: string
  display_name?: string
  avatar?: string
  tenant_id?: number
  tenant_name?: string
  has_local_user?: boolean
  access_enabled?: boolean
  already_selected?: boolean
  selection_disabled?: boolean
}

export interface IAMSpaceMemberCandidate {
  iam_external_id: string
  user_id: string
  username: string
  display_name?: string
  avatar?: string
  tenant_id: number
  tenant_name?: string
  iam_organization_external_id?: string
  iam_organization_name?: string
  has_local_user?: boolean
  access_enabled?: boolean
  already_selected?: boolean
  selection_disabled?: boolean
}

export function listIAMSpaceMemberOrganizations(params: {
  spaceId: string
  parentId?: string
  query?: string
  limit?: number
  includeUsers?: boolean
}): Promise<{ success: boolean; data: IAMSpaceMemberOrganization[]; message?: string }> {
  const search = new URLSearchParams()
  search.set('space_id', params.spaceId)
  if (typeof params.parentId === 'string') search.set('parent_id', params.parentId)
  if (params.query) search.set('q', params.query)
  if (params.includeUsers) search.set('include_users', 'true')
  search.set('limit', String(params.limit ?? 100))
  return get(`/api/v1/custom/iam/space-member-organizations?${search.toString()}`) as unknown as Promise<{ success: boolean; data: IAMSpaceMemberOrganization[]; message?: string }>
}

export function listIAMSpaceMemberCandidates(params: {
  spaceId: string
  query?: string
  iamOrgIds?: string[]
  direct?: boolean
  limit?: number
}): Promise<{ success: boolean; data: IAMSpaceMemberCandidate[]; message?: string }> {
  const search = new URLSearchParams()
  search.set('space_id', params.spaceId)
  if (params.query) search.set('q', params.query)
  if (params.iamOrgIds?.length) search.set('iam_org_ids', params.iamOrgIds.join(','))
  if (params.direct) search.set('direct', 'true')
  search.set('limit', String(params.limit ?? 50))
  return get(`/api/v1/custom/iam/space-member-candidates?${search.toString()}`) as unknown as Promise<{ success: boolean; data: IAMSpaceMemberCandidate[]; message?: string }>
}
