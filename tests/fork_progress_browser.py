"""Real Camoufox UI regression using the disposable WebSocket fixture.

Run with CAMOUFOXCTL pointing to camoufoxctl and a task-owned daemon/profile.
The fixture requires the same ws dependency as composer_drafts_browser.mjs.
"""
import json
import os
from pathlib import Path
import subprocess
import time

root = Path(__file__).resolve().parents[1]
ctl = os.environ["CAMOUFOXCTL"]


def browser(*args):
    result = subprocess.run([ctl, *args], check=True, capture_output=True, text=True)
    return json.loads(result.stdout)


def evaluate(source):
    return browser("eval", source).get("value")


def wait_for(source):
    deadline = time.monotonic() + 15
    while time.monotonic() < deadline:
        result = evaluate(source)
        if result:
            return result
        time.sleep(0.1)
    raise AssertionError(source)


def command(payload):
    return evaluate("async () => { const r = await fetch('/__test/fork', {method:'POST',body:JSON.stringify("
                    + json.dumps(payload) + ")}); return r.status; }")


fixture = subprocess.Popen(["node", str(root / "tests/composer_drafts_fixture.mjs"), "0", "--fork"], stdout=subprocess.PIPE, text=True)
try:
    origin = fixture.stdout.readline().strip()
    assert origin.startswith("http://127.0.0.1:")
    browser("open", origin + "/?thread=00000000-0000-4000-8000-0000000000a1")
    wait_for("() => !document.querySelector('#conversationMenuToggle').classList.contains('hidden')")
    for cancel in [True, False]:
        browser("click", "#conversationMenuToggle")
        browser("click", "#threadFork")
        wait_for("async () => (await (await fetch('/__test/fork')).json()).pending")
        command({"action": "progress", "wrongRequest": True})
        assert "40%" not in evaluate("() => document.querySelector('#forkProgressText').textContent")
        command({"action": "progress"})
        wait_for("() => document.querySelector('#forkProgressText').textContent.includes('40%')")
        assert evaluate("() => document.querySelector('#forkProgressBar').value") == 41943040
        command({"action": "progress", "phase": "heartbeat", "completedBytes": 0})
        assert "40%" in evaluate("() => document.querySelector('#forkProgressText').textContent")
        if cancel:
            browser("click", "#forkProgressCancel")
            wait_for("() => document.querySelector('#conversationNotice').textContent === '分支复制已取消。'")
            events = evaluate("async () => (await (await fetch('/__test/fork')).json()).events")
            assert events == ["cancel", "delete:00000000-0000-4000-8000-0000000000f1"], events
        else:
            command({"action": "progress", "phase": "committing", "completedBytes": 104857600})
            wait_for("() => document.querySelector('#forkProgressText').textContent.includes('正在保存')")
            assert evaluate("() => document.querySelector('#forkProgressCancel').hidden")
            events = evaluate("async () => (await (await fetch('/__test/fork')).json()).events")
            assert not any(event.startswith("resume:") for event in events)
            command({"action": "success"})
            wait_for("() => document.querySelector('#forkProgress').classList.contains('hidden')")
            wait_for("() => new URL(location.href).searchParams.get('thread') === '00000000-0000-4000-8000-0000000000f1'")

    print("fork progress: exact request correlation, real bytes, cancellation cleanup and durable-success gate passed")
finally:
    fixture.terminate()
    fixture.wait(timeout=10)
