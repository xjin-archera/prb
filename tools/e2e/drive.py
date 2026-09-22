import json, sys, time, subprocess, urllib.request
import websocket  # websocket-client

CH = "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
port = 9333
proc = subprocess.Popen([CH, "--headless=new", "--disable-gpu", "--no-sandbox", f"--remote-debugging-port={port}",
                         "--window-size=1400,900", "--user-data-dir=" + sys.argv[2], "about:blank"],
                        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
try:
    for _ in range(50):
        try:
            tabs = json.load(urllib.request.urlopen(f"http://127.0.0.1:{port}/json")); break
        except Exception: time.sleep(0.2)
    page = [t for t in tabs if t["type"] == "page"][0]
    ws = websocket.create_connection(page["webSocketDebuggerUrl"], suppress_origin=True)
    mid = 0
    def call(method, **params):
        global mid; mid += 1
        ws.send(json.dumps({"id": mid, "method": method, "params": params}))
        while True:
            m = json.loads(ws.recv())
            if m.get("id") == mid: return m.get("result", m)
    def js(expr):
        r = call("Runtime.evaluate", expression=expr, awaitPromise=True, returnByValue=True)
        return r.get("result", {}).get("value", r)
    call("Page.enable"); call("Runtime.enable")
    call("Page.navigate", url=sys.argv[1]); time.sleep(2.5)
    steps = []
    def step(name, expr, wait=0.8):
        v = js(expr); time.sleep(wait); steps.append((name, v))
    step("list rows", "document.querySelectorAll('#list .pr').length")
    step("click first PR", "document.querySelector('#list .pr .title').click(); 'ok'", 1.5)
    step("detail key", "document.querySelector('#detail .dwrap')?.dataset.key")
    step("toggleChat present", "!!document.getElementById('toggleChat')")
    step("aside before", "JSON.stringify({hidden: document.getElementById('chat').hidden, len: document.getElementById('chat').innerHTML.trim().length, cls: document.body.className})")
    step("click chat", "document.getElementById('toggleChat').click(); 'ok'", 1.0)
    step("aside after", "JSON.stringify({hidden: document.getElementById('chat').hidden, len: document.getElementById('chat').innerHTML.trim().length, cls: document.body.className, width: document.getElementById('chat').getBoundingClientRect().width, hasInput: !!document.getElementById('chatInput')})")
    step("click diff tab", "[...document.querySelectorAll('.tab')].find(t=>t.textContent.trim().startsWith('diff'))?.click(); 'ok'", 1.5)
    step("diff rows / gh cards", "JSON.stringify({rows: document.querySelectorAll('tr.dl').length, gc: document.querySelectorAll('.gc').length, ic: document.querySelectorAll('.ic').length})")
    step("click line number 4 (new)", "const tds=[...document.querySelectorAll('tr.dl td.ln[hx-get]')]; tds[tds.length-1].click(); 'ok'", 1.2)
    step("new editor", "JSON.stringify({newForm: !!document.querySelector('.ic.new'), focused: document.activeElement?.tagName})")
    for n, v in steps: print(f"{n}: {v}")
    got = dict(steps)
    ok = json.loads(got["aside after"])["hidden"] is False and json.loads(got["new editor"])["newForm"] and json.loads(got["diff rows / gh cards"])["gc"] >= 1
    print("RESULT:", "PASS" if ok else "FAIL")
    shot = call("Page.captureScreenshot", format="png")["data"]
    import base64; open(sys.argv[3], "wb").write(base64.b64decode(shot))
finally:
    proc.terminate()
