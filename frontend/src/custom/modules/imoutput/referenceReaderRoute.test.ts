import assert from "node:assert/strict";
import test from "node:test";

import { buildMobileReferenceReaderURL, shouldUseMobileReferenceReader } from "./referenceReaderRoute.ts";

test("same-origin IM reader moves device selection behind the initial direct link", () => {
  assert.equal(shouldUseMobileReferenceReader("Mozilla/5.0 (Windows NT 10.0) wxwork/4.1"), false);
  assert.equal(shouldUseMobileReferenceReader("Mozilla/5.0 (Android 15) Mobile wxwork/4.1"), true);
  assert.equal(shouldUseMobileReferenceReader("desktop", true), true);

  const target = new URL(buildMobileReferenceReaderURL("https://knora.example.com", "opaque.token", "original"));
  assert.equal(target.origin, "https://knora.example.com");
  assert.equal(target.pathname, "/mobile/reference");
  assert.deepEqual(Object.fromEntries(target.searchParams), { token: "opaque.token", view: "original" });
});
