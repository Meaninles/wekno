import type { RouteLocationRaw, Router } from 'vue-router'

export type OpenWindow = (
  url?: string | URL,
  target?: string,
  features?: string,
) => Window | null

// Resolve through Vue Router so the correct desktop/mobile base is retained,
// while leaving the current search page and all of its in-memory state intact.
export function openRouteInNewPage(
  router: Router,
  location: RouteLocationRaw,
  openWindow: OpenWindow = (...args) => window.open(...args),
) {
  const href = router.resolve(location).href
  openWindow(href, '_blank', 'noopener,noreferrer')
  return href
}
