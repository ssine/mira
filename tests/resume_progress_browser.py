"""Camoufox regression: slow resume, coalescing, cancellation and disconnect."""
import json
import os
from pathlib import Path
import subprocess
import time

root = Path(__file__).resolve().parents[1]
ctl = os.environ["CAMOUFOXCTL"]


def browser(*args):
    result = subprocess.run([ctl, *args], capture_output=True, text=True)
    if result.returncode:
        raise RuntimeError(f"browser {args[0]} failed: {result.stdout} {result.stderr}")
    return json.loads(result.stdout)


def evaluate(source):
    return browser("eval", source).get("value")


def wait_for(source, timeout=15):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        value = evaluate(source)
        if value:
            return value
        time.sleep(0.1)
    raise AssertionError(source)


fixture = subprocess.Popen(["node", str(root / "tests/composer_drafts_fixture.mjs"), "0", "--resume"],
                           stdout=subprocess.PIPE, text=True)
try:
    origin = fixture.stdout.readline().strip()
    assert origin.startswith("http://127.0.0.1:")
    browser("open", origin + "/?thread=00000000-0000-4000-8000-0000000000a1")
    wait_for("() => !document.querySelector('#conversationMenuToggle').classList.contains('hidden')")
    # Accelerate the old 120 s request deadline.
    # The fixed UI keeps the same request alive beyond that deadline.
    evaluate("mw:(() => { const timeout = window.setTimeout.bind(window); window.setTimeout = (fn, ms, ...args) => timeout(fn, ms === 120000 ? 100 : ms, ...args); return true; })()")
    browser("fill", "#conversationInput", "preserved draft")
    wait_for("async () => (await (await fetch('/__test/resume')).json()).requests === 1")
    wait_for("() => document.querySelector('#resumeProgressText').textContent.includes('仍在恢复')", timeout=25)
    assert evaluate("() => { const bar = document.querySelector('#resumeProgressBar'); return bar.matches(':indeterminate') && bar.getBoundingClientRect().width > 0; }")
    browser("fill", "#conversationInput", "preserved draft edited")
    evaluate("mw:document.dispatchEvent(new Event('visibilitychange'))")
    time.sleep(1)
    state = evaluate("async () => await (await fetch('/__test/resume')).json()")
    assert state == {"requests": 1, "turns": 0, "open": [True]}, state
    assert "失败" not in evaluate("() => document.querySelector('#conversationNotice').textContent")
    browser("click", "#resumeProgressCancel")
    wait_for("() => document.querySelector('#resumeProgress').classList.contains('hidden')")
    assert evaluate("() => document.querySelector('#conversationInput').value") == "preserved draft edited"
    wait_for("async () => !(await (await fetch('/__test/resume')).json()).open[0]")
    # A cancelled connection's late success cannot satisfy a newer resume.
    browser("fill", "#conversationInput", "second draft")
    wait_for("async () => (await (await fetch('/__test/resume')).json()).requests === 2")
    evaluate("async () => { await fetch('/__test/resume',{method:'POST',body:JSON.stringify({index:0})}); return true; }")
    assert not evaluate("() => document.querySelector('#resumeProgress').classList.contains('hidden')")
    evaluate("async () => { await fetch('/__test/resume',{method:'POST',body:'{}'}); return true; }")
    wait_for("() => document.querySelector('#resumeProgress').classList.contains('hidden')")
    browser("click", "#conversationSend")
    wait_for("async () => (await (await fetch('/__test/resume')).json()).turns === 1")
    state = evaluate("async () => await (await fetch('/__test/resume')).json()")
    assert state["requests"] == 2, state
    print("resume: slow request retained, duplicate coalescing, cancellable wait, preserved draft, late-reply isolation and send after success passed")
finally:
    browser("close-tab")
    fixture.terminate()
    fixture.wait(timeout=10)
