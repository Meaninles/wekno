import { get } from '@/utils/request'

export interface IAMSpaceMemberOrganization {
  external_id: string
  name: string
  parent_external_id?: string
  user_count: number
  tenant_count: number
}

export interface IAMSpaceMemberCandidate {
  user_id: string
  username: string
  display_name?: string
  avatar?: string
  tenant_id: number
  tenant_name?: string
  iam_organization_external_id?: string
  iam_organization_name?: string
}

export function listIAMSpaceMemberOrganizations(
  spaceId: string,
  limit = 1000,
): Promise<{ success: boolean; data: IAMSpaceMemberOrganization[]; message?: string }> {
  return get(`/api/v1/custom/iam/space-member-organizations?space_id=${encodeURIComponent(spaceId)}&limit=${limit}`) as unknown as Promise<{ success: boolean; data: IAMSpaceMemberOrganization[]; message?: string }>
}

export function listIAMSpaceMemberCandidates(params: {
  spaceId: string
  query?: string
  iamOrgIds?: string[]
  limit?: number
}): Promise<{ success: boolean; data: IAMSpaceMemberCandidate[]; message?: string }> {
  const search = new URLSearchParams()
  search.set('space_id', params.spaceId)
  if (params.query) search.set('q', params.query)
  if (params.iamOrgIds?.length) search.set('iam_org_ids', params.iamOrgIds.join(','))
  search.set('limit', String(params.limit ?? 50))
  return get(`/api/v1/custom/iam/space-member-candidates?${search.toString()}`) as unknown as Promise<{ success: boolean; data: IAMSpaceMemberCandidate[]; message?: string }>
}
