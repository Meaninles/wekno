import assert from "node:assert/strict";
import test from "node:test";
import {
  processItems,
  visibleProcess,
  previewText,
  safeSourceURL,
  reasoningRoundCount,
} from "./processPresentation.ts";

test('status and commentary do not inflate reasoning counts on desktop or mobile', () => {
  assert.equal(reasoningRoundCount([
    { type: 'thinking', process_kind: 'process_status', content: '正在思考' },
    { type: 'thinking', process_kind: 'commentary', content: '将核对资料' },
    { type: 'thinking', process_kind: 'thought_delta', content: '已有模型过程' },
    { type: 'thinking', process_kind: 'thought_delta', content: '  ' },
  ]), 1)
})

test("long mixed runs keep three thoughts and two actions, pinning does not stop live work", () => {
  const stream: any[] = [];
  for (let i = 0; i < 60; i++) {
    stream.push({
      type: "thinking",
      event_id: `t${i}`,
      content: `思考 ${i}`,
      thinking: false,
    });
    stream.push({
      type: "tool_call",
      tool_call_id: `s${i}`,
      tool_name: "knowledge_search",
      pending: false,
      tool_data: { process_source_count: 3 },
    });
  }
  stream.push({
    type: "tool_call",
    tool_call_id: "active",
    tool_name: "Bash",
    pending: true,
  });
  const items = processItems(stream);
  const view = visibleProcess(items, "s2");
  assert.equal(view.thoughts.length, 3);
  assert.equal(view.actions.length, 2);
  assert.equal(view.thoughts.at(-1)?.text, "思考 59");
  assert.equal(view.actions[0].key, "s2");
  assert.equal(view.actions[1].key, "active");
  assert.equal(visibleProcess(items).actions[0].key, "active");
});
test("metadata does not displace retrieved documents and counts are documents, not chunks", () => {
  const items = processItems([
    {
      type: "tool_call",
      tool_call_id: "search",
      tool_name: "mcp__knowledge_search",
      tool_data: { count: 15, process_source_count: 2 },
    },
    { type: "tool_call", tool_call_id: "meta", tool_name: "get_document_info" },
  ]);
  const view = visibleProcess(items);
  assert.equal(view.actions[0].text, "找到 2 份相关文档");
  assert.equal(view.actions.length, 1);
});
test("failed search is not an empty successful result; stopped actions are not completed", () => {
  const failed = processItems([
    { type: "tool_call", tool_name: "web_search", success: false },
  ])[0];
  assert.equal(failed.state, "error");
  assert.match(failed.text, /未完成/);
  const stopped = processItems(
    [{ type: "tool_call", tool_name: "Read", pending: true }],
    true,
  )[0];
  assert.equal(stopped.state, "stopped");
});
test("thoughts update to recent text, unsafe web links cannot become anchors", () => {
  assert.match(
    previewText("旧内容".repeat(200) + "。正在核对最新证据", true),
    /最新证据/,
  );
  assert.ok(Array.from(previewText("长".repeat(1000), true)).length <= 180);
  assert.equal(safeSourceURL("javascript:alert(1)"), "");
  assert.equal(
    safeSourceURL("https://example.com/doc?q=a"),
    "https://example.com/doc?q=a",
  );
});

test("missing, invalid and contradictory counts never claim no results", () => {
  for (const count of [undefined, null, "invalid", -1]) {
    const item = processItems([
      {
        type: "tool_call",
        tool_name: "knowledge_search",
        tool_data: { process_source_count: count },
      },
    ])[0];
    assert.equal(item.sourceCount, null);
    assert.equal(item.text, "资料检索完成");
  }
  const found = processItems([
    {
      type: "tool_call",
      tool_name: "knowledge_search",
      tool_data: {
        process_source_count: 0,
        results: [
          { knowledge_id: "doc", knowledge_title: "检验与放行管理规定" },
        ],
      },
    },
  ])[0];
  assert.equal(found.text, "已找到相关资料");
  assert.equal(found.sources[0].title, "检验与放行管理规定");
  const empty = processItems([
    {
      type: "tool_call",
      tool_name: "knowledge_search",
      tool_data: { process_source_count: 0 },
    },
  ])[0];
  assert.equal(empty.text, "本次未找到相关资料");
});
