"""Image-count checkpoints in canonical Linux/Windows App Server packages.

Uses disposable homes and a loopback provider; no live accounts or tools.
"""
import base64
import json
from pathlib import Path
import tempfile
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from interrupted_tool_resume_e2e import Runtime

IMAGE = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR4nGP4z8DwHwAFAAH/iZk9HQAAAABJRU5ErkJggg=="
COMPACT = "MIRA_IMAGE_SUMMARY_REQUEST"


def image_count(items):
    return sum(part.get("type") == "input_image" for item in items
               for part in (item.get("content", []) if item["type"] == "message"
                            else item.get("output", []) if isinstance(item.get("output"), list) else []))


class Provider(BaseHTTPRequestHandler):
    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        self.server.requests.append(body)
        is_compact = COMPACT in json.dumps(body["input"][-1])
        if is_compact and self.server.partial_failure:
            self.server.partial_failure = False
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.end_headers()
            for event in [
                {"type": "response.output_item.done", "item": {"type": "message", "role": "assistant",
                    "id": "partial", "content": [{"type": "output_text", "text": "PARTIAL_SUMMARY"}]}},
                {"type": "response.failed", "response": {"id": "partial", "error": {
                    "code": "invalid_request_error", "message": "synthetic partial summary failure"}}},
            ]:
                self.wfile.write(f"event: {event['type']}\ndata: {json.dumps(event)}\n\n".encode())
            return
        if is_compact and self.server.fail_summary:
            self.server.fail_summary = False
            self.send_response(400)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(json.dumps({"error": {"code": "invalid_request_error",
                                                  "message": "synthetic summary failure"}}).encode())
            return
        user = json.dumps([item for item in body["input"] if item.get("role") == "user"][-1])
        if not is_compact and "TAKE_SCREEN" in user and not self.server.tool_issued:
            self.server.tool_issued = True
            output = {"type": "function_call", "name": "screen", "call_id": "screen-once", "arguments": "{}"}
        else:
            output = {"type": "message", "role": "assistant", "id": "message",
                      "content": [{"type": "output_text", "text": "SAVED_IMAGE_SUMMARY" if is_compact else "IMAGES_READ"}]}
        events = [
            {"type": "response.created", "response": {"id": "response"}},
            {"type": "response.output_item.done", "item": output},
            {"type": "response.completed", "response": {"id": "response", "usage": {
                "input_tokens": 20, "output_tokens": 5, "total_tokens": 25}}},
        ]
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.end_headers()
        for event in events:
            self.wfile.write(f"event: {event['type']}\ndata: {json.dumps(event)}\n\n".encode())

    def do_GET(self):
        self.send_response(404)
        self.end_headers()

    def log_message(self, *_args):
        pass


def configure(home, provider, limit=None):
    (home / "image.png").write_bytes(base64.b64decode(IMAGE.split(",", 1)[1]))
    config = f'''model="gpt-5.1-codex"
model_provider="fixture"
compact_prompt="{COMPACT}"
'''
    if limit is not None:
        config += f"mira_auto_compact_image_limit={limit}\n"
    config += f'''[features]
plugins=false
[model_providers.fixture]
name="Fixture"
base_url="http://127.0.0.1:{provider.server_port}/v1"
wire_api="responses"
request_max_retries=0
stream_max_retries=0
'''
    (home / "config.toml").write_text(config, encoding="utf-8")


def turn(runtime, thread_id, count, text, status="completed"):
    started = runtime.call("turn/start", {"threadId": thread_id, "input": [
        *[{"type": "localImage", "path": str(Path(runtime.log.name).parent / "image.png")} for _ in range(count)],
        {"type": "text", "text": text},
    ]})["turn"]
    completed = runtime.wait(lambda event: event.get("method") == "turn/completed"
                             and event["params"]["turn"]["id"] == started["id"])["params"]["turn"]
    assert completed["status"] == status, completed


def start(runtime, home, **options):
    return runtime.call("thread/start", {"cwd": str(home), "historyMode": "legacy",
                        "approvalPolicy": "never", "sandbox": "danger-full-access", **options})["thread"]


def reset(provider):
    provider.requests = []
    provider.fail_summary = False
    provider.partial_failure = False
    provider.tool_issued = False


def run_checkpoint_case(provider, mode):
    reset(provider)
    with tempfile.TemporaryDirectory(prefix="mira-image-checkpoint-") as temporary:
        home = Path(temporary)
        configure(home, provider, None if mode == "restored" else 3)
        runtime = Runtime(home)
        try:
            # Count-triggered compaction must summarize even when opaque/token reset is selected.
            options = {"config": {"features.token_budget": True, "features.mira_plaintext_context": False}}
            thread = start(runtime, home, **options)
            path = Path(thread["path"])
            turn(runtime, thread["id"], 52 if mode == "restored" else 2, "OLD_IMAGES")
            original = path.read_bytes()
            if mode == "restored":
                runtime.close()
                configure(home, provider, 40)
                runtime = Runtime(home)
                runtime.call("thread/resume", {"threadId": thread["id"], "excludeTurns": True})
            if mode in ("failure", "partial_failure"):
                provider.fail_summary = mode == "failure"
                provider.partial_failure = mode == "partial_failure"
                turn(runtime, thread["id"], 1, "FRESH_IMAGES", "failed")
                assert len(provider.requests) == 2, "sampling continued after summary failure"
                assert not any(json.loads(line)["type"] == "compacted" for line in path.read_text().splitlines())
                turn(runtime, thread["id"], 0, "RETRY")
                compact, sampling = provider.requests[2:4]
            else:
                turn(runtime, thread["id"], 1, "FRESH_IMAGES")
                compact, sampling = provider.requests[1:3]
            assert compact["tools"] == [], compact
            assert image_count(compact["input"]) == (40 if mode == "restored" else 2)
            assert "FRESH_IMAGES" not in json.dumps(compact["input"])
            assert image_count(sampling["input"]) == 1
            assert "FRESH_IMAGES" in json.dumps(sampling["input"])
            turn(runtime, thread["id"], 0, "CONTINUE")
            assert provider.requests[-1]["input"][:len(sampling["input"])] == sampling["input"], "cached input prefix changed"
            saved = path.read_bytes()
            assert saved.startswith(original), "original images/history changed"
            assert sum(json.loads(line)["type"] == "compacted" for line in saved.decode().splitlines()) == 1
        finally:
            runtime.close()
        runtime = Runtime(home)
        try:
            runtime.call("thread/resume", {"threadId": thread["id"], "excludeTurns": True})
            turn(runtime, thread["id"], 0, "COLD_CONTINUE")
            assert image_count(provider.requests[-1]["input"]) == 1
            assert path.read_bytes().startswith(saved)
        finally:
            runtime.close()
    print(f"passed: {mode}: bounded summary, fresh images, cache prefix, append-only checkpoint and cold resume", flush=True)


def run_tool_case(provider):
    reset(provider)
    with tempfile.TemporaryDirectory(prefix="mira-image-tool-") as temporary:
        home = Path(temporary)
        configure(home, provider, 3)
        runtime = Runtime(home)
        try:
            thread = start(runtime, home, dynamicTools=[{"name": "screen", "description": "Synthetic screenshot",
                "inputSchema": {"type": "object", "properties": {}}, "deferLoading": False}])
            runtime.call("turn/start", {"threadId": thread["id"], "input": [
                {"type": "localImage", "path": str(Path(runtime.log.name).parent / "image.png")}, {"type": "localImage", "path": str(Path(runtime.log.name).parent / "image.png")},
                {"type": "text", "text": "TAKE_SCREEN"}]})
            call = runtime.wait(lambda event: event.get("method") == "item/tool/call")
            runtime.process.stdin.write(json.dumps({"id": call["id"], "result": {
                "contentItems": [{"type": "inputImage", "imageUrl": IMAGE}], "success": True}}) + "\n")
            runtime.process.stdin.flush()
            completed = runtime.wait(lambda event: event.get("method") == "turn/completed")["params"]["turn"]
            assert completed["status"] == "completed", completed
            assert len(provider.requests) == 3
            sampling = provider.requests[2]["input"]
            assert image_count(sampling) == 1
            assert sum(item["type"] == "function_call" and item["call_id"] == "screen-once" for item in sampling) == 1
            assert any(item["type"] == "function_call_output" and item["call_id"] == "screen-once" for item in sampling)
            records = [json.loads(line) for line in Path(thread["path"]).read_text().splitlines()]
            assert sum(record["type"] == "response_item" and record["payload"]["type"] == "function_call" for record in records) == 1
        finally:
            runtime.close()
    print("passed: tool screenshot preserved with original call/output and no repeated tool execution", flush=True)


def main():
    provider = ThreadingHTTPServer(("127.0.0.1", 0), Provider)
    threading.Thread(target=provider.serve_forever, daemon=True).start()
    try:
        for mode in ["threshold", "restored", "failure", "partial_failure"]:
            run_checkpoint_case(provider, mode)
        run_tool_case(provider)
    finally:
        provider.shutdown()
        provider.server_close()


if __name__ == "__main__":
    main()
