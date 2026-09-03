import json
import os
import queue
import subprocess
import sys
import threading
import time
import tomllib
import urllib.error
import urllib.request
from pathlib import Path


PROJECT_DIRECTORY = Path(__file__).resolve().parents[1]
BINARY = os.environ.get(
    "CODEX_TEST_BINARY",
    str(PROJECT_DIRECTORY / "codex" / "codex-rs" / "target" / "debug" / "codex"),
)
WORKSPACE = str(PROJECT_DIRECTORY / "runtime" / "workspace")
CLIENT_A = str(PROJECT_DIRECTORY / "runtime" / "client-a")
CLIENT_B = str(PROJECT_DIRECTORY / "runtime" / "client-b")
MARKER = "REMOTE_STORE_OK"


def remote_store_settings(codex_home, overrides):
    config_path = os.path.join(codex_home, "config.toml")
    config = {}
    try:
        with open(config_path, "rb") as config_file:
            config = tomllib.load(config_file).get("experimental_thread_store", {})
    except FileNotFoundError:
        pass
    settings = dict(config)
    prefix = "experimental_thread_store."
    for override in overrides:
        if not override.startswith(prefix) or "=" not in override:
            continue
        key, encoded = override[len(prefix) :].split("=", 1)
        try:
            settings[key] = tomllib.loads(f"value={encoded}")["value"]
        except tomllib.TOMLDecodeError:
            continue
    if settings.get("type") != "remote_http":
        return None
    return settings


def snapshot_has_assistant_text(payload, marker):
    histories = (payload.get("snapshot") or {}).get("histories") or {}
    for history in histories.values():
        for item in history:
            if item.get("type") != "response_item":
                continue
            response_item = item.get("payload") or {}
            if response_item.get("role") != "assistant":
                continue
            for content in response_item.get("content") or []:
                if marker in (content.get("text") or ""):
                    return True
    return False


def wait_remote_store_contains(settings, marker, timeout=30):
    if not settings:
        raise RuntimeError("the App Server is not configured with the remote HTTP thread store")
    endpoint = settings["endpoint"].rstrip("/")
    store_id = settings["store_id"]
    token = settings.get("bearer_token", "local-poc-token")
    deadline = time.monotonic() + timeout
    last_error = None
    marker_version = None
    stable_observations = 0
    while time.monotonic() < deadline:
        request = urllib.request.Request(
            f"{endpoint}/v1/stores/{store_id}",
            headers={"Authorization": f"Bearer {token}"},
        )
        try:
            with urllib.request.urlopen(request, timeout=2) as response:
                payload = json.loads(response.read().decode("utf-8"))
            if snapshot_has_assistant_text(payload, marker):
                version = payload.get("version")
                if version == marker_version:
                    stable_observations += 1
                else:
                    marker_version = version
                    stable_observations = 1
                # A completed turn is persisted by several asynchronous
                # callbacks.  Require a one-second quiescent head before an
                # execution-node handoff instead of merely seeing the answer.
                if stable_observations >= 10:
                    return True
            else:
                marker_version = None
                stable_observations = 0
        except (OSError, urllib.error.URLError) as error:
            last_error = error
        time.sleep(0.1)
    suffix = f": {last_error}" if last_error else ""
    raise RuntimeError(f"remote store did not reach the completed-turn handoff barrier{suffix}")


class AppServer:
    def __init__(self, codex_home):
        env = os.environ.copy()
        env["CODEX_HOME"] = codex_home
        arguments = [BINARY, "app-server"]
        self.overrides = json.loads(os.environ.get("CODEX_TEST_APP_SERVER_OVERRIDES", "[]"))
        self.remote_store = remote_store_settings(codex_home, self.overrides)
        for override in self.overrides:
            arguments.extend(["-c", override])
        self.process = subprocess.Popen(
            arguments,
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            bufsize=1,
            env=env,
        )
        self.messages = queue.Queue()
        self.stderr = []
        threading.Thread(target=self._read_stdout, daemon=True).start()
        threading.Thread(target=self._read_stderr, daemon=True).start()
        self.notifications = []

    def _read_stdout(self):
        for line in self.process.stdout:
            line = line.strip()
            if line:
                self.messages.put(json.loads(line))

    def _read_stderr(self):
        for line in self.process.stderr:
            self.stderr.append(line.rstrip())

    def send(self, message):
        self.process.stdin.write(json.dumps(message, separators=(",", ":")) + "\n")
        self.process.stdin.flush()

    def call(self, request_id, method, params, timeout=90):
        self.send({"method": method, "id": request_id, "params": params})
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            try:
                message = self.messages.get(timeout=min(1, deadline - time.monotonic()))
            except queue.Empty:
                if self.process.poll() is not None:
                    raise RuntimeError(
                        f"app-server exited with {self.process.returncode}: "
                        + "\n".join(self.stderr[-20:])
                    )
                continue
            if message.get("id") == request_id:
                if "error" in message:
                    raise RuntimeError(f"{method} failed: {message['error']}")
                return message["result"]
            self.notifications.append(message)
        raise TimeoutError(f"timed out waiting for {method}")

    def wait_notification(self, method, timeout=180):
        for message in self.notifications:
            if message.get("method") == method:
                return message
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            try:
                message = self.messages.get(timeout=min(1, deadline - time.monotonic()))
            except queue.Empty:
                if self.process.poll() is not None:
                    raise RuntimeError(
                        f"app-server exited with {self.process.returncode}: "
                        + "\n".join(self.stderr[-20:])
                    )
                continue
            self.notifications.append(message)
            if message.get("method") == method:
                return message
        raise TimeoutError(f"timed out waiting for {method}")

    def initialize(self):
        result = self.call(
            1,
            "initialize",
            {
                "clientInfo": {
                    "name": "codex_remote_store_poc",
                    "title": "Codex Remote Store PoC",
                    "version": "0.1.0",
                },
                "capabilities": {"experimentalApi": True},
            },
        )
        self.send({"method": "initialized"})
        return result

    def close(self):
        if self.process.poll() is None:
            # Closing stdio lets App Server drain its persistence workers.
            self.process.stdin.close()
            try:
                self.process.wait(timeout=15)
            except subprocess.TimeoutExpired:
                self.process.terminate()
                try:
                    self.process.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    self.process.kill()
                    self.process.wait(timeout=5)


def main():
    first = AppServer(CLIENT_A)
    try:
        first_initialize = first.initialize()
        started = first.call(
            2,
            "thread/start",
            {
                "cwd": WORKSPACE,
                "approvalPolicy": "never",
                "sandbox": "read-only",
            },
        )
        thread_id = started["thread"]["id"]
        turn = first.call(
            3,
            "turn/start",
            {
                "threadId": thread_id,
                "input": [
                    {
                        "type": "text",
                        "text": f"Reply with exactly {MARKER} and nothing else.",
                    }
                ],
                "approvalPolicy": "never",
            },
            timeout=120,
        )
        turn_id = turn["turn"]["id"]
        completed = first.wait_notification("turn/completed", timeout=240)
        if completed["params"]["turn"]["id"] != turn_id:
            raise RuntimeError("received completion for an unexpected turn")
        if completed["params"]["turn"].get("status") != "completed":
            raise RuntimeError(f"turn did not complete successfully: {completed['params']['turn']}")
        # The writer's in-memory thread view may lag the final persistence
        # callbacks by a few milliseconds.  The actual handoff contract is the
        # PostgreSQL barrier below, followed by recovery in a fresh process.
        handoff_settled = wait_remote_store_contains(first.remote_store, MARKER)
    finally:
        first.close()

    second = AppServer(CLIENT_B)
    try:
        second_initialize = second.initialize()
        listed = second.call(2, "thread/list", {"limit": 20})
        stored_ids = [thread["id"] for thread in listed["data"]]
        if thread_id not in stored_ids:
            raise RuntimeError("thread/list did not recover the PostgreSQL-backed thread")
        read = second.call(
            3,
            "thread/read",
            {"threadId": thread_id, "includeTurns": True},
        )
        read_json = json.dumps(read, ensure_ascii=False)
        if MARKER not in read_json:
            raise RuntimeError(
                "thread/read did not recover the completed model response: "
                + read_json[:8_000]
                + " stderr: "
                + "\n".join(second.stderr[-20:])[-4_000:]
            )
        resumed = second.call(4, "thread/resume", {"threadId": thread_id})
        if resumed["thread"]["id"] != thread_id:
            raise RuntimeError("thread/resume returned an unexpected thread")
    finally:
        second.close()

    print(
        json.dumps(
            {
                "ok": True,
                "threadId": thread_id,
                "turnId": turn_id,
                "marker": MARKER,
                "firstCodexHome": first_initialize["codexHome"],
                "secondCodexHome": second_initialize["codexHome"],
                "recoveredByList": True,
                "recoveredByRead": True,
                "resumed": True,
                "handoffSettled": handoff_settled,
            },
            ensure_ascii=False,
        )
    )


if __name__ == "__main__":
    try:
        main()
    except Exception as error:
        print(json.dumps({"ok": False, "error": str(error)}, ensure_ascii=False))
        sys.exit(1)
