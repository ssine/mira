"""Verify default portable context in canonical Linux/Windows release packages.

Built-in OpenAI, Azure and a custom provider all use a synthetic loopback model.
No plaintext feature/provider override is set, including when token_budget is on.
"""
import json
from pathlib import Path
import tempfile
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from interrupted_tool_resume_e2e import Runtime

PROMPT = "MIRA_PLAINTEXT_COMPACT_FIXTURE"
SUMMARY = "PORTABLE_TASK_STATE"


def prose(text):
    return {"type": "message", "id": "message", "role": "assistant",
            "content": [{"type": "output_text", "text": text}]}


class Provider(BaseHTTPRequestHandler):
    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        self.server.requests.append(body)
        items = body["input"]
        user = next((json.dumps(item) for item in reversed(items) if item.get("role") == "user"), "")
        tokens = 20
        if PROMPT in user:
            output = prose(SUMMARY)
        elif "ARM_AUTO" in user:
            output, tokens = prose("ready for automatic compaction"), 60_000
        elif "MID_AUTO" in user and not self.server.mid_call_issued:
            self.server.mid_call_issued = True
            output, tokens = {"type": "function_call", "namespace": "collaboration", "name": "list_agents",
                              "call_id": "mid-auto", "arguments": "{}"}, 60_000
        else:
            output = prose("CONTEXT_OK")
        events = [
            {"type": "response.created", "response": {"id": "response"}},
            {"type": "response.output_item.done", "item": output},
            {"type": "response.completed", "response": {"id": "response", "usage": {
                "input_tokens": tokens, "output_tokens": 5, "total_tokens": tokens + 5}}},
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


def turn(runtime, thread_id, prompt):
    started = runtime.call("turn/start", {"threadId": thread_id, "input": [{"type": "text", "text": prompt}]})["turn"]
    completed = runtime.wait(lambda event: event.get("method") == "turn/completed"
                             and event["params"]["turn"]["id"] == started["id"])["params"]["turn"]
    assert completed["status"] == "completed", completed


def checkpoints(path):
    return [json.loads(line)["payload"] for line in path.read_text().splitlines()
            if json.loads(line)["type"] == "compacted"]


def assert_plaintext_checkpoint(checkpoint):
    items = checkpoint["replacement_history"]
    assert items and all(item["type"] == "message" for item in items), checkpoint
    assert SUMMARY in json.dumps(items), checkpoint
    assert "encrypted_content" not in json.dumps(items), checkpoint


def run_case(provider, name, token_budget):
    with tempfile.TemporaryDirectory(prefix="mira-plaintext-context-") as temporary:
        home = Path(temporary)
        config = f'''model="gpt-5.1-codex"
model_provider="{name}"
model_auto_compact_token_limit=50000
compact_prompt="{PROMPT}"
[features]
plugins=false
multi_agent_v2=true
token_budget={str(token_budget).lower()}
'''
        base = f"http://127.0.0.1:{provider.server_port}/v1"
        if name == "openai":
            config = f'openai_base_url="{base}"\n' + config
            (home / "auth.json").write_text(json.dumps({"OPENAI_API_KEY": "synthetic-loopback-only"}))
        else:
            config += f'''[model_providers.{name}]
name="{'Azure' if name == 'azure' else 'Custom API'}"
base_url="{base}"
wire_api="responses"
request_max_retries=0
stream_max_retries=0
'''
        (home / "config.toml").write_text(config)
        provider.requests = []
        provider.mid_call_issued = False
        runtime = Runtime(home)
        try:
            thread = runtime.call("thread/start", {"cwd": temporary, "historyMode": "legacy",
                "approvalPolicy": "never", "sandbox": "danger-full-access"})["thread"]
            path = Path(thread["path"])
            turn(runtime, thread["id"], "Remember the task state")
            original = path.read_bytes()
            first = provider.requests[0]
            namespace = next(tool for tool in first["tools"] if tool.get("name") == "mira_collaboration")
            for tool_name in ["spawn_agent", "send_message", "followup_task"]:
                tool = next(tool for tool in namespace["tools"] if tool["name"] == tool_name)
                assert "encrypted" not in tool["parameters"]["properties"]["message"], tool
            before = len(runtime.events)
            runtime.call("thread/compact/start", {"threadId": thread["id"]})
            runtime.wait(lambda event: event.get("method") == "turn/completed"
                         and event["params"]["threadId"] == thread["id"] and event not in runtime.events[:before])
            # A following turn also synchronizes the persisted replacement history.
            turn(runtime, thread["id"], "Continue after manual compaction")
            assert_plaintext_checkpoint(checkpoints(path)[-1])
            assert path.read_bytes().startswith(original), "compaction rewrote original history"
            assert SUMMARY in json.dumps(provider.requests[-1]["input"])

            count = len(checkpoints(path))
            turn(runtime, thread["id"], "ARM_AUTO")
            turn(runtime, thread["id"], "Continue after automatic compaction")
            assert len(checkpoints(path)) == count + 1, "pre-turn compaction did not run"
            assert_plaintext_checkpoint(checkpoints(path)[-1])

            count = len(checkpoints(path))
            turn(runtime, thread["id"], "MID_AUTO")
            assert len(checkpoints(path)) == count + 1, "mid-turn compaction did not run"
            assert_plaintext_checkpoint(checkpoints(path)[-1])
            for request in provider.requests:
                assert not any(item["type"] in ("compaction", "compaction_trigger") for item in request["input"]), request
                if PROMPT in json.dumps(request["input"][-1]):
                    assert request["tools"] == [], request
            saved = path.read_bytes()
        finally:
            runtime.close()
        # A fresh process must restore the portable checkpoint, not just live memory.
        runtime = Runtime(home)
        try:
            runtime.call("thread/resume", {"threadId": thread["id"], "path": str(path), "excludeTurns": True})
            turn(runtime, thread["id"], "Continue after cold resume")
            assert SUMMARY in json.dumps(provider.requests[-1]["input"])
            assert path.read_bytes().startswith(saved), "resume rewrote original history"
        finally:
            runtime.close()
    print(f"passed: {name}, token_budget={token_budget}: default tools, manual/pre-turn/mid-turn compaction, cold resume", flush=True)


def main():
    provider = ThreadingHTTPServer(("127.0.0.1", 0), Provider)
    threading.Thread(target=provider.serve_forever, daemon=True).start()
    try:
        for name in ["openai", "azure", "custom"]:
            for budget in [False, True]:
                run_case(provider, name, budget)
    finally:
        provider.shutdown()
        provider.server_close()


if __name__ == "__main__":
    main()
