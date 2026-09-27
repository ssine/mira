"""Camoufox regression for slow/failed sidebars and late ancestor reads."""
import json
import os
from pathlib import Path
import subprocess
import time

ctl = os.environ['CAMOUFOXCTL']

def browser(*args):
    result = subprocess.run([ctl, *args], capture_output=True, text=True, check=True)
    return json.loads(result.stdout)

def evaluate(source):
    return browser('eval', source).get('value')

def wait_for(source, timeout=8):
    deadline=time.monotonic()+timeout
    while time.monotonic()<deadline:
        value=evaluate(source)
        if value:
            return value
        time.sleep(.1)
    raise AssertionError(source)

def control(**changes):
    return evaluate('async () => await (await fetch("/__test/control",{method:"POST",body:'+json.dumps(json.dumps(changes))+'})).json()')

fixture=subprocess.Popen(['node',str(Path(__file__).with_name('thread_read_latency_fixture.mjs'))],stdout=subprocess.PIPE,text=True)
opened=False
try:
    origin=fixture.stdout.readline().strip()
    browser('open', origin+'/?thread=00000000-0000-4000-8000-0000000000a1')
    opened=True
    wait_for('() => document.querySelector("#conversationTrace").textContent.includes("History for Conversation 1")')
    state=evaluate('async () => await (await fetch("/__test/control")).json()')
    assert 'roots' in state['pending'], state
    assert not any(q['limit']=='300' for q in state['requests']), state
    control(failRoots=True)
    browser('goto',origin+'/?thread=00000000-0000-4000-8000-0000000000a1')
    wait_for('() => document.querySelector("#conversationTrace").textContent.includes("History for Conversation 1")')
    evaluate('mw:document.dispatchEvent(new Event("visibilitychange"))')
    time.sleep(6)
    state=evaluate('async () => await (await fetch("/__test/control")).json()')
    assert sum(q['view']=='roots' for q in state['requests'])>=3, state
    assert not any(q['limit']=='300' for q in state['requests']), state
    control(holdPaths=True)
    browser('goto',origin+'/?thread=00000000-0000-4000-8000-0000000000a1')
    wait_for('() => document.querySelector("#conversationTrace").textContent.includes("History for Conversation 1")')
    wait_for('async () => (await (await fetch("/__test/control")).json()).pending.includes("path")')
    browser('click','button[data-thread-id="00000000-0000-4000-8000-0000000000b2"]')
    wait_for('() => document.querySelector("#conversationTrace").textContent.includes("History for Conversation 2")')
    control()
    time.sleep(.3)
    assert evaluate('() => document.querySelector("#conversationTitle").textContent')=='Conversation 2'
    assert evaluate('() => document.querySelector("#conversationTrace").textContent.includes("History for Conversation 2")')
    print('slow/failed root list, bounded retry, slow ancestor list and late selection isolation passed')
finally:
    if opened:
        browser('close-tab')
    fixture.terminate()
    fixture.wait(timeout=10)
