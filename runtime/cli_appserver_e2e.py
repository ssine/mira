import json
import os
import subprocess
import sys

from app_server_e2e import (
    AppServer,
    BINARY,
    CLIENT_A,
    CLIENT_B,
    WORKSPACE,
    remote_store_settings,
    wait_remote_store_contains,
)


CONTROL_URL = os.environ.get("CONTROL_SERVER_URL", "http://127.0.0.1:8787").rstrip("/")
STORE_ID = os.environ.get("CLI_E2E_STORE_ID", f"cli-appserver-e2e-{os.getpid()}")
MARKER = "CLI_APP_SERVER_SHARED_OK"
OVERRIDES = [
    f'experimental_thread_store.endpoint="{CONTROL_URL}"',
    f'experimental_thread_store.store_id="{STORE_ID}"',
]


def main():
    env = os.environ.copy()
    env["CODEX_HOME"] = CLIENT_A
    arguments = [BINARY, "exec"]
    for override in OVERRIDES:
        arguments.extend(["-c", override])
    arguments.extend(
        [
            "--json",
            "--skip-git-repo-check",
            "--sandbox",
            "read-only",
            "-C",
            WORKSPACE,
            f"Reply with exactly {MARKER} and nothing else.",
        ]
    )
    completed = subprocess.run(
        arguments,
        env=env,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        timeout=300,
        check=False,
    )
    if completed.returncode != 0:
        raise RuntimeError(
            f"codex exec failed with {completed.returncode}: {completed.stderr[-2_000:]}"
        )
    if MARKER not in completed.stdout:
        raise RuntimeError("codex exec did not produce the expected model marker")
    handoff_settled = wait_remote_store_contains(
        remote_store_settings(CLIENT_A, OVERRIDES), MARKER
    )

    previous_overrides = os.environ.get("CODEX_TEST_APP_SERVER_OVERRIDES")
    os.environ["CODEX_TEST_APP_SERVER_OVERRIDES"] = json.dumps(OVERRIDES)
    server = AppServer(CLIENT_B)
    try:
        initialized = server.initialize()
        listed = server.call(2, "thread/list", {"limit": 20})
        if len(listed["data"]) != 1:
            raise RuntimeError(
                f"expected exactly one PostgreSQL thread, found {len(listed['data'])}"
            )
        thread_id = listed["data"][0]["id"]
        read = server.call(
            3,
            "thread/read",
            {"threadId": thread_id, "includeTurns": True},
        )
        if MARKER not in json.dumps(read, ensure_ascii=False):
            raise RuntimeError("App Server did not read the CLI-created response")
        resumed = server.call(4, "thread/resume", {"threadId": thread_id})
        if resumed["thread"]["id"] != thread_id:
            raise RuntimeError("App Server resumed an unexpected thread")
    finally:
        server.close()
        if previous_overrides is None:
            os.environ.pop("CODEX_TEST_APP_SERVER_OVERRIDES", None)
        else:
            os.environ["CODEX_TEST_APP_SERVER_OVERRIDES"] = previous_overrides

    print(
        json.dumps(
            {
                "ok": True,
                "storeId": STORE_ID,
                "threadId": thread_id,
                "marker": MARKER,
                "cliCodexHome": CLIENT_A,
                "appServerCodexHome": initialized["codexHome"],
                "listedByAppServer": True,
                "readByAppServer": True,
                "resumedByAppServer": True,
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
