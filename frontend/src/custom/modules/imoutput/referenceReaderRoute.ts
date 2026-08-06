const MOBILE_REFERENCE_UA = /android|iphone|ipad|ipod|windows phone|harmonyos|openharmony|\bmobile\b/i;

export function shouldUseMobileReferenceReader(userAgent: string, mobileHint = false): boolean {
  return mobileHint || MOBILE_REFERENCE_UA.test(String(userAgent || ""));
}

export function buildMobileReferenceReaderURL(origin: string, token: string, view?: string): string {
  const target = new URL("/mobile/reference", origin);
  target.searchParams.set("token", token);
  if (view === "original") target.searchParams.set("view", "original");
  return target.toString();
}
