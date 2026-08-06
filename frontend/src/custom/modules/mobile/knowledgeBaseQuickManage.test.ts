import assert from "node:assert/strict";
import test from "node:test";

import { buildQuickKnowledgeBasePayload } from "./knowledgeBaseQuickManage.ts";

test("quick KB creation prefers tenant defaults and excludes derivative-only chat models", () => {
  const result = buildQuickKnowledgeBasePayload(
    [
      { id: "chat-derivative", type: "KnowledgeQA", is_default: true, workload_scope: "derivative_only" },
      { id: "chat-usable", type: "KnowledgeQA" },
      { id: "embedding-first", type: "Embedding" },
      { id: "embedding-default", type: "Embedding", is_default: true },
      { id: "vlm-default", type: "VLLM", is_default: true },
    ] as any,
    { name: "  手机知识库  ", description: "  默认创建  " },
  );

  assert.deepEqual(result.missing, []);
  assert.equal(result.payload?.name, "手机知识库");
  assert.equal(result.payload?.summary_model_id, "chat-usable");
  assert.equal(result.payload?.embedding_model_id, "embedding-default");
  assert.deepEqual(result.payload?.vlm_config, { enabled: true, model_id: "vlm-default" });
  assert.deepEqual(result.payload?.indexing_strategy, {
    vector_enabled: true,
    keyword_enabled: true,
    wiki_enabled: false,
    graph_enabled: false,
  });
  assert.equal(result.payload?.chunking_config.strategy, "auto");
});

test("quick KB creation fails closed when required default resources are missing", () => {
  const result = buildQuickKnowledgeBasePayload([], { name: "缺资源" });
  assert.equal(result.payload, undefined);
  assert.deepEqual(result.missing, ["KnowledgeQA", "Embedding"]);
});

test("quick KB creation keeps optional multimodal disabled when no VLM exists", () => {
  const result = buildQuickKnowledgeBasePayload(
    [
      { id: "chat", type: "KnowledgeQA" },
      { id: "embedding", type: "Embedding" },
    ] as any,
    { name: "普通文档库" },
  );
  assert.deepEqual(result.payload?.vlm_config, { enabled: false, model_id: "" });
});

