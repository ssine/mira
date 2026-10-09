"""Camoufox regression: viewport content, lazy tools and retained cache history."""
import json
import os
from pathlib import Path
import subprocess
import time

ctl = os.environ["CAMOUFOXCTL"]
thread_id = "00000000-0000-4000-8000-0000000000a1"


def browser(*args):
    result = subprocess.run([ctl, *args], capture_output=True, text=True, check=True)
    return json.loads(result.stdout)


def evaluate(source):
    return browser("eval", source).get("value")


def wait_for(source, timeout=30):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        value = evaluate(source)
        if value:
            return value
        time.sleep(.1)
    state = evaluate('() => ({scrollTop:document.querySelector("#conversationScroll")?.scrollTop, '
                     'cards:[...document.querySelectorAll(".trace-card")].slice(0,5).map(c=>({key:c.dataset.traceKey,top:c.getBoundingClientRect().top}))})')
    print(evaluate('mw:globalThis.__testViewport?.()'), flush=True)
    raise AssertionError((source, state))


def control(**changes):
    return evaluate('async () => (await fetch("/__test/control", {method:"POST",body:'
                    + json.dumps(json.dumps(changes)) + '})).json()')


fixture = subprocess.Popen(["node", str(Path(__file__).with_name("transcript_cache_fixture.mjs"))], stdout=subprocess.PIPE, text=True)
opened = False
try:
    origin = fixture.stdout.readline().strip()
    browser("open", origin + "/__test/setup")
    opened = True
    evaluate('async () => { const {ClientCache}=await import("/client-cache.js"); const cache=new ClientCache(); '
             'cache.setVersion("cache-test"); cache.write("transcript:' + thread_id + '", '
             'await (await fetch("/__test/cache")).json()); await cache.flush(); }')
    browser("goto", origin + "/?thread=" + thread_id)
    wait_for('() => document.querySelector("#conversationTrace").textContent.includes("Latest reply")')
    state = evaluate('() => ({cards:document.querySelectorAll("#conversationTrace .trace-card").length, '
                     'nodes:document.querySelectorAll("#conversationTrace *").length, '
                     'renderMs:Number(document.documentElement.dataset.renderMs), '
                     'bottom:document.querySelector("#conversationScroll").scrollHeight-'
                     'document.querySelector("#conversationScroll").clientHeight-document.querySelector("#conversationScroll").scrollTop})')
    print(json.dumps(state), flush=True)
    if os.environ.get("MIRA_TEST_CACHE_BASELINE"):
        assert state["cards"] == 2400
    else:
        assert state["cards"] == 1, state
        assert state["nodes"] < 1000, state
        assert state["bottom"] < 3, state
        assert evaluate('async () => (await (await fetch("/__test/control")).json()).pending') > 0
        assert evaluate('() => document.querySelector(".tool-group-total").textContent.includes("2159")')
        assert evaluate('() => document.querySelector(".tool-group-items").childElementCount') == 0
        # The entire group is represented once, with content only near the viewport.
        evaluate('mw:(() => { document.querySelector(".tool-group").open=true; document.querySelector("#conversationScroll").scrollTop=0; })()')
        wait_for('() => document.querySelectorAll(".tool-group .trace-card").length>5')
        assert evaluate('() => document.querySelectorAll(".tool-group .trace-card").length') < 100
        assert evaluate('() => document.querySelectorAll(".tool-row-placeholder").length') == 2399
        assert evaluate('() => [...document.querySelectorAll(".tool-group .trace-body")].every(body=>!body.childNodes.length)')
        evaluate('mw:document.querySelector(".trace-card.reasoning .trace-detail").open=true')
        wait_for('() => document.querySelector(".trace-card.reasoning .trace-body strong") !== null')
        evaluate('mw:document.querySelector("#conversationScroll").scrollTop=40000')
        wait_for('() => document.querySelector(".tool-group .trace-card")?.dataset.traceKey !== "history-1" && '
                 'Number(document.querySelector(".tool-group .trace-card")?.dataset.traceKey.split("-")[1])>500', timeout=5)
        assert evaluate('() => document.querySelectorAll(".tool-group .trace-card").length') < 100
        requests = evaluate('async () => (await (await fetch("/__test/control")).json()).requests')
        assert not any("cursor" in query for query in requests), requests
        evaluate('mw:document.querySelector(".tool-group").open=false')
        wait_for('() => document.querySelector(".tool-group-items").childElementCount===0')
        wait_for('() => document.querySelector("#conversationTrace").textContent.includes("Latest reply")')
        control(holdTail=False)
        time.sleep(.3)
        assert evaluate('() => document.querySelectorAll("#conversationTrace .trace-card").length') == 1
        # Revalidation adds new output without exposing the thousands of older cards.
        control(append=True)
        browser("goto", origin + "/?thread=" + thread_id)
        wait_for('() => document.querySelector("#conversationTrace").textContent.includes("New latest reply")')
        assert evaluate('() => document.querySelectorAll("#conversationTrace .trace-card").length') == 2
        # The full saved cache is retained even though its DOM is windowed.
        evaluate('mw:document.dispatchEvent(new Event("visibilitychange"))')
        wait_for('async () => { const db=await new Promise(resolve=>{const r=indexedDB.open("mira-client-cache",1);r.onsuccess=()=>resolve(r.result)}); '
                 'return await new Promise(resolve=>{const r=db.transaction("entries").objectStore("entries").get("transcript:' + thread_id + '");'
                 'r.onsuccess=()=>resolve(r.result?.value.items.length===2401)}); }')
        # Different prose heights determine the rendered range, without a row cap.
        control(layout="prose", holdTail=True)
        browser("goto", origin + "/__test/setup")
        evaluate('async () => { const {ClientCache}=await import("/client-cache.js"); const cache=new ClientCache(); '
                 'cache.setVersion("cache-test"); cache.write("transcript:' + thread_id + '", '
                 'await (await fetch("/__test/cache")).json()); await cache.flush(); }')
        browser("goto", origin + "/?thread=" + thread_id)
        wait_for('() => document.querySelector("#conversationTrace").textContent.includes("Paragraph 299")')
        assert evaluate('() => document.querySelectorAll(".trace-card.assistant").length') == 300
        assert evaluate('() => [...document.querySelectorAll(".trace-body")].filter(body=>body.childNodes.length).length') < 10
        evaluate('mw:document.querySelector("[data-trace-key=prose-150]").scrollIntoView()')
        wait_for('() => document.querySelector("[data-trace-key=prose-150] .trace-body").textContent.includes("Paragraph 150")')
        wait_for('() => !document.querySelector("[data-trace-key=prose-299] .trace-body").childNodes.length')
        assert evaluate('() => [...document.querySelectorAll(".trace-body")].filter(body=>body.childNodes.length).length') < 10
        before = evaluate('() => document.querySelector("[data-trace-key=prose-150]").getBoundingClientRect().top')
        time.sleep(.3)
        after = evaluate('() => document.querySelector("[data-trace-key=prose-150]").getBoundingClientRect().top')
        assert abs(after - before) < 3, (before, after)
        # Native deltas still accumulate while their tool/thinking group has no DOM.
        evaluate('mw:(() => { const params={threadId:"' + thread_id + '",turnId:"live",itemId:"thought"}; '
                 '__testNotify({method:"turn/started",params:{threadId:params.threadId,turn:{id:"live"}}}); '
                 '__testNotify({method:"item/reasoning/summaryTextDelta",params:{...params,delta:"**First "}}); '
                 '__testNotify({method:"item/reasoning/summaryTextDelta",params:{...params,delta:"second**"}}); })()')
        assert evaluate('() => document.querySelector(".tool-group-items").childElementCount') == 0
        evaluate('mw:(() => { document.querySelector(".tool-group").open=true; '
                 'document.querySelector("#conversationScroll").scrollTop=document.querySelector("#conversationScroll").scrollHeight; })()')
        wait_for('() => document.querySelector(".trace-card.reasoning") !== null')
        evaluate('mw:document.querySelector(".trace-card.reasoning .trace-detail").open=true')
        wait_for('() => document.querySelector(".trace-card.reasoning .trace-body strong")?.textContent==="First second"')
        control(generation=3, holdTail=False)
        browser("goto", origin + "/?thread=" + thread_id)
        wait_for('() => document.querySelector("#conversationTrace").textContent.includes("Replacement generation")')
        assert evaluate('() => document.querySelectorAll("#conversationTrace .trace-card").length') == 1
        assert not evaluate('() => document.querySelector("#conversationTrace .history-loader") !== null')
        print("viewport prose, lazy grouped tools, stable reading position, tail merge and generation replacement passed")
finally:
    if opened:
        browser("close-tab")
    fixture.terminate()
    fixture.wait(timeout=10)
