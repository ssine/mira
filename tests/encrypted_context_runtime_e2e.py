"""Permanent encrypted-input errors must survive HTTP/SSE and stop both retry layers.

Run against a canonical release package using CODEX_TEST_BINARY. All model
responses and histories are synthetic and stay on loopback, without credentials.
"""
import json
from pathlib import Path
import tempfile
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from interrupted_tool_resume_e2e import Runtime


class Provider(BaseHTTPRequestHandler):
    def do_POST(self):
        self.rfile.read(int(self.headers["Content-Length"]))
        case = self.server.case
        self.server.attempts += 1
        status, payload = case["response"]
        if case["retry"] and self.server.attempts > case.get("failures", 1):
            status, payload = 200, [
                {"type": "response.created", "response": {"id": "fixture-response"}},
                {"type": "response.output_item.done", "item": {
                    "type": "message", "id": "fixture-message", "role": "assistant",
                    "content": [{"type": "output_text", "text": "RECOVERED"}]}},
                {"type": "response.completed", "response": {"id": "fixture-response",
                    "usage": {"input_tokens": 10, "output_tokens": 1, "total_tokens": 11}}},
            ]
        self.send_response(status)
        if status == 429 or case.get("rate_limit"):
            self.send_header("Retry-After", "0")
        self.send_header("Content-Type", "text/event-stream" if status == 200 else "text/plain" if isinstance(payload, str) else "application/json")
        self.end_headers()
        if status == 200:
            for event in payload:
                self.wfile.write(f"event: {event['type']}\ndata: {json.dumps(event)}\n\n".encode())
        else:
            self.wfile.write((payload if isinstance(payload, str) else json.dumps(payload)).encode())

    def log_message(self, *_args):
        pass


def main():
    cases = []
    for code in ["invalid_encrypted_content", "unknown_reasoning_pool"]:
        error = {"code": code, "message": "Encrypted context cannot be verified."}
        for status in [400, 409, 429, 500, 502]:
            cases.append({"name": f"HTTP {status} {code}", "code": code, "retry": False,
                          "response": (status, {"error": error})})
        for event in [
            {"type": "response.failed", "response": {"error": error}},
            {"type": "error", "error": error},
            {"type": "error", **error},
        ]:
            cases.append({"name": f"SSE {json.dumps(event)}", "code": code, "retry": False,
                          "response": (200, [event])})
    for status, error in [
        (500, {"code": "server_error", "message": "Temporary service failure"}),
        (500, {"code": "server_error", "message": "Text mentioning invalid_encrypted_content"}),
        (429, {"code": "rate_limit_exceeded", "message": "Try again later"}),
    ]:
        cases.append({"name": f"transient HTTP {status}: {error['message']}", "retry": True,
                      "response": (status, {"error": error}), "failures": 3 if status == 429 else 1})

    # Sanitized production diagnostic: a TPM admission timeout mislabeled as 401.
    tpm_timeout = "ratelimiter: tpm acquire project: tpm peek tpm:project:test-model:42: context deadline exceeded"
    cases.extend([
        {"name": "HTTP 401 project TPM admission timeout", "retry": True, "rate_limit": True,
         "failures": 3, "response": (401, tpm_timeout)},
        {"name": "HTTP 401 invalid API key", "retry": False, "code": "invalid_api_key", "bounded_attempts": 3,
         "response": (401, {"error": {"code": "invalid_api_key", "message": "Invalid API key"}})},
        {"name": "HTTP 401 unrelated deadline", "retry": False, "code": "context deadline exceeded", "bounded_attempts": 3,
         "response": (401, "authentication: context deadline exceeded")},
    ])
    warmup_case = next(case for case in cases if case["retry"])

    provider = ThreadingHTTPServer(("127.0.0.1", 0), Provider)
    threading.Thread(target=provider.serve_forever, daemon=True).start()
    try:
        with tempfile.TemporaryDirectory(prefix="mira-encrypted-context-") as temporary:
            home = Path(temporary)
            (home / "config.toml").write_text(f'''model="gpt-5.1-codex"
model_provider="fixture"
[features]
plugins=false
[model_providers.fixture]
name="Fixture"
base_url="http://127.0.0.1:{provider.server_port}/v1"
wire_api="responses"
request_max_retries=2
stream_max_retries=2
''')
            runtime = Runtime(home)
            try:
                for operation in ["turn", "compact"]:
                    for case in cases:
                        provider.case, provider.attempts = case, 0
                        thread = runtime.call("thread/start", {"cwd": temporary, "historyMode": "legacy",
                            "approvalPolicy": "never", "sandbox": "danger-full-access"})["thread"]
                        if operation == "compact":
                            # Seed ordinary history through the fixture's existing success path.
                            provider.case, provider.attempts = warmup_case, warmup_case.get("failures", 1)
                            warmup = runtime.call("turn/start", {"threadId": thread["id"],
                                "input": [{"type": "text", "text": "Synthetic compaction history"}]})["turn"]
                            runtime.wait(lambda event: event.get("method") == "turn/completed"
                                and event["params"]["turn"]["id"] == warmup["id"])
                            provider.case, provider.attempts = case, 0
                        event_count = len(runtime.events)
                        if operation == "compact":
                            runtime.call("thread/compact/start", {"threadId": thread["id"]})
                        else:
                            runtime.call("turn/start", {"threadId": thread["id"],
                                "input": [{"type": "text", "text": "Synthetic retry fixture"}]})
                        completed = runtime.wait(lambda event: event.get("method") == "turn/completed"
                            and event["params"]["threadId"] == thread["id"]
                            and event not in runtime.events[:event_count])["params"]["turn"]
                        expected = "completed" if case["retry"] else "failed"
                        assert completed["status"] == expected, (case["name"], completed)
                        assert provider.attempts == (case.get("failures", 1) + 1 if case["retry"] else case.get("bounded_attempts", 1)), (case["name"], provider.attempts)
                        if case["retry"] and (case["response"][0] == 429 or case.get("rate_limit")):
                            assert any(event.get("method") == "error" and event["params"].get("willRetry")
                                and event["params"].get("error", {}).get("message", "").startswith("Rate limited;")
                                for event in runtime.events[event_count:]), (operation, case["name"])
                        if not case["retry"]:
                            assert case["code"] in completed["error"]["message"], (case["name"], completed)
                            assert not any(event.get("method") == "error"
                                and event["params"].get("threadId") == thread["id"]
                                and event["params"].get("willRetry")
                                and (not case.get("bounded_attempts") or event["params"].get("error", {}).get("message", "").startswith("Rate limited;"))
                                for event in runtime.events), case["name"]
                        print(f"passed: {operation}: {case['name']}", flush=True)
            finally:
                runtime.close()
    finally:
        provider.shutdown()
        provider.server_close()
    print(f"encrypted context release runtime: {len(cases) * 2} scenarios passed")


if __name__ == "__main__":
    main()
