import test from "node:test";
import assert from "node:assert/strict";
import { visibleAssistantUpdate } from "../server/thread-read-state.mjs";

const event = (type, extra = {}) => ({ type: "event_msg", payload: { type, ...extra } });
const response = (role, content) => ({ type: "response_item", payload: { type: "message", role, content } });

test("only non-empty assistant prose creates an unread update", () => {
  assert.equal(visibleAssistantUpdate(event("agent_message", { message: "已完成。" })), true);
  assert.equal(visibleAssistantUpdate(event("agent_message", { message: " \n\u0000\t" })), false);
  assert.equal(visibleAssistantUpdate(event("item_completed", { item: {
    type: "AgentMessage", content: [{ type: "Text", text: "可阅读的结果" }],
  } })), true);
  assert.equal(visibleAssistantUpdate(response("assistant", [{ type: "output_text", text: "回复正文" }])), true);
  assert.equal(visibleAssistantUpdate(response("assistant", [{ type: "output_image", image_url: "image" }])), false);
});

test("user text, tool evidence and lifecycle records stay quiet", () => {
  assert.equal(visibleAssistantUpdate(event("user_message", { message: "我的问题" })), false);
  assert.equal(visibleAssistantUpdate(response("user", [{ type: "input_text", text: "我的问题" }])), false);
  assert.equal(visibleAssistantUpdate(event("item_completed", { item: {
    type: "FunctionCallOutput", content: [{ type: "Text", text: "工具输出" }],
  } })), false);
  assert.equal(visibleAssistantUpdate({ type: "response_item", payload: {
    type: "function_call_output", output: "工具输出",
  } }), false);
  assert.equal(visibleAssistantUpdate(event("view_image_tool_call", { path: "/tmp/chart.png" })), false);
  assert.equal(visibleAssistantUpdate(event("task_complete", { last_agent_message: "嵌套文本" })), false);
  assert.equal(visibleAssistantUpdate(event("error", { message: "执行失败" })), false);
});
