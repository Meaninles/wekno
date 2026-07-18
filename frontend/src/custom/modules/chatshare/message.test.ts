import assert from "node:assert/strict";
import test from "node:test";
import { userQueryFor } from "./message.ts";

test("userQueryFor pairs selected answers by request id across omitted turns", () => {
  const messages = [
    { id: "q1", role: "user", request_id: "r1", content: "问题一" },
    { id: "q2", role: "user", request_id: "r2", content: "问题二" },
    { id: "a1", role: "assistant", request_id: "r1", content: "回答一" },
  ];
  assert.equal(userQueryFor(messages, 2), "问题一");
});

test("userQueryFor does not borrow an unrelated previous question", () => {
  const messages = [
    { id: "q2", role: "user", request_id: "r2", content: "问题二" },
    { id: "a1", role: "assistant", request_id: "r1", content: "回答一" },
  ];
  assert.equal(userQueryFor(messages, 1), "");
});

test("userQueryFor never falls back to an adjacent question without request id", () => {
  const messages = [
    { id: "q1", role: "user", content: "不能被猜配的问题", request_id: "" },
    { id: "a1", role: "assistant", content: "无关联键的回答", request_id: "" },
  ];

  assert.equal(userQueryFor(messages, 1), "");
});
