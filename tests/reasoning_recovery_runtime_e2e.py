"""Conditional reasoning recovery through the canonical App Server protocol.

All ciphertext and responses are synthetic loopback fixtures. No account or
production store is used. Run with CODEX_TEST_BINARY pointing at a full package.
"""
import json
from pathlib import Path
import tempfile
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from interrupted_tool_resume_e2e import Runtime


def reasoning(item_id):
    return {"type": "reasoning", "id": item_id, "summary": [],
            "encrypted_content": "synthetic-ciphertext-" + item_id}


def message():
    return {"type": "message", "id": "msg_done", "role": "assistant",
            "content": [{"type": "output_text", "text": "RECOVERED"}]}


class Provider(BaseHTTPRequestHandler):
    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        self.server.requests.append(body)
        case = self.server.case
        ids = [item.get("id") for item in body["input"] if item["type"] == "reasoning"]
        if len(self.server.requests) == 1:
            output = [reasoning("rs_good")] + [reasoning(item_id) for item_id in case["bad_ids"]]
            if case.get("compact"):
                output.append(message())
            else:
                output.append({"type": "function_call", "id": "fc_plan", "call_id": "plan_once",
                               "name": "update_plan", "arguments": json.dumps({"plan": [
                                   {"step": "Synthetic plan", "status": "completed"}]})})
        else:
            rejected = next((item_id for item_id in case["bad_ids"] if item_id in ids), None)
            if case.get("repeat"):
                rejected = case["bad_ids"][0]
            if case.get("no_error"):
                rejected = None
            if rejected:
                error = {"code": case.get("code", "invalid_encrypted_content"), "message":
                         f"The encrypted content for item {case.get('error_id', rejected)} could not be verified. "
                         "Reason: Encrypted content could not be decrypted or parsed."}
                transport = case.get("transport", "failed")
                if isinstance(transport, int):
                    self.send_response(transport)
                    self.send_header("Content-Type", "application/json")
                    self.end_headers()
                    self.wfile.write(json.dumps({"error": error}).encode())
                    return
                if transport == "failed":
                    event = {"type": "response.failed", "response": {"error": error}}
                elif transport == "error":
                    event = {"type": "error", "error": error}
                else:
                    event = {"type": "error", **error}
                self.send_events([event])
                return
            output = [message()]
        response_id = f"response_{len(self.server.requests)}"
        self.send_events([
            {"type": "response.created", "response": {"id": response_id}},
            *[{"type": "response.output_item.done", "item": item} for item in output],
            {"type": "response.completed", "response": {"id": response_id, "usage": {
                "input_tokens": 20, "output_tokens": 5, "total_tokens": 25}}},
        ])

    def send_events(self, events):
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


def turn(runtime, thread_id):
    started = runtime.call("turn/start", {"threadId": thread_id,
        "input": [{"type": "text", "text": "Complete the synthetic plan."}]})["turn"]
    return runtime.wait(lambda event: event.get("method") == "turn/completed"
        and event["params"]["turn"]["id"] == started["id"])["params"]["turn"]


def ids(request):
    return [item.get("id") for item in request["input"] if item["type"] == "reasoning"]


def run_case(provider, case):
    provider.case, provider.requests = case, []
    with tempfile.TemporaryDirectory(prefix="mira-reasoning-recovery-") as temporary:
        home = Path(temporary)
        (home / "config.toml").write_text(f'''model="gpt-5.1-codex"
model_provider="fixture"
tools.update_plan.enabled=true
[features]
plugins=false
[model_providers.fixture]
name="Fixture"
base_url="http://127.0.0.1:{provider.server_port}/v1"
wire_api="responses"
request_max_retries=0
stream_max_retries=0
''', encoding="utf-8")
        runtime = Runtime(home)
        try:
            params = {"cwd": temporary, "historyMode": "legacy",
                      "approvalPolicy": "never", "sandbox": "danger-full-access"}
            if case.get("enabled", True):
                params["config"] = {"mira_auto_reasoning_recovery": True}
            thread = runtime.call("thread/start", params)["thread"]
            path = Path(thread["path"])
            completed = turn(runtime, thread["id"])
            if case.get("compact"):
                assert completed["status"] == "completed", completed
                before = len(runtime.events)
                runtime.call("thread/compact/start", {"threadId": thread["id"]})
                completed = runtime.wait(lambda event: event.get("method") == "turn/completed"
                    and event["params"]["threadId"] == thread["id"]
                    and event not in runtime.events[:before])["params"]["turn"]
            assert completed["status"] == case.get("status", "completed"), (case, completed)
            requests = provider.requests
            assert len(requests) == case.get("attempts", 3), (case, len(requests))
            assert ids(requests[1]) == ["rs_good", *case["bad_ids"]], (case, ids(requests[1]))
            if completed["status"] == "completed" and not case.get("no_error"):
                assert ids(requests[-1]) == ["rs_good"], (case, ids(requests[-1]))
                if not case.get("compact"):
                    outputs = [[item for item in request["input"] if item["type"] == "function_call_output"]
                               for request in requests[1:]]
                    assert len(outputs[0]) == 1 and all(output == outputs[0] for output in outputs), outputs
                    updates = [event for event in runtime.events if event.get("method") == "turn/plan/updated"]
                    assert len(updates) == 1, (updates, outputs)
            assert all(request.get("include") == ["reasoning.encrypted_content"] for request in requests), case
        finally:
            runtime.close()

        saved = path.read_bytes()
        records = [json.loads(line) for line in saved.decode("utf-8").splitlines()]
        for item_id in case["bad_ids"]:
            matching = [record["payload"] for record in records if record.get("type") == "response_item"
                        and record["payload"].get("id") == item_id]
            assert len(matching) == 1, (case, item_id, matching)
            assert {key: matching[0].get(key) for key in reasoning(item_id)} == reasoning(item_id), matching
        settings = [record["payload"]["thread_settings"].get("mira_reasoning_recovery") for record in records
                    if record.get("type") == "event_msg" and record["payload"].get("type") == "thread_settings_applied"]
        if case.get("restart"):
            assert settings[-1] == {"enabled": True, "rejected_item_ids": case["bad_ids"]}, settings
            runtime = Runtime(home)
            try:
                resume_params = {"threadId": thread["id"], "path": str(path), "excludeTurns": True}
                if case.get("disable"):
                    resume_params["config"] = {"mira_auto_reasoning_recovery": False}
                runtime.call("thread/resume", resume_params)
                assert turn(runtime, thread["id"])["status"] == "completed"
                assert ids(provider.requests[-1]) == ["rs_good"]
                # The override belongs to the resumed thread, not this process/account.
                other = runtime.call("thread/start", {"cwd": temporary, "historyMode": "legacy",
                    "approvalPolicy": "never", "sandbox": "danger-full-access"})["thread"]
                provider.requests = []
                assert turn(runtime, other["id"])["status"] == "failed"
                assert len(provider.requests) == 2
            finally:
                runtime.close()
            assert path.read_bytes().startswith(saved), "cold resume rewrote original history"
            if case.get("disable"):
                snapshots = [json.loads(line)["payload"]["thread_settings"] for line in path.read_text(encoding="utf-8").splitlines()
                             if json.loads(line).get("payload", {}).get("type") == "thread_settings_applied"]
                assert snapshots[-1]["mira_reasoning_recovery"] == {"enabled": False, "rejected_item_ids": case["bad_ids"]}
    print(f"passed: {case['name']}", flush=True)


def main():
    cases = [{"name": f"precise recovery and cold resume ({transport})", "transport": transport,
              "bad_ids": ["rs_bad"], "restart": True} for transport in [400, 409, 500, "failed", "error", "flat"]]
    cases += [
        {"name": "normal reasoning preserved", "bad_ids": ["rs_bad"], "no_error": True, "attempts": 2},
        {"name": "disabled by default", "bad_ids": ["rs_bad"], "enabled": False, "status": "failed", "attempts": 2},
        {"name": "unattributable error stops", "bad_ids": ["rs_bad"], "error_id": "rs_missing", "status": "failed", "attempts": 2},
        {"name": "unknown pool stops", "bad_ids": ["rs_bad"], "code": "unknown_reasoning_pool", "status": "failed", "attempts": 2},
        {"name": "repeated rejection stops", "bad_ids": ["rs_bad"], "repeat": True, "status": "failed"},
        {"name": "multiple rejected items", "bad_ids": ["rs_bad", "rs_bad2"], "attempts": 4, "restart": True},
        {"name": "explicit disable retains exclusions", "bad_ids": ["rs_bad"], "restart": True, "disable": True},
        {"name": "eight-recovery bound", "bad_ids": [f"rs_bad{i}" for i in range(10)], "status": "failed", "attempts": 10},
        {"name": "plaintext compaction recovery", "bad_ids": ["rs_bad"], "compact": True},
    ]
    provider = ThreadingHTTPServer(("127.0.0.1", 0), Provider)
    threading.Thread(target=provider.serve_forever, daemon=True).start()
    try:
        for case in cases:
            run_case(provider, case)
    finally:
        provider.shutdown()
        provider.server_close()
    print(f"conditional reasoning recovery: {len(cases)} scenarios passed")


if __name__ == "__main__":
    main()
