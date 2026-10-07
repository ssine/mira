"""Run with DEEPSEEK_TEST_TOKENIZER=/path/to/model/tokenizer.json python -m unittest -v."""
from dataclasses import replace
import json
import os
from pathlib import Path
import tempfile
import unittest

from deepseek_recipe import Tokenizer
from fastapi.testclient import TestClient
import httpx

from service import Settings, create_app, prepare, RequestError


TOKENIZER = os.environ.get("DEEPSEEK_TEST_TOKENIZER")


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
        self.output = "Compute.</think>391<｜end▁of▁sentence｜>"
        self.finish_reason = "stop"
        self.backend_status = 200

    def backend(self, request):
        self.requests.append(json.loads(request.content))
        if self.backend_status != 200:
            return httpx.Response(self.backend_status, json={"error": "engine private data"})
        ids = self.tokenizer.encode(self.output)
        # Token-by-token frames exercise special tokens and partial UTF-8.
        chunks = [{"choices": [{"index": 0, "token_ids": [i], "finish_reason": None}], "usage": {"prompt_tokens": len(self.requests[-1]["prompt"]), "prompt_tokens_details": {"cached_tokens": 8}}} for i in ids]
        if self.finish_reason:
            chunks.append({"choices": [{"index": 0, "token_ids": [], "finish_reason": self.finish_reason}]})
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


if __name__ == "__main__":
    unittest.main()
