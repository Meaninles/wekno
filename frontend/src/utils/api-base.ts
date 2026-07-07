export function getApiBaseUrl(): string {
  // LocalHub plugin patch (2026-04-29): respect vite's BASE_URL so that
  // axios calls work at `/app/weknora/` (LocalHub reverse proxy). Without
  // this · axios hits `/api/v1/...` at LocalHub root · gets 404 "Cannot
  // POST". Strip trailing slash so axios doesn't produce `/app/weknora//api/v1/...`.
  // See: plugins/weknora/patches/api-base-baseurl.patch
  const base = (import.meta.env.BASE_URL || '/').replace(/\/+$/, '');
  // Mobile is mounted under /mobile/ only for SPA assets and history routes.
  // API calls must still go to the shared backend at /api/ on the same host.
  if (base === '/mobile') {
    return '';
  }
  return base;
}
