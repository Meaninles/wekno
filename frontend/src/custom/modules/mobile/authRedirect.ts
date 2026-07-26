export const MOBILE_SSO_ENTRY = '/api/v1/custom/iam/sso/entry?client=mobile'

export function mobileSSOEntryForPath(pathname: string): string | null {
  const normalized = pathname.trim()
  if (normalized === '/mobile' || normalized.startsWith('/mobile/')) {
    return MOBILE_SSO_ENTRY
  }
  return null
}
