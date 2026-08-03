import assert from "node:assert/strict";
import test from "node:test";
import {
  formatReferenceFileType,
  formatSourceLocator,
  publicReferenceToken,
} from "./publicReference.ts";

test("public IM reference accepts only the opaque token route parameter", () => {
  assert.equal(publicReferenceToken({ token: " signed-token ", chunk_id: "must-not-be-used" }), "signed-token");
  assert.equal(publicReferenceToken({ token: ["first", "second"] }), "first");
  assert.equal(publicReferenceToken({ knowledge_id: "doc-1", chunk_id: "chunk-1" }), "");
});

test("public document reference derives the same visible file type without exposing a KB route", () => {
  assert.equal(formatReferenceFileType({
    title: "制度",
    file_name: "采购制度.docx",
    file_type: "",
    file_size: 0,
    created_at: "",
    updated_at: "",
  }), "DOCX");
  assert.equal(formatReferenceFileType({
    title: "制度",
    file_name: "采购制度",
    file_type: ".pdf",
    file_size: 0,
    created_at: "",
    updated_at: "",
  }), "PDF");
});

test("public fragment formats stable logical source coordinates", () => {
  assert.equal(formatSourceLocator({ page: 9 }), "第 9 页");
  assert.equal(
    formatSourceLocator({ sheet: "采购明细", row_start: 20, row_end: 25 }),
    "工作表 采购明细 · 第 20–25 行",
  );
  assert.equal(formatSourceLocator({ line_start: 8, line_end: 8 }), "第 8 行");
});
