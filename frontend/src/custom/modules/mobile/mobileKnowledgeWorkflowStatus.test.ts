import assert from "node:assert/strict";
import test from "node:test";

import { resolveMobileKnowledgeWorkflowPresentation } from "./mobileKnowledgeWorkflowStatus.ts";

test("shows core-searchable while derivative processing is still running", () => {
  const presentation = resolveMobileKnowledgeWorkflowPresentation({
    parse_status: "completed",
    core_status: "ready",
    summary_status: "processing",
    enrichment_status: "completed",
    wiki_status: "completed",
  });
  assert.deepEqual(presentation, {
    status: "processing",
    label: "核心可检索，增强处理中",
    className: "is-running",
  });
});

test("uses the shared workflow resolver for completed, degraded and failed documents", () => {
  assert.equal(resolveMobileKnowledgeWorkflowPresentation({
    parse_status: "completed",
    summary_status: "completed",
    enrichment_status: "completed",
    wiki_status: "completed",
  }).label, "已完成");
  assert.equal(resolveMobileKnowledgeWorkflowPresentation({
    parse_status: "completed",
    summary_status: "degraded",
    enrichment_status: "completed",
    wiki_status: "completed",
  }).className, "is-warning");
  assert.equal(resolveMobileKnowledgeWorkflowPresentation({
    parse_status: "failed",
  }).className, "is-failed");
});
