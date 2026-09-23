"""Release-runtime recovery of interrupted custom tools, using a loopback model.

CODEX_TEST_BINARY must name a canonical Codex entrypoint. No live store, account,
model credentials or tool side effects are used.
"""
import json
import os
from pathlib import Path
import queue
import signal
import subprocess
import tempfile
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

requests = []


class Provider(BaseHTTPRequestHandler):
    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        requests.append(body)
        item = {"type": "message", "id": "fixture-message", "role": "assistant",
                "content": [{"type": "output_text", "text": "RECOVERED"}]}
        events = [
            {"type": "response.created", "response": {"id": "fixture-response"}},
            {"type": "response.output_item.done", "item": item},
            {"type": "response.completed", "response": {"id": "fixture-response",
             "usage": {"input_tokens": 10, "output_tokens": 1, "total_tokens": 11}}},
        ]
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.end_headers()
        for event in events:
            self.wfile.write(f"event: {event['type']}\ndata: {json.dumps(event)}\n\n".encode())

    def log_message(self, *_args):
        pass


class Runtime:
    def __init__(self, home):
        self.queue = queue.Queue()
        self.events = []
        self.next_id = 0
        # Do not inherit a developer's production store/account configuration.
        env = {k: v for k, v in os.environ.items() if not k.startswith(
            ("MIRA", "CODEX", "OPENAI", "AZURE", "ANTHROPIC", "CONTROL_SERVER", "APP_SERVER", "NODE_AGENT"))}
        env.update(CODEX_HOME=str(home), RUST_LOG="warn")
        self.log = (home / "runtime.log").open("a", encoding="utf-8")
        self.process = subprocess.Popen([os.environ["CODEX_TEST_BINARY"], "app-server", "--listen", "stdio://"],
            cwd=home, env=env, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=self.log, encoding="utf-8", bufsize=1,
            start_new_session=os.name != "nt")

        def read():
            for line in self.process.stdout:
                self.queue.put(json.loads(line))
            self.queue.put({"exited": True})

        self.reader = threading.Thread(target=read, daemon=True)
        self.reader.start()
        try:
            self.call("initialize", {"clientInfo": {"name": "interrupted_tool_fixture", "version": "1"},
                                     "capabilities": {"experimentalApi": True}})
            self.process.stdin.write('{"method":"initialized"}\n')
            self.process.stdin.flush()
        except BaseException:
            self.close()
            raise

    def wait(self, predicate):
        for event in self.events:
            if predicate(event):
                return event
        deadline = time.monotonic() + 30
        while time.monotonic() < deadline:
            event = self.queue.get(timeout=max(1, deadline - time.monotonic()))
            assert not event.get("exited"), "runtime exited"
            self.events.append(event)
            if predicate(event):
                return event
        raise TimeoutError("runtime event")

    def call(self, method, params):
        self.next_id += 1
        self.process.stdin.write(json.dumps({"id": self.next_id, "method": method, "params": params}) + "\n")
        self.process.stdin.flush()
        result = self.wait(lambda event: event.get("id") == self.next_id)
        assert "error" not in result, result
        return result["result"]

    def close(self):
        # The canonical entrypoint can launch a child runtime. Terminating only
        # the launcher leaves that child holding runtime.log open on Windows.
        try:
            if os.name == "nt":
                result = subprocess.run(["taskkill", "/PID", str(self.process.pid), "/T", "/F"],
                                        capture_output=True, text=True, timeout=15)
                if result.returncode and self.process.poll() is None:
                    raise RuntimeError(f"fixture process tree cleanup failed: {result.stderr}")
            else:
                try:
                    os.killpg(self.process.pid, signal.SIGTERM)
                except ProcessLookupError:
                    pass
            try:
                self.process.wait(timeout=15)
            except subprocess.TimeoutExpired:
                if os.name != "nt":
                    os.killpg(self.process.pid, signal.SIGKILL)
                else:
                    self.process.kill()
                self.process.wait(timeout=15)
            self.reader.join(timeout=15)
            if self.reader.is_alive():
                raise RuntimeError("fixture child retained stdout after shutdown")
        finally:
            self.process.stdin.close()
            self.process.stdout.close()
            self.log.close()


def main():
    provider = ThreadingHTTPServer(("127.0.0.1", 0), Provider)
    threading.Thread(target=provider.serve_forever, daemon=True).start()
    try:
        with tempfile.TemporaryDirectory(prefix="mira-interrupted-tool-") as temporary:
            home = Path(temporary)
            (home / "config.toml").write_text(f'''model="gpt-5.1-codex"
model_provider="fixture"
[model_providers.fixture]
name="Fixture"
base_url="http://127.0.0.1:{provider.server_port}/v1"
wire_api="responses"
request_max_retries=0
stream_max_retries=0
''', encoding="utf-8")
            runtime = Runtime(home)
            try:
                thread = runtime.call("thread/start", {"cwd": temporary, "historyMode": "legacy", "approvalPolicy": "never",
                                                       "sandbox": "danger-full-access"})["thread"]
                rollout = Path(thread["path"])
                initial = runtime.call("turn/start", {"threadId": thread["id"],
                    "input": [{"type": "text", "text": "Initial fixture request"}]})["turn"]
                runtime.wait(lambda event: event.get("method") == "turn/completed"
                    and event["params"]["turn"]["id"] == initial["id"])
            finally:
                runtime.close()
            # Simulate a process stopping after a persisted call, before output.
            with rollout.open("a", encoding="utf-8") as output:
                output.write(json.dumps({"timestamp": "2026-01-01T00:00:00Z", "type": "response_item", "payload": {
                    "type": "custom_tool_call", "call_id": "interrupted-fixture", "name": "fixture", "input": "no side effects"}}) + "\n")
            original = rollout.read_bytes()
            runtime = Runtime(home)
            try:
                resumed = runtime.call("thread/resume", {"threadId": thread["id"], "path": str(rollout),
                                                        "excludeTurns": True})
                turn = runtime.call("turn/start", {"threadId": resumed["thread"]["id"],
                                                  "input": [{"type": "text", "text": "Continue"}]})["turn"]
                completed = runtime.wait(lambda event: event.get("method") == "turn/completed"
                    and event["params"]["turn"]["id"] == turn["id"])
                assert completed["params"]["turn"]["status"] == "completed", completed
                outputs = [item for item in requests[-1]["input"] if item.get("call_id") == "interrupted-fixture"
                           and item["type"] == "custom_tool_call_output"]
                assert len(outputs) == 1 and outputs[0]["output"] == "aborted", outputs
                assert rollout.read_bytes().startswith(original), "recovery rewrote original history"
            finally:
                runtime.close()
            print("interrupted custom tool: cold resume, aborted input projection, completed turn and unchanged original history passed")
    finally:
        provider.shutdown()
        provider.server_close()


if __name__ == "__main__":
    main()
