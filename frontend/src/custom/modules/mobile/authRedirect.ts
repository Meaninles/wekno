export const MOBILE_SSO_ENTRY = '/api/v1/custom/iam/sso/entry'

export function mobileReturnPath(pathname: string, search = ''): string | null {
  const normalized = pathname.trim()
  if (normalized !== '/mobile' && !normalized.startsWith('/mobile/')) return null
  const normalizedSearch = search && !search.startsWith('?') ? `?${search}` : search
  return `${normalized}${normalizedSearch}`
}

export function mobileRouterPathForLocation(pathname: string, search = ''): string {
  const returnPath = mobileReturnPath(pathname, search)
  if (!returnPath) return '/chat'
  const routePath = returnPath.slice('/mobile'.length)
  return routePath && routePath !== '/' ? routePath : '/chat'
}

export function mobileSSOEntryForPath(pathname: string, search = ''): string | null {
  const returnTo = mobileReturnPath(pathname, search)
  if (!returnTo) return null
  return `${MOBILE_SSO_ENTRY}?${new URLSearchParams({
    client: 'mobile',
    return_to: returnTo,
  }).toString()}`
}
