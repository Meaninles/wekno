import assert from "node:assert/strict";
import test from "node:test";

import {
  resolveMobileArtifactDownloadURL,
  resolveMobileDocumentDownloadURL,
  unwrapMobileDocumentDownloadLink,
} from "./documentDownloadLink.ts";

test("unwrapMobileDocumentDownloadLink accepts the API envelope", () => {
  assert.deepEqual(
    unwrapMobileDocumentDownloadLink({
      success: true,
      data: {
        url: "/api/v1/custom/mobile-documents/download?sig=abc",
        expires_at: "2026-07-28T08:02:00Z",
      },
    }),
    {
      url: "/api/v1/custom/mobile-documents/download?sig=abc",
      expires_at: "2026-07-28T08:02:00Z",
    },
  );
});

test("resolveMobileArtifactDownloadURL only allows the artifact capability endpoint", () => {
  assert.equal(
    resolveMobileArtifactDownloadURL(
      "/api/v1/custom/mobile-documents/artifacts/download?sig=abc",
      "https://knora.example/mobile/chat",
    ),
    "https://knora.example/api/v1/custom/mobile-documents/artifacts/download?sig=abc",
  );
  assert.throws(
    () => resolveMobileArtifactDownloadURL(
      "/api/v1/custom/mobile-documents/download?sig=abc",
      "https://knora.example/mobile/chat",
    ),
    /无效的下载链接/,
  );
});

test("resolveMobileDocumentDownloadURL only allows the same-origin signed endpoint", () => {
  assert.equal(
    resolveMobileDocumentDownloadURL(
      "/api/v1/custom/mobile-documents/download?sig=abc",
      "https://knora.example/mobile/settings/knowledge",
    ),
    "https://knora.example/api/v1/custom/mobile-documents/download?sig=abc",
  );
  assert.throws(
    () => resolveMobileDocumentDownloadURL(
      "https://attacker.example/api/v1/custom/mobile-documents/download?sig=abc",
      "https://knora.example/mobile/",
    ),
    /无效的下载链接/,
  );
  assert.throws(
    () => resolveMobileDocumentDownloadURL(
      "/api/v1/knowledge/secret/download",
      "https://knora.example/mobile/",
    ),
    /无效的下载链接/,
  );
});
