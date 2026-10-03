"""Browser acceptance through the context-owned Camoufox daemon.
Run against the retained fixture from file_preview_e2e.mjs with
MIRA_FILE_FIXTURE=/private/task/browser-fixture.json and CAMOUFOX_CTL=... .
Does not launch another browser/profile or modify user tabs.
"""
import json
import os
from pathlib import Path
import subprocess
import time
import uuid

fixture = json.loads(Path(os.environ['MIRA_FILE_FIXTURE']).read_text())
ctl = os.environ['CAMOUFOX_CTL']

def run(*args):
    result = subprocess.run([ctl, *args], capture_output=True, text=True)
    if result.returncode: raise AssertionError(result.stdout + result.stderr)
    data = json.loads(result.stdout)
    if not data.get('ok', True):
        raise AssertionError(data)
    return data

def activate(selector):
    # Keyboard activation avoids the shared daemon's spoofed viewport exceeding
    # its screenshot/input clip. It dispatches a real trusted activation event.
    evaluate('document.querySelector('+json.dumps(selector)+')?.focus()')
    run('press','Enter')

def evaluate(expression):
    result = run('eval', expression)
    return result.get('value', result.get('result'))

def wait(expression, timeout=20):
    end = time.monotonic() + timeout
    while time.monotonic() < end:
        if evaluate(expression):
            return
        time.sleep(.2)
    raise AssertionError('browser condition timed out: ' + expression)

root = Path(fixture['root'])
thread = str(uuid.uuid4())
turn = str(uuid.uuid4())
rollout = root / 'codex-home' / 'sessions' / '2026' / '10' / '04' / f'rollout-2026-10-04-{thread}.jsonl'
rollout.parent.mkdir(parents=True, exist_ok=True)
message = '[Read document](readme.md)\n\n[Browse directory](' + str(root) + '/)\n\n[Browse ZIP](bundle.zip)\n\n[Website](site/pages/index.html)\n\n[Source](readme.md:3)'
message += '\n\n[Large source](big.txt)\n\n[Distant line](big.txt:15000)'
message += '\n\n[Website in new window](site/index.html)'
if fixture.get('media'):
    message += '\n\n[Audio](sample.wav)\n\n[Video](sample.webm)'
records = [
    {'type':'session_meta', 'payload':{'id':thread,'cwd':str(root),'source':'cli','cli_version':'0.159.3','history_mode':'paginated'},'timestamp':'2026-10-04T01:00:00Z'},
    {'type':'event_msg','payload':{'type':'task_started','turn_id':turn},'timestamp':'2026-10-04T01:00:01Z'},
    {'type':'turn_context','payload':{'turn_id':turn,'cwd':str(root)},'timestamp':'2026-10-04T01:00:02Z'},
    {'type':'event_msg','payload':{'type':'agent_message','message':message,'turn_id':turn},'timestamp':'2026-10-04T01:00:03Z'},
    {'type':'event_msg','payload':{'type':'task_complete','turn_id':turn},'timestamp':'2026-10-04T01:00:04Z'},
]
rollout.write_text(''.join(json.dumps(record) + '\n' for record in records))
baseline = {row['index'] for row in run('tabs')['tabs']}
owned = set()
try:
    own = run('open', fixture['base'])['active']; owned.add(own)
    # A disposable test account; no production cookies or credentials are read.
    wait("Boolean(document.querySelector(\"#loginView\"))")
    if evaluate("!document.querySelector('#loginView').classList.contains('hidden')"):
        run('fill', '#password', os.environ.get('MIRA_TEST_ADMIN_PASSWORD','mira-local-admin-password'))
        run('click', '#loginForm button[type=submit]')
    imported = evaluate("""async () => {
      const auth=await fetch('/v1/admin/session').then(r=>r.json());
      const r=await fetch(%s,{method:'POST',headers:{'Content-Type':'application/json','x-mira-csrf':auth.csrfToken},body:JSON.stringify({path:%s,storeId:'personal'})});
      const body=await r.json();return {ok:r.ok,status:r.status,error:body.error};
    }""" % (json.dumps('/v1/nodes/'+fixture['nodeID']+'/codex-session-imports'),json.dumps(str(rollout))))
    assert imported['ok'], imported
    run('goto', fixture['base']+'/?thread='+thread)
    wait("document.querySelector('a[data-node-file-path$=\"readme.md\"]') !== null")
    run('fill', '#conversationInput', 'Unsaved draft remains while reading files')
    activate('a[data-node-file-path$="readme.md"]')
    if evaluate("document.querySelector('#nodeFileReader')?.textContent.includes('请选择') || document.querySelector('#nodeFileReader')?.textContent.includes('请选')"):
        activate('#nodeFileReader button')
    wait("document.querySelector('#nodeFileReader h1')?.textContent === 'Preview document'")
    wait("document.querySelector('#nodeFileReader img')?.naturalWidth > 0")
    assert evaluate("document.querySelector('#nodeFileDialog').open && !document.querySelector('#nodeFileDialog').matches(':modal')")
    assert evaluate("document.querySelector('#nodeFileReader img').src.includes('/v1/files/content?')")
    evaluate("[...document.querySelectorAll('#nodeFileReader a')].find(a=>a.textContent==='Next document').focus()");run('press','Enter')
    wait("document.querySelector('#nodeFileReader h1')?.textContent === 'Second document'")
    evaluate("[...document.querySelectorAll('#nodeFileReader button')].find(a=>a.textContent==='←').focus()");run('press','Enter')
    wait("document.querySelector('#nodeFileReader h1')?.textContent === 'Preview document'")
    evaluate("[...document.querySelectorAll('#nodeFileReader button')].find(a=>a.textContent==='阅读 / 原文').focus()");run('press','Enter')
    assert evaluate("document.querySelector('#nodeFileReader .file-source-line')?.dataset.line === '1'")
    activate('#nodeFileClose')
    wait("!document.querySelector('#nodeFileDialog').open")
    assert evaluate("document.querySelector('#conversationInput').value === 'Unsaved draft remains while reading files'")
    print('Agent output → nonmodal Markdown, relative image, navigation and source: passed')

    def open_reference(selector):
        activate(selector)
        if evaluate("document.querySelector('#nodeFileReader')?.textContent.includes('请选择') || document.querySelector('#nodeFileReader')?.textContent.includes('请选')"):
            activate('#nodeFileReader button')

    open_reference('a[data-node-file-path$="big.txt"]:not([data-node-file-line])')
    wait("document.querySelector('#nodeFileReader .file-source-line') !== null")
    evaluate("[...document.querySelectorAll('#nodeFileReader button')].find(b=>b.textContent.startsWith('继续读取')).focus()")
    run('press', 'Enter')
    wait("document.querySelectorAll('#nodeFileReader .file-source-line').length > 4000")
    evaluate("document.querySelector('#nodeFileReader .file-reader-body').scrollTop=500")
    reading_top = evaluate("document.querySelector('#nodeFileReader .file-reader-body').scrollTop")
    lines = evaluate("document.querySelectorAll('#nodeFileReader .file-source-line').length")
    run('press', 'Escape')
    wait("!document.querySelector('#nodeFileDialog').open")
    open_reference('a[data-node-file-path$="big.txt"]:not([data-node-file-line])')
    wait("document.querySelectorAll('#nodeFileReader .file-source-line').length === " + str(lines))
    restored_top = evaluate("document.querySelector('#nodeFileReader .file-reader-body').scrollTop")
    assert abs(restored_top - reading_top) < 2, (reading_top, restored_top)
    activate('#nodeFileClose')
    print('Reader → incremental loading, reopening position, Escape and draft preservation: passed')

    if fixture.get('media'):
        for name, tag in [('sample.wav', 'audio'), ('sample.webm', 'video')]:
            open_reference('a[data-node-file-path$="'+name+'"]')
            wait("document.querySelector('#nodeFileReader "+tag+"')?.readyState >= 1")
            assert evaluate("document.querySelector('#nodeFileReader "+tag+"').duration > 1")
            evaluate("document.querySelector('#nodeFileReader "+tag+"').currentTime = 1")
            wait("document.querySelector('#nodeFileReader "+tag+"').currentTime >= .9")
            activate('#nodeFileClose')
        print('Audio/video → native metadata decode and seek through file streaming: passed')

    open_reference('a[data-node-file-path$="big.txt"][data-node-file-line="15000"]')
    wait("document.querySelector('#nodeFileReader [data-line=\"15000\"]') !== null")
    assert evaluate("document.querySelector('#nodeFileReader .file-source-line').dataset.line !== '1'")
    activate('#nodeFileClose')
    print('Distant source reference → bounded scan and correct line: passed')

    before = {row['index'] for row in run('tabs')['tabs']}
    open_reference('a[data-node-file-path$="site/index.html"]')
    popup = None
    for _ in range(60):
        added = [row for row in run('tabs')['tabs'] if row['index'] not in before]
        if added:
            popup = added[0]['index']; owned.add(popup); break
        time.sleep(.2)
    assert popup is not None, 'HTML click did not create an independent window'
    run('select', str(popup))
    wait("document.body.dataset.result === 'relative fetch works'",30)
    assert evaluate("window.opener === null")
    run('close-tab',str(popup)); owned.discard(popup); run('select',str(own))
    assert evaluate("document.querySelector('#conversationInput').value === 'Unsaved draft remains while reading files'")
    print('Agent HTML click → isolated new window, no opener, warm conversation draft: passed')
    run('goto',fixture['base']+'/files.html?nodeId='+fixture['nodeID']+'&path='+str(root/'site/pages/index.html')+'&root='+str(root/'site')+'&entry=pages/index.html&view=site&noauto=1')
    wait("!document.querySelector('#siteControls')?.hidden")
    before = {row['index'] for row in run('tabs')['tabs']}
    activate('#siteOpen')
    popup = None
    for _ in range(60):
        added = [row for row in run('tabs')['tabs'] if row['index'] not in before]
        if added:
            popup = added[0]['index']; owned.add(popup); break
        time.sleep(.2)
    assert popup is not None, 'Root adjustment did not create an independent window'
    run('select',str(popup))
    wait("document.body.dataset.result === 'relative fetch works'",30)
    assert evaluate("window.opener === null")
    run('close-tab',str(popup)); owned.discard(popup); run('select',str(own))
    activate('#siteStop')
    wait("document.querySelector('#siteStop').hidden")
    print('Root adjustment → isolated new window without an opener: passed')
    query='?nodeId='+fixture['nodeID']+'&path='+str(root/'bundle.zip')
    run('goto',fixture['base']+'/files.html'+query)
    wait("document.querySelectorAll('.file-tree-row').length > 0")
    evaluate("[...document.querySelectorAll('.file-tree-row button')].find(a=>a.textContent==='readme.md · #0').focus()");run('press','Enter')
    wait("document.querySelector('.file-tab-pane:not([hidden]) h1')?.textContent === 'Preview document'")
    evaluate("[...document.querySelectorAll('.file-tree-row button')].find(a=>a.textContent==='other.md · #1').focus()");run('press','Enter')
    wait("document.querySelectorAll('.file-tab').length === 2")
    assert evaluate("document.querySelector('.file-tab-pane:not([hidden]) h1')?.textContent === 'Second document'")
    print('ZIP workspace → virtual tree, relative assets and multiple tabs: passed')
    run('goto',fixture['base']+'/files.html?nodeId='+fixture['nodeID']+'&path='+str(root/'site/pages/index.html')+'&root='+str(root/'site')+'&entry=pages/index.html&view=site')
    wait("document.body.dataset.result === 'relative fetch works'",30)
    assert evaluate("location.hostname.endsWith('.preview.localhost') && typeof window.MiraAndroid === 'undefined'")
    assert evaluate("document.body.dataset.worker === 'blocked'")
    print('Isolated website → root paths, module, fetch, blocked Service Worker: passed')
finally:
    # Indices only shift when closing; close this test's tracked pages backwards.
    tabs = run('tabs')['tabs']
    for row in sorted(tabs, key=lambda row:row['index'], reverse=True):
        if row['index'] in owned:
            run('close-tab', str(row['index']))
