"""Resume an explicitly supplied imported desktop thread, without a model turn."""
import json
import os
from app_server_e2e import AppServer

store_id = os.environ["MIRA_DESKTOP_TEST_STORE_ID"]
thread_id = os.environ["MIRA_DESKTOP_TEST_THREAD_ID"]
endpoint = os.environ["MIRA_SERVER_URL"]
os.environ["CODEX_TEST_APP_SERVER_OVERRIDES"] = json.dumps([
    'experimental_thread_store.type="remote_http"',
    f'experimental_thread_store.endpoint="{endpoint}"',
    f'experimental_thread_store.store_id="{store_id}"',
    'experimental_thread_store.api_version="v2"',
])
app = AppServer(os.environ["MIRA_DESKTOP_TEST_CODEX_HOME"])
try:
    app.initialize()
    result = app.call(2, "thread/resume", {"threadId": thread_id}, timeout=120)
    assert result["thread"]["id"] == thread_id
    assert result["thread"].get("turns"), "resumed desktop thread has no turns"
    minimum_turns = int(os.environ.get("MIRA_DESKTOP_MIN_TURNS", "1"))
    assert len(result["thread"]["turns"]) >= minimum_turns, "ancestor turns missing from resumed fork"
    print(json.dumps({"desktopResume": True, "turns": len(result["thread"]["turns"])}))
finally:
    app.close()
