"""Run with DEEPSEEK_TEST_TOKENIZER=/path/to/model/tokenizer.json python -m unittest -v."""
from dataclasses import replace
import base64
import json
import os
from pathlib import Path
import tempfile
import unittest
import struct
import zlib

from deepseek_recipe import DeepseekV41Encoding, Tokenizer
from fastapi.testclient import TestClient
import httpx

from service import Settings, create_app, prepare, RequestError


TOKENIZER = os.environ.get("DEEPSEEK_TEST_TOKENIZER")


def image_url(color=(255, 0, 0), width=64, height=48):
    def chunk(kind, data):
        return struct.pack(">I", len(data)) + kind + data + struct.pack(">I", zlib.crc32(kind + data))
    raw = (b"\0" + bytes(color) * width) * height
    png = b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", struct.pack(">IIBBBBB", width, height, 8, 2, 0, 0, 0))
    png += chunk(b"IDAT", zlib.compress(raw)) + chunk(b"IEND", b"")
    return "data:image/png;base64," + base64.b64encode(png).decode()


@unittest.skipUnless(TOKENIZER, "Set DEEPSEEK_TEST_TOKENIZER to a V4.1 tokenizer.json")
class ContractTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.tokenizer = Tokenizer.from_file(TOKENIZER)

    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        key_path = Path(self.directory.name) / "key"
        key_path.write_text("test-only-" + "x" * 48)
        key_path.chmod(0o600)
        self.settings = Settings(TOKENIZER, str(key_path))
        self.headers = {"Authorization": "Bearer " + key_path.read_text()}
        self.requests = []
        self.paths = []
        self.output = "Compute.</think>391<｜end▁of▁sentence｜>"
        self.finish_reason = "stop"
        self.backend_status = 200
        self.final_cached_tokens = None

    def backend(self, request):
        self.paths.append(request.url.path)
        self.requests.append(json.loads(request.content))
        if self.backend_status != 200:
            return httpx.Response(self.backend_status, json={"error": "engine private data"})
        ids = self.tokenizer.encode(self.output)
        # Token-by-token frames exercise special tokens and partial UTF-8.
        prompt_tokens = self.requests[-1].get("expected_prompt_tokens", len(self.requests[-1]["prompt"]))
        chunks = [{"choices": [{"index": 0, "token_ids": [i], "finish_reason": None}], "usage": {"prompt_tokens": prompt_tokens, "prompt_tokens_details": {"cached_tokens": 8}}} for i in ids]
        if self.finish_reason:
            chunks.append({"choices": [{"index": 0, "token_ids": [], "finish_reason": self.finish_reason}]})
            if self.final_cached_tokens is not None:
                chunks.append({"choices": [], "usage": {"prompt_tokens": prompt_tokens, "completion_tokens": len(ids), "prompt_tokens_details": {"cached_tokens": self.final_cached_tokens}}})
        return httpx.Response(200, text="".join("data: " + json.dumps(c) + "\n\n" for c in chunks) + "data: [DONE]\n\n")

    def client(self):
        client = TestClient(create_app(self.settings, transport=httpx.MockTransport(self.backend)))
        self.addCleanup(client.__exit__, None, None, None)
        return client.__enter__()

    def payload(self, **extra):
        return {"model": self.settings.model, "input": "17 * 23?", **extra}

    def test_all_routes_require_auth_before_body_or_backend(self):
        client = self.client()
        for path in ("/v1/responses", "/v1/chat/completions", "/v1/models", "/health", "/v1/completions", "/docs", "/unknown"):
            for headers in ({}, {"Authorization": "Bearer wrong"}):
                self.assertEqual(401, client.post(path, content=b"invalid", headers=headers).status_code)
        self.assertEqual([], self.requests)
        self.assertEqual(200, client.get("/v1/models", headers=self.headers).status_code)
        self.assertEqual(404, client.get("/v1/completions", headers=self.headers).status_code)

    def test_text_usage_and_raw_token_backend(self):
        result = self.client().post("/v1/responses", json=self.payload(), headers=self.headers)
        self.assertEqual(200, result.status_code, result.text)
        body = result.json()
        text = [c["text"] for i in body["output"] if i["type"] == "message" for c in i["content"]]
        self.assertEqual(["391"], text)
        self.assertEqual("completed", body["status"])
        self.assertGreater(body["usage"]["output_tokens"], 0)
        self.assertEqual(8, body["usage"]["input_tokens_details"]["cached_tokens"])
        sent = self.requests[0]
        self.assertIsInstance(sent["prompt"][0], int)
        self.assertNotIn("messages", sent)
        self.assertFalse(sent["add_special_tokens"])
        self.assertEqual(0.95, sent["top_p"])

    def test_custom_apply_patch_type_and_history(self):
        patch = "*** Begin Patch\n*** Add File: result.txt\n+391\n*** End Patch"
        self.output = ('Use tool.</think><｜DSML｜ calls>\n<｜DSML｜ invoke name="apply_patch">\n'
                       '<｜DSML｜ parameter name="input" string="true">' + patch + '</｜DSML｜ parameter>\n'
                       '</｜DSML｜ invoke>\n</｜DSML｜ calls><｜end▁of▁sentence｜>')
        tools = [{"type": "custom", "name": "apply_patch"}]
        client = self.client()
        first = client.post("/v1/responses", json=self.payload(tools=tools), headers=self.headers)
        self.assertEqual(200, first.status_code, first.text)
        calls = [x for x in first.json()["output"] if x["type"] == "custom_tool_call"]
        self.assertEqual(1, len(calls))
        self.assertEqual(patch, calls[0]["input"])
        self.output = "Applied.</think>Done.<｜end▁of▁sentence｜>"
        history = [{"role": "user", "content": "Create result.txt"}] + first.json()["output"] + [{"type": "custom_tool_call_output", "call_id": calls[0]["call_id"], "output": "Success"}]
        second = client.post("/v1/responses", json=self.payload(tools=tools, input=history), headers=self.headers)
        self.assertEqual(200, second.status_code, second.text)

    def test_final_usage_frame_overrides_initial_cache_count(self):
        self.final_cached_tokens = 16
        self.settings = replace(self.settings, recipe_backend=True)
        client = self.client()
        for stream in (False, True):
            result = client.post("/v1/responses", json=self.payload(stream=stream), headers=self.headers)
            self.assertEqual(200, result.status_code, result.text)
            if stream:
                events = [json.loads(line[6:]) for line in result.text.splitlines() if line.startswith("data: {")]
                response = events[-1]["response"]
            else:
                response = result.json()
            self.assertEqual(16, response["usage"]["input_tokens_details"]["cached_tokens"])
            self.assertEqual("/recipe/v1/completions", self.paths[-1])
            self.assertEqual([], self.requests[-1]["images"])

    def test_stream_unicode_and_terminal_event(self):
        self.output = "计算。</think>结果是391。<｜end▁of▁sentence｜>"
        result = self.client().post("/v1/responses", json=self.payload(stream=True), headers=self.headers)
        events = [json.loads(line[6:]) for line in result.text.splitlines() if line.startswith("data: {")]
        self.assertEqual("response.created", events[0]["type"])
        self.assertEqual("response.completed", events[-1]["type"])
        self.assertEqual("结果是391。", "".join(e["delta"] for e in events if e["type"] == "response.output_text.delta"))
        sequence = [e["sequence_number"] for e in events]
        self.assertEqual(sorted(set(sequence)), sequence)

    def test_engine_failure_and_truncation_are_not_completed(self):
        self.backend_status = 503
        client = self.client()
        result = client.post("/v1/responses", json=self.payload(stream=True), headers=self.headers)
        self.assertEqual(502, result.status_code)
        self.assertNotIn("private data", result.text)
        self.backend_status = 200
        self.output = "Unfinished reasoning"
        self.finish_reason = None
        result = client.post("/v1/responses", json=self.payload(stream=True), headers=self.headers)
        self.assertIn('"type": "error"', result.text)
        self.assertNotIn("response.completed", result.text)

    def test_limits_and_unsupported_inputs(self):
        client = self.client()
        for payload in (self.payload(model="wrong"), self.payload(max_output_tokens=294912), self.payload(previous_response_id="resp_missing"), self.payload(input=[{"role":"user","content":[{"type":"input_image","image_url":"https://example.invalid/image.png"}]}])):
            result = client.post("/v1/responses", json=payload, headers=self.headers)
            self.assertTrue(400 <= result.status_code < 500, result.text)
        self.assertEqual([], self.requests)
        with self.assertRaises(RequestError):
            prepare("responses", json.dumps(self.payload()).encode(), replace(self.settings, context_tokens=1), self.tokenizer)

    def test_chat_completions_and_length(self):
        self.output = "</think>Hello"
        self.finish_reason = "length"
        result = self.client().post("/v1/chat/completions", json={"model": self.settings.model, "messages":[{"role":"user","content":"Hello"}]}, headers=self.headers)
        self.assertEqual(200, result.status_code, result.text)
        self.assertEqual("Hello", result.json()["choices"][0]["message"]["content"])
        self.assertEqual("length", result.json()["choices"][0]["finish_reason"])

    def test_agent_tasks_messages_and_answers_reach_the_model_in_order(self):
        content = []
        for kind, author, recipient, text in (
            ("NEW_TASK", "/root", "/root/worker", "执行任务：写入任务标记。"),
            ("MESSAGE", "/root", "/root/worker", "追加信息：保留先前结果。"),
            ("FINAL_ANSWER", "/root/worker", "/root", "任务结果：已写入标记。"),
        ):
            blocks = [{"type": "input_text", "text": f"Message Type: {kind}\nPayload:\n{text}"},
                      {"type": "input_text", "text": f"END_{kind}"}]
            content.append({"type": "agent_message", "id": f"amsg_{kind}",
                            "author": author, "recipient": recipient, "content": blocks})
        history = [{"role": "user", "content": "Environment only."}, *content]
        prepared = prepare("responses", json.dumps(self.payload(input=history)).encode(),
                           self.settings, self.tokenizer)
        prompt = DeepseekV41Encoding().render_conversation(prepared.converted.conversation).prompt
        positions = []
        for item in content:
            for block in item["content"]:
                self.assertIn(block["text"], prompt)
                positions.append(prompt.index(block["text"]))
        self.assertEqual(sorted(positions), positions)
        self.assertIn("/root/worker", prompt)
        self.assertIn("from /root/worker to /root", prompt)
        client = self.client()
        for stream in (False, True):
            response = client.post("/v1/responses", json=self.payload(input=history, stream=stream),
                                   headers=self.headers)
            self.assertEqual(200, response.status_code, response.text)
            self.assertEqual(prepared.tokens, self.requests[-1]["prompt"])

    def test_agent_message_tool_history_and_context_budget(self):
        history = [
            {"role": "user", "content": "Run the check."},
            {"type": "function_call", "call_id": "check", "name": "check", "arguments": "{}"},
            {"type": "function_call_output", "call_id": "check", "output": "CHECK_COMPLETED"},
            {"type": "agent_message", "author": "/root/worker", "recipient": "/root",
             "content": [{"type": "input_text", "text": "FINAL_RESULT_391"}]},
            {"type": "message", "role": "user", "content": "Continue after the result."},
        ]
        body = json.dumps(self.payload(input=history)).encode()
        prepared = prepare("responses", body, self.settings, self.tokenizer)
        prompt = DeepseekV41Encoding().render_conversation(prepared.converted.conversation).prompt
        self.assertLess(prompt.index("CHECK_COMPLETED"), prompt.index("FINAL_RESULT_391"))
        self.assertLess(prompt.index("FINAL_RESULT_391"), prompt.index("Continue after the result."))
        large_message = {"type": "agent_message", "author": "/root", "recipient": "/root/worker",
                         "content": [{"type": "input_text", "text": "TASK_TEXT_391 " * 1000}]}
        with self.assertRaises(RequestError):
            prepare("responses", json.dumps(self.payload(input=[large_message])).encode(),
                    replace(self.settings, context_tokens=512), self.tokenizer)

    def test_unreadable_agent_messages_fail_before_inference(self):
        valid = {"type": "agent_message", "author": "/root", "recipient": "/root/worker",
                 "content": [{"type": "input_text", "text": "TASK_PAYLOAD"}]}
        invalid = [
            {**valid, "content": []},
            {**valid, "content": "TASK_PAYLOAD"},
            {**valid, "content": [{"type": "encrypted_content", "encrypted_content": "opaque"}]},
            {**valid, "content": [*valid["content"], {"type": "encrypted_content", "encrypted_content": "opaque"}]},
            {**valid, "content": [{"type": "input_text", "text": 391}]},
            {**valid, "content": [{"type": "input_text", "text": "  "}]},
            {**valid, "content": [{"type": "input_image", "image_url": image_url()}]},
            {**valid, "author": None},
            {**valid, "recipient": ""},
        ]
        client = self.client()
        for item in invalid:
            with self.subTest(item=item):
                response = client.post("/v1/responses", json=self.payload(input=[item]), headers=self.headers)
                self.assertEqual(400, response.status_code, response.text)
                self.assertNotIn("opaque", response.text)
        self.assertEqual([], self.requests)

    def test_images_preserve_order_tokens_usage_and_context(self):
        content = [
            {"type": "input_text", "text": "Compare these images."},
            {"type": "input_image", "image_url": image_url()},
            {"type": "input_image", "image_url": image_url((0, 0, 255)), "detail": "low"},
        ]
        payload = self.payload(input=[{"role": "user", "content": content}])
        result = self.client().post("/v1/responses", json=payload, headers=self.headers)
        self.assertEqual(200, result.status_code, result.text)
        sent = self.requests[-1]
        self.assertEqual("/recipe/v1/completions", self.paths[-1])
        self.assertEqual(2, sent["prompt"].count(129264))
        self.assertEqual(2, len(sent["images"]))
        self.assertNotEqual(sent["images"][0], sent["images"][1])
        self.assertEqual(b"WEBP", base64.b64decode(sent["images"][0])[8:12])
        self.assertGreater(sent["expected_prompt_tokens"], len(sent["prompt"]) + 300)
        self.assertEqual(sent["expected_prompt_tokens"], result.json()["usage"]["input_tokens"])
        with self.assertRaises(RequestError):
            prepare("responses", json.dumps(payload).encode(),
                    replace(self.settings, context_tokens=len(sent["prompt"]) + 300), self.tokenizer)

    def test_chat_image_and_tool_image_history(self):
        chat = {"model": self.settings.model, "messages": [{"role": "user", "content": [
            {"type": "text", "text": "Describe."},
            {"type": "image_url", "image_url": {"url": image_url()}},
        ]}]}
        client = self.client()
        self.assertEqual(200, client.post("/v1/chat/completions", json=chat, headers=self.headers).status_code)
        payload = self.payload(input=[
            {"role": "user", "content": "Inspect the screenshot."},
            {"type": "function_call", "name": "screenshot", "arguments": "{}", "call_id": "call_test"},
            {"type": "function_call_output", "call_id": "call_test", "output": [
                {"type": "input_image", "image_url": image_url()},
            ]},
        ], tools=[{"type": "function", "name": "screenshot", "parameters": {"type": "object"}}])
        result = client.post("/v1/responses", json=payload, headers=self.headers)
        self.assertEqual(200, result.status_code, result.text)
        self.assertEqual(1, self.requests[-1]["prompt"].count(129264))

    def test_invalid_images_fail_before_inference(self):
        client = self.client()
        for url in ("data:image/png;base64,invalid!", "data:image/png;base64,aGVsbG8=",
                    "http://127.0.0.1/private", "file:///etc/passwd", image_url(width=8193, height=1)):
            payload = self.payload(input=[{"role": "user", "content": [{"type": "input_image", "image_url": url}]}])
            result = client.post("/v1/responses", json=payload, headers=self.headers)
            self.assertTrue(400 <= result.status_code < 500, result.text)
        self.assertEqual([], self.requests)


if __name__ == "__main__":
    unittest.main()
