"""Fork >64 MiB through a real patched App Server and disposable Mira Server.

Required: CODEX_APP_SERVER_BINARY, CONTROL_SERVER_URL, MIRA_NODE_TOKEN.
No model request or production conversation is used.
"""
import json
import os
from pathlib import Path
import queue
import subprocess
import tempfile
import threading
import time
import urllib.request
import uuid

endpoint = os.environ["CONTROL_SERVER_URL"].rstrip("/")
token = os.environ["MIRA_NODE_TOKEN"]
store = "large-fork-e2e-" + str(uuid.uuid4())


def api(path, body=None, operation=None):
    headers = {"Authorization": "Bearer " + token, "Content-Type": "application/json"}
    if operation:
        headers["X-Codex-Operation-Id"] = operation
    request = urllib.request.Request(endpoint + path, headers=headers, data=None if body is None else json.dumps(body).encode())
    with urllib.request.urlopen(request, timeout=120) as response:
        return json.load(response)


class Runtime:
    def __init__(self, home):
        self.messages = queue.Queue()
        self.progress = []
        self.next_id = 0
        self.log = open(Path(home) / "app-server.log", "a")
        self.process = subprocess.Popen([
            os.environ["CODEX_APP_SERVER_BINARY"],
            "-c", 'experimental_thread_store.type="remote_http"',
            "-c", f'experimental_thread_store.endpoint="{endpoint}"',
            "-c", f'experimental_thread_store.store_id="{store}"',
            "-c", 'features.remote_control=false',
        ], env={**os.environ, "CODEX_HOME": home}, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=self.log, text=True, bufsize=1)
        def read():
            for line in self.process.stdout:
                self.messages.put(json.loads(line))
            self.messages.put({"runtimeExited": True})
        threading.Thread(target=read, daemon=True).start()
        self.call("initialize", {"clientInfo": {"name": "mira_large_fork_test", "version": "1"}, "capabilities": {"experimentalApi": True}})
        self.process.stdin.write(json.dumps({"method": "initialized"}) + "\n")
        self.process.stdin.flush()

    def call(self, method, params):
        self.next_id += 1
        request_id = self.next_id
        self.process.stdin.write(json.dumps({"id": request_id, "method": method, "params": params}) + "\n")
        self.process.stdin.flush()
        deadline = time.monotonic() + 180
        while time.monotonic() < deadline:
            event = self.messages.get(timeout=max(1, deadline - time.monotonic()))
            if event.get("runtimeExited"):
                raise RuntimeError("App Server exited: " + (Path(self.log.name).read_text()[-2000:]))
            if event.get("method") == "mira/thread/fork/progress":
                assert event["params"]["requestId"] == request_id
                self.progress.append(event["params"])
            if event.get("id") == request_id:
                if "error" in event:
                    raise RuntimeError(event["error"])
                return event["result"]
        raise TimeoutError(method)

    def close(self):
        self.process.terminate()
        self.process.wait(timeout=15)
        self.log.close()


def main():
    with tempfile.TemporaryDirectory(prefix="mira-large-fork-") as home:
        runtime = Runtime(home)
        try:
            source = runtime.call("thread/start", {"cwd": home, "approvalPolicy": "never", "sandbox": "danger-full-access", "experimentalRawEvents": False})["thread"]["id"]
        finally:
            runtime.close()
        head = api(f"/v2/stores/{store}?threadId={source}")
        # Match the reported legacy conversation, using the same authoritative
        # state and append API as other clients; the runtime is quiescent here.
        current_mode = head["state"]["created_threads"][source].get("history_mode")
        state_changes = [{"path": ["created_threads", source, "history_mode"], "mode": "set", "conflictPolicy": "compareAndSwap",
                          "expected": {"exists": current_mode is not None, **({"value": current_mode} if current_mode is not None else {})}, "value": "legacy"}]
        total = 0
        for index in range(36):
            call_id = f"large-fork-{index}"
            records = [
                {"type": "response_item", "payload": {"type": "function_call", "call_id": call_id, "name": "fixture", "arguments": "{}"}},
                {"type": "response_item", "payload": {"type": "function_call_output", "call_id": call_id, "output": f"fork-record-{index}:" + "x" * (2 * 1024 * 1024)}},
            ]
            entry = head["historyManifest"][source]
            api(f"/v2/stores/{store}/commits", {"expectedVersion": head["version"], "stateChanges": state_changes,
                "historyChanges": [{"threadId": source, "mode": "append", "expectedGeneration": entry["generation"], "expectedItemCount": entry["itemCount"], "items": records}]}, str(uuid.uuid4()))
            state_changes = []
            total += sum(len(json.dumps(item)) for item in records)
            head = api(f"/v2/stores/{store}?threadId={source}")
        source_count = head["historyManifest"][source]["itemCount"]
        runtime = Runtime(home)
        try:
            result = runtime.call("thread/fork", {"threadId": source, "cwd": home, "excludeTurns": True, "deferGoalContinuation": True, "approvalPolicy": "never", "sandbox": "danger-full-access"})
            child = result["thread"]["id"]
            progress = runtime.progress
            assert any(p["phase"] == "uploading" and p["totalBytes"] > 64 * 1024 * 1024 for p in progress), progress
            assert all(p["threadId"] == child for p in progress if p["phase"] != "heartbeat")
        finally:
            runtime.close()
        source_after = api(f"/v2/stores/{store}?threadId={source}")
        assert source_after["historyManifest"][source]["itemCount"] == source_count
        # Read immediately after fork success. No settling sleep or repair is allowed.
        history = api(f"/v2/stores/{store}/threads/{child}/history")
        outputs = [item["payload"] for item in history["items"] if item.get("type") == "response_item" and item.get("payload", {}).get("type") == "function_call_output"]
        for index in range(36):
            match = [item for item in outputs if item.get("call_id") == f"large-fork-{index}"]
            assert len(match) == 1, index
            output = match[0]["output"]
            if isinstance(output, dict):
                output = output.get("body", output.get("content"))
            assert output == f"fork-record-{index}:" + "x" * (2 * 1024 * 1024), index
        del history, outputs
        restarted = Runtime(home)
        try:
            resumed = restarted.call("thread/resume", {"threadId": child, "cwd": home, "excludeTurns": True})
            assert resumed["thread"]["id"] == child
        finally:
            restarted.close()
        print(json.dumps({"ok": True, "historyBytes": total, "progressEvents": len(progress), "forkPersistedBeforeSuccess": True, "coldRuntimeResume": True, "storeId": store}))


if __name__ == "__main__":
    main()
