import assert from "node:assert/strict";
import test from "node:test";

import {
  iamSSOEntryForReturnPath,
  isWeComUserAgent,
  normalizeAuthReturnPath,
} from "./authReturn.ts";

const origin = "https://knora.example.com";

test("citation auth return keeps exact document and Wiki coordinates", () => {
  assert.equal(
    normalizeAuthReturnPath(
      "/platform/knowledge-bases/kb-1?knowledge_id=doc-1&chunk_id=chunk-1",
      origin,
    ),
    "/platform/knowledge-bases/kb-1?knowledge_id=doc-1&chunk_id=chunk-1",
  );
  assert.equal(
    normalizeAuthReturnPath("/platform/knowledge-bases/kb-1?tab=graph&slug=ops%2Frunbook", origin),
    "/platform/knowledge-bases/kb-1?tab=graph&slug=ops%2Frunbook",
  );
});

test("auth return rejects cross-origin and non-product redirects", () => {
  assert.equal(normalizeAuthReturnPath("https://evil.example/steal", origin), "");
  assert.equal(normalizeAuthReturnPath("//evil.example/steal", origin), "");
  assert.equal(normalizeAuthReturnPath("/api/v1/users", origin), "");
});

test("WeCom user agent uses IAM entry with encoded deep link", () => {
  assert.equal(isWeComUserAgent("Mozilla/5.0 wxwork/4.1.31 MicroMessenger/7.0"), true);
  assert.equal(isWeComUserAgent("Mozilla/5.0 Chrome/126"), false);
  const entry = iamSSOEntryForReturnPath(
    "/platform/knowledge-bases/kb-1?knowledge_id=doc-1&chunk_id=chunk-1",
    origin,
  );
  const parsed = new URL(entry, origin);
  assert.equal(parsed.pathname, "/api/v1/custom/iam/sso/entry");
  assert.equal(
    parsed.searchParams.get("return_to"),
    "/platform/knowledge-bases/kb-1?knowledge_id=doc-1&chunk_id=chunk-1",
  );
});
