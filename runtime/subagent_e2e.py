import json
import os
import queue
import sys
import time

from app_server_e2e import AppServer, PROJECT_DIRECTORY, WORKSPACE


CLIENT_C = str(PROJECT_DIRECTORY / "runtime" / "client-c")
CLIENT_D = str(PROJECT_DIRECTORY / "runtime" / "client-d")
CHILD_MARKER = "SUBAGENT_STORE_OK"
PARENT_MARKER = "PARENT_STORE_OK"


def child_threads(server, parent_thread_id):
    response = server.call(
        20,
        "thread/list",
        {
            "limit": 20,
            "sourceKinds": ["subAgentThreadSpawn"],
            "parentThreadId": parent_thread_id,
        },
    )
    return response["data"]


def wait_for_child(server, parent_thread_id, timeout=120):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        children = child_threads(server, parent_thread_id)
        if children:
            return children[0]
        time.sleep(1)
    raise TimeoutError("spawned child did not become visible in thread/list")


def wait_for_turn_completion(server, turn_id, timeout=420):
    for message in server.notifications:
        if (
            message.get("method") == "turn/completed"
            and message.get("params", {}).get("turn", {}).get("id") == turn_id
        ):
            return message
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        try:
            message = server.messages.get(timeout=min(1, deadline - time.monotonic()))
        except queue.Empty:
            if server.process.poll() is not None:
                raise RuntimeError(
                    f"app-server exited with {server.process.returncode}: "
                    + "\n".join(server.stderr[-20:])
                )
            continue
        server.notifications.append(message)
        if (
            message.get("method") == "turn/completed"
            and message.get("params", {}).get("turn", {}).get("id") == turn_id
        ):
            return message
    raise TimeoutError("timed out waiting for the parent turn to complete")


def assert_no_rollouts(codex_home):
    sessions = os.path.join(codex_home, "sessions")
    if os.path.exists(sessions):
        rollouts = []
        for root, _, files in os.walk(sessions):
            rollouts.extend(os.path.join(root, name) for name in files)
        if rollouts:
            raise RuntimeError(f"unexpected local rollout files: {rollouts}")


def main():
    first = AppServer(CLIENT_C)
    try:
        first_initialize = first.initialize()
        started = first.call(
            2,
            "thread/start",
            {
                "model": "gpt-5.6-sol",
                "cwd": WORKSPACE,
                "approvalPolicy": "never",
                "sandbox": "read-only",
            },
        )
        parent_thread_id = started["thread"]["id"]
        turn = first.call(
            3,
            "turn/start",
            {
                "threadId": parent_thread_id,
                "input": [
                    {
                        "type": "text",
                        "text": (
                            "This is a required remote-store integration test. Use the "
                            "collaboration.spawn_agent tool exactly once with task_name "
                            "remote_store_worker, fork_turns none, and the child message "
                            f"'Reply with exactly {CHILD_MARKER} and nothing else.' Then use "
                            "collaboration.wait_agent until that child finishes. Finally reply "
                            f"with exactly {PARENT_MARKER} and nothing else. Do not perform the "
                            "child task yourself."
                        ),
                    }
                ],
                "approvalPolicy": "never",
                "effort": "high",
            },
            timeout=120,
        )
        parent_turn_id = turn["turn"]["id"]
        wait_for_turn_completion(first, parent_turn_id)

        child = wait_for_child(first, parent_thread_id)
        child_thread_id = child["id"]
        if child.get("parentThreadId") != parent_thread_id:
            raise RuntimeError("child lost its parentThreadId before the first restart")

        parent_read = first.call(
            21,
            "thread/read",
            {"threadId": parent_thread_id, "includeTurns": True},
        )
        child_read = first.call(
            22,
            "thread/read",
            {"threadId": child_thread_id, "includeTurns": True},
        )
        if PARENT_MARKER not in json.dumps(parent_read, ensure_ascii=False):
            raise RuntimeError("parent completion marker was not persisted")
        if CHILD_MARKER not in json.dumps(child_read, ensure_ascii=False):
            raise RuntimeError("child completion marker was not persisted")
    finally:
        first.close()

    second = AppServer(CLIENT_D)
    try:
        second_initialize = second.initialize()
        roots = second.call(
            2,
            "thread/list",
            {"limit": 50},
        )["data"]
        if parent_thread_id not in [thread["id"] for thread in roots]:
            raise RuntimeError("new client could not list the persisted parent")
        children = child_threads(second, parent_thread_id)
        stored_child = next(
            (thread for thread in children if thread["id"] == child_thread_id), None
        )
        if stored_child is None:
            raise RuntimeError("new client could not list the persisted child")
        if stored_child.get("parentThreadId") != parent_thread_id:
            raise RuntimeError("new client recovered the child without its parentThreadId")

        cold_child_resume_rejected = False
        try:
            second.call(
                21,
                "thread/resume",
                {"threadId": child_thread_id, "excludeTurns": True},
            )
        except RuntimeError as error:
            message = str(error)
            if "resume the parent first" not in message:
                raise
            cold_child_resume_rejected = True
        if not cold_child_resume_rejected:
            raise RuntimeError("cold child resume unexpectedly bypassed parent ownership")

        child_read = second.call(
            22,
            "thread/read",
            {"threadId": child_thread_id, "includeTurns": True},
        )
        if CHILD_MARKER not in json.dumps(child_read, ensure_ascii=False):
            raise RuntimeError("new client could not read the persisted child history")

        resumed_parent = second.call(
            23,
            "thread/resume",
            {"threadId": parent_thread_id, "excludeTurns": True},
        )
        if resumed_parent["thread"]["id"] != parent_thread_id:
            raise RuntimeError("parent resume returned an unexpected thread")
        resumed_child = second.call(
            24,
            "thread/resume",
            {"threadId": child_thread_id, "excludeTurns": True},
        )
        if resumed_child["thread"]["id"] != child_thread_id:
            raise RuntimeError("child resume returned an unexpected thread")
    finally:
        second.close()

    assert_no_rollouts(CLIENT_C)
    assert_no_rollouts(CLIENT_D)
    print(
        json.dumps(
            {
                "ok": True,
                "parentThreadId": parent_thread_id,
                "childThreadId": child_thread_id,
                "parentTurnId": parent_turn_id,
                "parentMarker": PARENT_MARKER,
                "childMarker": CHILD_MARKER,
                "firstCodexHome": first_initialize["codexHome"],
                "secondCodexHome": second_initialize["codexHome"],
                "childParentLinkPersisted": True,
                "coldChildResumeRejected": True,
                "parentThenChildResumeSucceeded": True,
                "localRolloutFiles": 0,
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
