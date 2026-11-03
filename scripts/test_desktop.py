#!/usr/bin/env python3
"""Real Linux Tauri/WebKit smoke test using only Python's standard library.

Requires tauri-driver, WebKitWebDriver and Xvfb. Build Relay's desktop and Go
runtime bundle first, then run:
  python3 scripts/test_desktop.py --desktop /path/to/relay-desktop --bundle /path/to/bundle

Add --packaged-resources for an installed Debian package to verify Tauri's
default resource lookup instead of overriding the native runtime bundle path.

All state, browser data and shell work live in private temporary directories.
The test never changes HOME, signs into a provider, or calls an inference API.
"""

import argparse
import base64
import contextlib
import hashlib
import http.cookiejar
import json
import os
from pathlib import Path
import platform
import re
import select
import shlex
import shutil
import signal
import socket
import struct
import subprocess
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid


ELEMENT = "element-6066-11e4-a52e-4f735466cecf"
SCROLLBAR_COLORS = {"dark": "rgb(99,116,90)", "light": "rgb(120,137,107)"}


def require(condition, message):
    if not condition:
        raise AssertionError(message)


def wait_for(predicate, description, timeout=20):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        value = predicate()
        if value:
            return value
        time.sleep(0.1)
    raise AssertionError("Timed out: " + description)


def stop_process(process):
    if process is None or process.poll() is not None:
        return
    process.terminate()
    try:
        process.wait(timeout=8)
    except subprocess.TimeoutExpired:
        process.kill()
        process.wait(timeout=3)


def available_port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def tcp_ready(port):
    try:
        with socket.create_connection(("127.0.0.1", port), timeout=0.2):
            return True
    except OSError:
        return False


def unix_ready(path):
    try:
        with socket.socket(socket.AF_UNIX) as sock:
            sock.settimeout(0.2)
            sock.connect(str(path))
            return True
    except OSError:
        return False


def controller_identity(path):
    """Kernel peer PID plus creation time prevents cleanup targeting a reused PID."""
    with socket.socket(socket.AF_UNIX) as sock:
        sock.settimeout(2)
        sock.connect(str(path))
        pid, uid, _ = struct.unpack("3i", sock.getsockopt(socket.SOL_SOCKET, socket.SO_PEERCRED, 12))
    require(uid == os.getuid(), "Test controller socket is owned by another user")
    return pid, process_start_time(pid)


def process_start_time(pid):
    try:
        # The command name in parentheses can itself contain spaces.
        fields = (Path("/proc") / str(pid) / "stat").read_text().rsplit(")", 1)[1].split()
        return None if fields[0] == "Z" else fields[19]
    except FileNotFoundError:
        return None


def stop_controller(identity, state_dir):
    if identity is None:
        return
    pid, started = identity
    if started is None or process_start_time(pid) != started:
        return
    arguments = (Path("/proc") / str(pid) / "cmdline").read_bytes().split(b"\0")
    require(os.fsencode(state_dir) in arguments, "Refusing to stop a controller outside this test's state directory")
    os.kill(pid, signal.SIGTERM)
    deadline = time.monotonic() + 8
    while time.monotonic() < deadline and process_start_time(pid) == started:
        time.sleep(0.1)
    if process_start_time(pid) == started:
        os.kill(pid, signal.SIGKILL)


class WebDriver:
    def __init__(self, port, native_port):
        self.base = f"http://127.0.0.1:{port}"
        self.native_base = f"http://127.0.0.1:{native_port}"
        self.session = None
        self.opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))

    def request(self, method, path, data=None):
        body = None if data is None else json.dumps(data).encode()
        # tauri-driver only translates capabilities on POST /session. Once
        # WebKit creates the session, use its unchanged W3C API directly. This
        # removes the proxy's pooled upstream connection from subsequent commands.
        # Clicks and keystrokes with an unknown outcome are never retried.
        base = self.base if method == "POST" and path == "/session" else self.native_base
        request = urllib.request.Request(base + path, data=body, method=method,
                                         headers={"Content-Type": "application/json"})
        try:
            response = self.opener.open(request, timeout=75)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            result = json.loads(response.read(16 << 20))
        value = result.get("value")
        if isinstance(value, dict) and "error" in value:
            # Driver messages can include the current URL. Never print launch secrets.
            message = re.sub(r"token=[^\s&\"']+", "token=[redacted]", str(value.get("message", "")))
            raise RuntimeError(f"WebDriver {value['error']}: {message[:500]}")
        return value

    def start(self, application, arguments=None):
        result = self.request("POST", "/session", {"capabilities": {"alwaysMatch": {
            "tauri:options": {"application": str(application), "args": arguments or []}
        }}})
        self.session = result["sessionId"]
        self.call("POST", "/timeouts", {"script": 10000, "pageLoad": 30000, "implicit": 0})

    def call(self, method, path, data=None):
        require(self.session is not None, "No active native WebDriver session")
        return self.request(method, "/session/" + self.session + path, data)

    def script(self, source, *arguments):
        return self.call("POST", "/execute/sync", {"script": source, "args": list(arguments)})

    def async_script(self, source, *arguments):
        return self.call("POST", "/execute/async", {"script": source, "args": list(arguments)})

    def body_contains(self, text):
        return self.script("return document.body && document.body.innerText.includes(arguments[0]);", text)

    def check_frame(self):
        current = self.call("GET", "/window")
        checked = menu_checked = False
        try:
            for handle in self.call("GET", "/window/handles"):
                self.call("POST", "/window", {"handle": handle})
                if self.call("GET", "/url") == "tauri://localhost/chrome.html":
                    size = self.script("return {width:innerWidth,height:innerHeight};")
                    require(size["height"] == 40 and size["width"] >= 390,
                            "GTK custom title bar has incorrect geometry")
                    checked = True
                if self.call("GET", "/url") == "tauri://localhost/menu.html":
                    require(self.script("return [...document.querySelectorAll('[data-action]')].map(button=>button.dataset.action);") == ["reload", "reconnect", "close"],
                            "Desktop menu contains an unexpected browser action")
                    menu_checked = True
        finally:
            self.call("POST", "/window", {"handle": current})
        require(checked, "Trusted custom title bar was not created")
        require(menu_checked, "Trusted desktop-only menu was not created")

    def terminal_contains(self, text):
        return self.script("const rows = document.querySelector('.xterm-rows'); return !!rows && rows.textContent.includes(arguments[0]);", text)

    def appearance(self, preference=None):
        arguments = {} if preference is None else {"preference": preference}
        response = self.async_script("const done=arguments[arguments.length-1]; window.__TAURI_INTERNALS__.invoke('appearance',arguments[0]).then(value=>done({ok:true,value}),()=>done({ok:false}));", arguments)
        require(response.get("ok") is True, "Native appearance IPC failed")
        state = response["value"]
        require(state.get("preference") in ("system", "light", "dark") and state.get("resolved") in ("light", "dark"), "Invalid native appearance state")
        return state

    def check_appearance_views(self, state):
        current = self.call("GET", "/window")
        seen = set()
        try:
            for handle in self.call("GET", "/window/handles"):
                self.call("POST", "/window", {"handle": handle})
                url = self.call("GET", "/url")
                kind = "chrome" if url == "tauri://localhost/chrome.html" else "menu" if url == "tauri://localhost/menu.html" else "content"
                seen.add(kind)
                wait_for(lambda: self.script("return document.documentElement.dataset.theme; ") == state["resolved"], "native " + kind + " appearance")
                require(self.script("return getComputedStyle(document.documentElement).colorScheme;") == state["resolved"], "Native color-scheme disagrees with appearance")
                require(self.appearance() == state, "Native views disagree on the saved preference")
        finally:
            self.call("POST", "/window", {"handle": current})
        require(seen == {"content", "chrome", "menu"}, "Appearance check missed a native view")

    def check_appearance(self, artifacts):
        require(self.appearance()["preference"] == "system", "Isolated native appearance did not default to System")
        for preference in ("dark", "light", "system"):
            state = self.appearance(preference)
            require(state["preference"] == preference, "Native preference was not saved")
            if preference != "system":
                require(state["resolved"] == preference, "Explicit native appearance was not applied")
            else:
                expected = self.script("return matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';")
                require(state["resolved"] == expected, "Native System appearance disagrees with the OS")
            self.check_appearance_views(state)
            self.screenshot(artifacts / ("native-appearance-" + preference + ".png"))
        denied = self.async_script("const done=arguments[arguments.length-1]; window.__TAURI_INTERNALS__.invoke('frame_action',{action:'maximize'}).then(()=>done(false),()=>done(true));")
        require(denied is True, "Appearance capability granted controller window controls")
        invalid = self.async_script("const done=arguments[arguments.length-1]; window.__TAURI_INTERNALS__.invoke('appearance',{preference:'invalid'}).then(()=>done(false),()=>done(true));")
        require(invalid is True, "Native appearance accepted an invalid preference")
        self.check_appearance_views(self.appearance("light"))
        content_handle = self.call("GET", "/window")
        try:
            for page in ("index", "error"):
                self.call("POST", "/url", {"url": "tauri://localhost/" + page + ".html"})
                wait_for(lambda: self.script("return document.documentElement.dataset.theme;") == "light", "bundled " + page + " appearance")
                require(self.appearance() == {"preference": "light", "resolved": "light"}, "Bundled page could not read native appearance")
                require(self.script("return getComputedStyle(document.body).backgroundColor;") == "rgb(244, 246, 240)", "Bundled page kept its dark background")
                self.screenshot(artifacts / ("native-appearance-" + page + ".png"))
        finally:
            # Returning from the local shell crosses origins. Follow Relay's
            # actual Reconnect flow rather than weakening SameSite cookies.
            reconnected = False
            for handle in self.call("GET", "/window/handles"):
                self.call("POST", "/window", {"handle": handle})
                if self.call("GET", "/url") == "tauri://localhost/chrome.html":
                    reconnected = self.async_script("const done=arguments[arguments.length-1]; window.__TAURI_INTERNALS__.invoke('frame_action',{action:'reconnect'}).then(()=>done(true),()=>done(false));")
                    break
            self.call("POST", "/window", {"handle": content_handle})
            require(reconnected, "Trusted chrome could not reconnect after bundled appearance checks")
        wait_for(lambda: self.body_contains("Fleet overview"), "fleet restored after bundled appearance checks")
        print("PASS: native Light/Dark/System IPC, chrome/menu parity and frame permission isolation", flush=True)

    def sidebar_collapsed(self):
        return self.script("const button=document.querySelector('.attention-label'),list=document.querySelector('.attention-list');return !!button&&button.getAttribute('aria-expanded')==='false'&&list?.hidden&&getComputedStyle(list).display==='none';")

    def check_sidebar_attention(self, state, artifacts):
        button = '.attention-label'
        require(self.script("const b=document.querySelector('.attention-label');return b?.getAttribute('aria-expanded')==='true'&&document.getElementById(b.getAttribute('aria-controls'))===document.querySelector('.attention-list');"), "Native attention disclosure did not default to accessible expanded state")
        expanded = self.script("return document.querySelector('.attention-pins').getBoundingClientRect().height;")
        require(144 <= expanded <= 146, "Expanded attention no longer reserves a stable notification slot")
        self.click_css(button)
        wait_for(self.sidebar_collapsed, "native attention collapse")
        collapsed = self.script("return document.querySelector('.attention-pins').getBoundingClientRect().height;")
        require(30 <= collapsed <= 35 and expanded - collapsed >= 100, "Collapsed attention did not return space to the machine tree")
        preference_file = state / "desktop" / "sidebar.json"
        def saved(collapsed=True):
            return preference_file.exists() and json.loads(preference_file.read_text()) == {"format": 1, "collapsed": collapsed}
        wait_for(saved, "native sidebar preference durably saved")
        self.click_css(button)
        wait_for(lambda: self.script("return document.querySelector('.attention-label')?.getAttribute('aria-expanded')==='true'&&!document.querySelector('.attention-list')?.hidden;"), "native attention expands again")
        wait_for(lambda: saved(False), "expanded sidebar preference saved before next toggle")
        require(self.script("return document.querySelector('.attention-pins').getBoundingClientRect().height;") == expanded, "Attention expansion changed its reserved height")
        self.click_css(button)
        wait_for(self.sidebar_collapsed, "native attention collapses before relaunch")
        wait_for(saved, "latest native sidebar preference saved")
        rejected = self.async_script("const done=arguments[arguments.length-1];window.__TAURI_INTERNALS__.invoke('sidebar_attention',{collapsed:'yes'}).then(()=>done(false),()=>done(true));")
        require(rejected is True and saved(), "Malformed native preference changed durable sidebar state")
        self.screenshot(artifacts / "native-attention-collapsed.png")
        print("PASS: native attention disclosure, reclaimed space, durable preference and malformed IPC rejection", flush=True)

    def check_scrollbar_colors(self, artifacts, terminal=False):
        dimensions = self.script("const t=document.querySelector('.terminal-shell');return t?[t.dataset.terminalCols,t.dataset.terminalRows]:null;")
        records = []
        for theme in ("dark", "light"):
            self.appearance(theme)
            wait_for(lambda: self.script("return document.documentElement.dataset.theme;") == theme, "native scrollbar appearance")
            record = self.script("""
                const selectors=['html','.sidebar-main','.attention-list','.page-scroll','.terminal-canvas','.xterm-viewport'];
                const owners=selectors.flatMap(selector=>{const node=document.querySelector(selector);return node?[{selector,color:getComputedStyle(node).scrollbarColor}]:[];});
                const slider=document.querySelector('.terminal-canvas .xterm-scrollable-element > .scrollbar > .slider');
                return {owners,slider:slider?getComputedStyle(slider).backgroundColor:null};
            """)
            expected = SCROLLBAR_COLORS[theme]
            require(len(record["owners"]) >= 3, "Native scrollbar check missed the sidebar")
            for owner in record["owners"]:
                require(re.sub(r"\s+", "", owner["color"]) == expected + "rgba(0,0,0,0)", "Native scrollbar palette mismatch: " + owner["selector"])
            if terminal:
                require(record["slider"] and re.sub(r"\s+", "", record["slider"]) == expected, "xterm custom scrollbar missed the native palette")
            records.append({"theme": theme, **record})
            self.screenshot(artifacts / ("native-scrollbars-" + ("terminal-" if terminal else "") + theme + ".png"))
        if terminal:
            require(self.script("const t=document.querySelector('.terminal-shell');return [t.dataset.terminalCols,t.dataset.terminalRows];") == dimensions, "Color-only scrollbar changes resized the terminal")
        (artifacts / ("native-scrollbars-terminal.json" if terminal else "native-scrollbars.json")).write_text(json.dumps(records, indent=2) + "\n")
        print("PASS: native light/dark scrollbar colors" + (" including xterm with unchanged terminal dimensions" if terminal else " across application panes"), flush=True)

    def find(self, using, value):
        return self.call("POST", "/element", {"using": using, "value": value})[ELEMENT]

    def click_button(self, text):
        # All test labels are static ASCII, so no XPath interpolation of user data.
        element = self.find("xpath", f"//button[normalize-space(.)='{text}']")
        self.call("POST", f"/element/{element}/click", {})

    def click_css(self, selector):
        element = self.find("css selector", selector)
        self.call("POST", f"/element/{element}/click", {})

    def select_mastermind_view(self, section):
        require(section in ("Overview", "Pages", "Tasks", "Terminal"), "Unknown test-owned Mastermind section")
        wait_for(lambda: self.script('return [...document.querySelectorAll(".workspace-view-navigation button > span")].some(label=>label.textContent===arguments[0]);', section), "Mastermind section navigation is ready")
        element = self.find("xpath", f"//nav[@aria-label='Mastermind views']/button[span[normalize-space(.)='{section}']]")
        self.call("POST", f"/element/{element}/click", {})
        wait_for(lambda: self.script('return document.querySelector(".workspace-view-navigation button[aria-current=page] > span")?.textContent===arguments[0];', section), "Mastermind " + section + " view")

    def fill(self, selector, text, using="css selector"):
        element = self.find(using, selector)
        self.call("POST", f"/element/{element}/clear", {})
        self.call("POST", f"/element/{element}/value", {"text": text, "value": list(text)})

    def terminal_keys(self, keys):
        # xterm owns this textarea. Clearing/filling its DOM value would bypass
        # terminal editing, so send actual WebDriver key events only.
        element = self.find("css selector", ".terminal-shell textarea.xterm-helper-textarea")
        self.call("POST", f"/element/{element}/value", {"text": keys, "value": list(keys)})

    def terminal_command(self, command):
        self.terminal_keys(command + "\ue007")

    def claim_terminal(self):
        self.click_css(".terminal-shell .terminal-canvas")
        wait_for(lambda: self.body_contains("Controlling"), "terminal click acquires available writer lease")

    def check_terminal_keys(self, session):
        # Bash/readline behavior is deterministic without reading or writing the
        # user's shell settings. All commands/files belong to this test session.
        require(re.fullmatch(r"[0-9a-f]{32}", session), "Unexpected test session ID")
        self.terminal_command("exec env HISTFILE=/dev/null INPUTRC=/dev/null bash --noprofile --norc -i")
        self.terminal_command("printf 'RELAY_%s\\n' 'DIRECT_READY'")
        wait_for(lambda: self.terminal_contains("RELAY_DIRECT_READY"), "isolated readline shell")
        self.terminal_keys("printf 'RELAY_%s\\n' 'KEYS_OKX'" + "\ue012\ue003\ue014\ue007")
        wait_for(lambda: self.terminal_contains("RELAY_KEYS_OK"), "direct Left/Backspace/Right editing")
        require(not self.terminal_contains("RELAY_KEYS_OKX"), "Backspace did not edit the remote command")
        name = "relay_tab_probe_" + session + "_complete"
        self.terminal_command("printf 'RELAY_%s\\n' 'TAB_OK' > " + name)
        self.terminal_keys("cat " + name[:-4] + "\ue004\ue007")
        wait_for(lambda: self.terminal_contains("RELAY_TAB_OK"), "direct Tab filename completion")
        self.terminal_command("rm -- " + name)
        self.terminal_command("printf 'RELAY_%s\\n' 'INTERRUPT_READY'; sleep 30")
        wait_for(lambda: self.terminal_contains("RELAY_INTERRUPT_READY"), "foreground command ready for Ctrl+C")
        self.terminal_keys("\ue009c\ue000")
        self.terminal_command("printf 'RELAY_%s\\n' 'INTERRUPTED'")
        wait_for(lambda: self.terminal_contains("RELAY_INTERRUPTED"), "Ctrl+C interrupts foreground command", 5)
        print("PASS: direct terminal arrows, Backspace, Tab and Ctrl+C", flush=True)

    def check_terminal_clipboard(self):
        # Xvfb owns an isolated clipboard: no user's clipboard is read or changed.
        command = "printf 'RELAY_%s\\n' 'NATIVE_CLIPBOARD_OK'"
        copied = self.async_script("""
          const done=arguments[arguments.length-1];
          navigator.clipboard.writeText(arguments[0]).then(()=>done(true),()=>done(false));
        """, command)
        require(copied is True, "Native clipboard text write was denied")
        self.terminal_keys("\ue009v\ue000")
        wait_for(lambda: self.terminal_contains(command), "Ctrl+V pastes native clipboard text")
        require(not self.terminal_contains("RELAY_NATIVE_CLIPBOARD_OK"), "Pasting text unexpectedly submitted the command")
        self.terminal_keys("\ue007")
        wait_for(lambda: self.terminal_contains("RELAY_NATIVE_CLIPBOARD_OK"), "pasted native command executes only after Enter")
        self.terminal_command("printf '\\033[2J\\033[HRELAY_COPY_ME\\n'")
        wait_for(lambda: self.script("return document.querySelector('.xterm-rows > div')?.textContent.trim()==='RELAY_COPY_ME';"), "terminal copy fixture")
        point = self.script("const r=document.querySelector('.xterm-rows > div').getBoundingClientRect();return {x:Math.round(r.x+15),y:Math.round(r.y+r.height/2)};")
        self.call("POST", "/actions", {"actions": [{"type": "pointer", "id": "clipboard-mouse", "parameters": {"pointerType": "mouse"}, "actions": [
            {"type": "pointerMove", "duration": 0, "origin": "viewport", "x": point["x"], "y": point["y"]},
            {"type": "pointerDown", "button": 0}, {"type": "pointerUp", "button": 0},
            {"type": "pause", "duration": 80},
            {"type": "pointerDown", "button": 0}, {"type": "pointerUp", "button": 0},
        ]}]})
        wait_for(lambda: self.script("return document.querySelector('[aria-label=\"Copy terminal selection\"]')?.disabled===false;"), "terminal text selection")
        self.terminal_keys("\ue009c\ue000")
        text = self.async_script("const done=arguments[arguments.length-1];navigator.clipboard.readText().then(done,()=>done(null));")
        require(text == "RELAY_COPY_ME", "Ctrl+C did not copy the selected native terminal text")
        print("PASS: native text clipboard, Ctrl+V paste and Ctrl+C selection copy", flush=True)

    def check_terminal_image_clipboard(self, state, second, session, artifacts):
        # This method runs only inside the test's private Xvfb display. It never
        # reads, replaces, or snapshots the user's Windows/Wayland clipboard.
        pixels = [17, 93, 201, 255, 225, 41, 69, 255,
                  54, 171, 88, 255, 249, 193, 22, 255]
        copied = self.async_script("""
          const done=arguments[arguments.length-1];
          if(!navigator.clipboard?.write||typeof ClipboardItem==='undefined'){done(false);return;}
          const canvas=document.createElement('canvas');canvas.width=2;canvas.height=2;
          canvas.getContext('2d').putImageData(new ImageData(new Uint8ClampedArray(arguments[0]),2,2),0,0);
          canvas.toBlob(blob=>{if(!blob){done(false);return;}
            navigator.clipboard.write([new ClipboardItem({'image/png':blob})]).then(()=>done(true),()=>done(false));
          },'image/png');
        """, pixels)
        require(copied is True, "Native synthetic PNG clipboard write was denied")
        # If paste accidentally presses Enter, this marker executes. Its exact
        # text does not appear in the input itself, so terminal echo cannot pass.
        self.terminal_keys("printf 'RELAY_%s\\n' 'IMAGE_SUBMITTED'; : ")
        self.terminal_keys("\ue009v\ue000")
        image_root = state / "images"
        def saved_image():
            files = list(image_root.rglob("*.png")) if image_root.exists() else []
            require(len(files) <= 1, "One paste uploaded more than one fixture image")
            return files[0] if files else None
        image = wait_for(saved_image, "clipboard image saved on this terminal's machine", 30)
        require(not image.is_symlink() and image.resolve().is_relative_to(image_root.resolve()),
                "Image attachment escaped this test's private storage")
        raw = image.read_bytes()
        require(raw.startswith(b"\x89PNG\r\n\x1a\n") and len(raw) <= 10 << 20,
                "Attachment is not a bounded PNG")
        require(image.stem == hashlib.sha256(raw).hexdigest(), "Saved image checksum does not match its filename")
        decoded = self.async_script("""
          const done=arguments[arguments.length-1];
          const bytes=Uint8Array.from(atob(arguments[0]),c=>c.charCodeAt(0));
          createImageBitmap(new Blob([bytes],{type:'image/png'})).then(image=>{
            const canvas=document.createElement('canvas');canvas.width=image.width;canvas.height=image.height;
            const context=canvas.getContext('2d');context.drawImage(image,0,0);image.close();
            done({width:canvas.width,height:canvas.height,pixels:Array.from(context.getImageData(0,0,canvas.width,canvas.height).data)});
          },()=>done(null));
        """, base64.b64encode(raw).decode())
        require(decoded == {"width": 2, "height": 2, "pixels": pixels}, "Uploaded image pixels changed")
        wait_for(lambda: self.terminal_contains(str(image)), "uploaded image path is inserted into the current terminal")
        require("RELAY_IMAGE_SUBMITTED" not in second.history(session), "Image paste submitted the terminal input")
        self.screenshot(artifacts / "native-image-attachment.png")
        self.terminal_keys("\ue009c\ue000")
        self.terminal_command("printf 'RELAY_%s\\n' 'IMAGE_CANCELLED'")
        wait_for(lambda: self.terminal_contains("RELAY_IMAGE_CANCELLED"), "terminal remains usable after image paste")
        require("RELAY_IMAGE_SUBMITTED" not in second.history(session), "Image input was submitted before cancellation")
        print("PASS: private native PNG clipboard, exact uploaded pixels/checksum, terminal path insertion without Enter", flush=True)

    def check_terminal_geometry(self, artifacts):
        original = self.call("GET", "/window/rect")
        records = []
        try:
            for width, height in ((1003, 733), (1191, 863), (841, 641)):
                self.call("POST", "/window/rect", {"width": width, "height": height})
                last_geometry = None
                stable_since = 0
                def settled_geometry():
                    nonlocal last_geometry, stable_since
                    candidate = self.script("""
                    const shell=document.querySelector('.terminal-shell');
                    const mount=shell?.querySelector('.terminal-mount'), screen=shell?.querySelector('.xterm-screen');
                    if(!shell||!mount||!screen)return false;
                    const cols=Number(shell.dataset.terminalCols),rows=Number(shell.dataset.terminalRows);
                    if(!cols||!rows)return false;
                    const m=mount.getBoundingClientRect(),s=screen.getBoundingClientRect();
                    window.relayGeometryProbe={cols,rows,dpr:devicePixelRatio,window:{width:innerWidth,height:innerHeight},mount:m.toJSON(),screen:s.toJSON(),status:shell.querySelector('[aria-label="Terminal dimensions"]')?.textContent};
                    const canvas=mount.parentElement,c=canvas.getBoundingClientRect(),style=getComputedStyle(canvas);
                    if(m.right>c.right-parseFloat(style.paddingRight)+1||m.bottom>c.bottom-parseFloat(style.paddingBottom)+1||m.left<c.left+parseFloat(style.paddingLeft)-1||m.top<c.top+parseFloat(style.paddingTop)-1)return false;
                    if(c.width-parseFloat(style.paddingLeft)-parseFloat(style.paddingRight)-m.width>2||c.height-parseFloat(style.paddingTop)-parseFloat(style.paddingBottom)-m.height>2)return false;
                    if(c.right>innerWidth+1)return false;
                    if(shell.querySelector(".xterm-rows")?.children.length!==rows)return false;
                    if(!s.width||!s.height||s.left<m.left-1||s.top<m.top-1||s.right>m.right+1||s.bottom>m.bottom+1)return false;
                    const scrollbar=shell.querySelector(".xterm-scrollable-element > .scrollbar.vertical")?.getBoundingClientRect().width||0;
                    if(m.width-s.width>scrollbar+s.width/cols+2||m.height-s.height>s.height/rows+2)return false;
                    if(!shell.querySelector('[aria-label="Terminal dimensions"]')?.textContent.includes(`${cols} × ${rows}`))return false;
                    return {cols,rows,dpr:devicePixelRatio,mount:{width:m.width,height:m.height},screen:{width:s.width,height:s.height}};
                    """)
                    # Native frame layout, the ordered PTY resize acknowledgement,
                    # xterm rendering and React's dimension label settle separately.
                    # Require a stable complete frame, not one transient valid box.
                    if not candidate or candidate != last_geometry:
                        last_geometry, stable_since = candidate, time.monotonic()
                        return False
                    return candidate if time.monotonic() - stable_since >= 0.3 else False
                geometry = wait_for(settled_geometry, "native WebKit terminal fits its current viewport")
                # The expected dimensions are deliberately absent from the
                # submitted command; only the real PTY can produce this result.
                marker = "RELAY_SIZE_" + uuid.uuid4().hex[:8]
                self.terminal_command("printf '" + marker + " '; stty size")
                expected = f"{marker} {geometry['rows']} {geometry['cols']}"
                wait_for(lambda: self.terminal_contains(expected), "remote PTY and displayed terminal dimensions agree")
                geometry["window"] = {"width": width, "height": height}
                records.append(geometry)
                self.screenshot(artifacts / f"native-terminal-{width}x{height}.png")
        except Exception:
            (artifacts / "native-terminal-geometry-failure.json").write_text(json.dumps(self.script("return {probe:window.relayGeometryProbe || null,text:document.querySelector('.xterm-rows')?.textContent,cols:document.querySelector('.terminal-shell')?.dataset.terminalCols,rows:document.querySelector('.terminal-shell')?.dataset.terminalRows};"), indent=2) + "\n")
            self.screenshot(artifacts / "native-terminal-geometry-failure.png")
            raise
        finally:
            self.call("POST", "/window/rect", original)
        (artifacts / "native-terminal-geometry.json").write_text(json.dumps(records, indent=2) + "\n")
        print("PASS: native WebKit terminal viewport and real PTY geometry across odd window sizes", flush=True)

    def screenshot(self, destination):
        destination.write_bytes(base64.b64decode(self.call("GET", "/screenshot")))

    def close_window(self):
        self.call("DELETE", "/window")
        self.close()

    def close(self):
        if self.session:
            with contextlib.suppress(Exception):
                self.call("DELETE", "")
            self.session = None


class BrowserClient:
    """Independent HTTP cookie jar: it never borrows the native WebView's cookie."""
    def __init__(self, launch):
        parsed = urllib.parse.urlsplit(launch["url"])
        require(parsed.scheme == "http" and "http://" + parsed.netloc == launch["address"] and
                parsed.hostname == "127.0.0.1" and parsed.path == "/auth" and
                not parsed.username and not parsed.fragment, "Unexpected desktop handshake URL")
        self.origin = launch["address"]
        self.opener = urllib.request.build_opener(urllib.request.ProxyHandler({}),
            urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))
        self.csrf = ""
        with self.opener.open(launch["url"], timeout=10) as response:
            require(response.status == 200, "Second client could not authenticate")
        self.csrf = self.json("/api/bootstrap")["csrf"]
        try:
            with self.opener.open(launch["url"], timeout=10):
                raise AssertionError("Login link was accepted twice")
        except urllib.error.HTTPError as error:
            require(error.code == 401, "Reused login link returned an unexpected status")

    def request(self, path, method="GET", body=None):
        headers = {"Origin": self.origin}
        if body is not None:
            headers.update({"Content-Type": "application/json", "X-Relay-CSRF": self.csrf})
        elif method != "GET":
            headers["X-Relay-CSRF"] = self.csrf
        request = urllib.request.Request(self.origin + path, method=method, headers=headers,
                                         data=None if body is None else json.dumps(body).encode())
        try:
            response = self.opener.open(request, timeout=10)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            return response.status, response.read(2 << 20)

    def json(self, path):
        status, body = self.request(path)
        require(status == 200, "Second client API request failed: " + str(status))
        return json.loads(body)

    def history(self, session):
        status, body = self.request(f"/api/hosts/local/runtime/sessions/{session}/history")
        require(status == 200, "Could not read test session history")
        return body.decode(errors="replace")


def launch_handshake(binary, bundle, state, runtime, environment, listen=None):
    command = [str(binary), "desktop", "--state-dir", str(state),
               "--runtime-dir", str(runtime), "--binaries", str(bundle)]
    if listen is not None:
        command += ["--listen", listen]
    result = subprocess.run(command,
                            env=environment, capture_output=True, timeout=30, check=False)
    require(result.returncode == 0, "Desktop helper failed; stdout/stderr withheld to protect login secrets")
    require(len(result.stdout) <= 16384, "Oversized desktop handshake")
    data = json.loads(result.stdout)
    require(data.get("protocol") == 1 and isinstance(data.get("pid"), int), "Unexpected desktop protocol")
    return data


def check_detached_terminal(browser, desktop, driver_path, environment, log, second, session, artifacts):
    """Real independent Tauri process, private profile/bootstrap, same live PTY."""
    arguments = ["--session-window", "local", session]
    def spawned_identity():
        for entry in Path('/proc').iterdir():
            if not entry.name.isdigit():
                continue
            try:
                argv = (entry / 'cmdline').read_bytes().rstrip(b'\0').split(b'\0')
                if argv == [os.fsencode(desktop), *map(os.fsencode, arguments)]:
                    pid = int(entry.name)
                    return (pid, process_start_time(pid))
            except (OSError, ValueError):
                pass
        return None
    child_driver = None
    child = None
    spawned = None
    try:
        require(browser.async_script("const done=arguments[arguments.length-1];window.__TAURI_INTERNALS__.invoke('open_session_window',{hostId:'local',sessionId:arguments[0]}).then(()=>done(true),()=>done(false));", session), "Native new-window command failed")
        spawned = wait_for(spawned_identity, "native command creates an independent desktop process")
        # The direct command is verified above; a separately owned WebDriver
        # launches the same literal route so its renderer and writer can be tested.
        require(process_start_time(spawned[0]) == spawned[1], "Spawned viewer exited immediately")
        os.kill(spawned[0], signal.SIGTERM)
        wait_for(lambda: process_start_time(spawned[0]) != spawned[1], "owned command-created viewer exits")
        spawned = None
        port, native = available_port(), available_port()
        require(port != native, "Detached driver ports collided")
        child_driver = subprocess.Popen([driver_path, '--port', str(port), '--native-port', str(native)], env=environment, stdin=subprocess.DEVNULL, stdout=log, stderr=log)
        wait_for(lambda: tcp_ready(port), "detached driver ready")
        child = WebDriver(port, native)
        child.start(desktop, arguments)
        wait_for(lambda: child.script("return location.hash===arguments[0] && !!document.querySelector('.terminal-shell');", f'#host/local/session/{session}/window'), "detached route survives private authentication", 60)
        wait_for(lambda: child.body_contains('Watching') or child.body_contains('Controlling'), 'detached terminal receives writer state')
        require(child.body_contains('Watching'), "Detached viewer unexpectedly stole control")
        require(len(second.json('/api/hosts/local/runtime/sessions')) == 1, "Detached window created another terminal")
        child.click_css('.terminal-canvas')
        wait_for(lambda: child.body_contains('Another viewer') or child.body_contains('another viewer'), "detached viewer respects active writer")
        browser.click_button('Release control')
        wait_for(lambda: browser.body_contains('Watching'), "parent releases writer")
        child.click_css('.terminal-canvas')
        wait_for(lambda: child.body_contains('Controlling'), "detached viewer takes released writer")
        child.terminal_command("printf 'RELAY_%s\\n' 'DETACHED_INPUT_OK'")
        wait_for(lambda: 'RELAY_DETACHED_INPUT_OK' in second.history(session), "detached terminal inputs reach the original PTY")
        detached_artifacts = artifacts / "detached"
        detached_artifacts.mkdir(exist_ok=True)
        child.check_terminal_geometry(detached_artifacts)
        reconnect_marker = uuid.uuid4().hex
        child.script('window.relayReconnectMarker=arguments[0];', reconnect_marker)
        content = child.call('GET', '/window')
        reconnected = False
        for handle in child.call('GET', '/window/handles'):
            child.call('POST', '/window', {'handle':handle})
            if child.call('GET', '/url') == 'tauri://localhost/chrome.html':
                reconnected = child.async_script("const done=arguments[arguments.length-1];window.__TAURI_INTERNALS__.invoke('frame_action',{action:'reconnect'}).then(()=>done(true),()=>done(false));")
                break
        child.call('POST', '/window', {'handle':content})
        require(reconnected, 'Detached native reconnect failed')
        wait_for(lambda: child.script("return window.relayReconnectMarker!==arguments[1] && location.hash===arguments[0] && !!document.querySelector('.terminal-shell') && [...document.querySelectorAll('button')].some(b=>b.textContent==='Take control'&&!b.disabled);", f'#host/local/session/{session}/window', reconnect_marker), 'detached route and available writer survive reconnect', 30)
        child.claim_terminal()
        browser.close_window()
        require(child.body_contains('Controlling'), "Closing main window killed detached viewer")
        child.terminal_command("printf 'RELAY_%s\\n' 'PARENT_CLOSED_OK'")
        wait_for(lambda: 'RELAY_PARENT_CLOSED_OK' in second.history(session), "detached viewer continues after parent closure")
        child.screenshot(artifacts / 'native-detached.png')
        child.close_window()
        require(second.json('/api/hosts/local/runtime/sessions')[0]['status']=='running', "Closing detached viewer stopped its PTY")
        print('PASS: native detached process/profile, authentication route, single-writer transfer and PTY survival after both windows close', flush=True)
    finally:
        if child:
            child.close()
        stop_process(child_driver)
        if spawned and process_start_time(spawned[0]) == spawned[1]:
            os.kill(spawned[0], signal.SIGTERM)


def check_machine_resources(browser, second, artifacts):
    def matching_reading():
        view = browser.script('''
            const area=document.querySelector('[aria-label="Machine resources"]');
            if(!area)return false;
            const value=label=>area.querySelector(`[aria-label="${label}"] strong`)?.textContent;
            const cpu=value('CPU usage');if(!cpu?.endsWith('%'))return false;
            return {sampledAt:area.querySelector('time')?.dateTime,cpu,ram:value('RAM usage'),storage:value('Storage usage'),
                ramBar:parseFloat(area.querySelector('[aria-label="RAM usage"] .machine-stat-bar i')?.style.width),
                storageBar:parseFloat(area.querySelector('[aria-label="Storage usage"] .machine-stat-bar i')?.style.width)};
        ''')
        if not view:
            return False
        sample = second.json('/api/hosts/local/stats')
        if view['sampledAt'] != sample['sampledAt']:
            return False
        return view, sample
    view, sample = wait_for(matching_reading, 'native machine cards display a current two-reading CPU sample', 20)
    cpu, memory = sample['cpu'], sample['memory']
    require(sample['platform'] == 'linux' and cpu['usagePercent'] is not None and 0 <= cpu['usagePercent'] <= 100 and cpu['intervalSeconds'] > 0, 'Native CPU cards do not have measured system counters')
    require(abs(float(view['cpu'].rstrip('%').replace(',', '.')) - cpu['usagePercent']) <= 0.11, 'Native CPU text disagrees with its API sample')
    require(memory and memory['usedBytes'] == memory['totalBytes'] - memory['availableBytes'], 'Native RAM counters are inconsistent')
    root = next(item for item in sample['storage'] if item['scope'] == 'root')
    require(abs(view['ramBar'] - memory['usedBytes'] / memory['totalBytes'] * 100) < 0.001 and abs(view['storageBar'] - root['usedBytes'] / root['totalBytes'] * 100) < 0.001, 'Native capacity bars disagree with their API counters')
    browser.click_css('.machine-storage-details > summary')
    require(browser.script('return document.querySelectorAll(".machine-storage-details dl").length;') == len(sample['storage']), 'Native storage locations were combined or omitted')
    browser.screenshot(artifacts / 'native-machine-resources.png')
    (artifacts / 'native-machine-resources.json').write_text(json.dumps({'platform': sample['platform'], 'cpuMeasured': True, 'memoryAvailableSemantics': True, 'separateStorageLocations': len(sample['storage']), 'cardsMatchAPISample': True}, indent=2) + '\n')
    print('PASS: native CPU/RAM/storage cards agree with read-only OS counters, including two-read CPU and separate storage scopes', flush=True)


def check_canvas_layout(browser, second, page_id, artifacts):
    """Actual opaque document sizing; preview controls never save HTML."""
    page_path = '/api/mastermind/workspace/pages/' + page_id
    saved = second.json(page_path)
    workspace = second.json('/api/mastermind/workspace')
    route = '#mastermind/pages/' + page_id
    browser.click_css(f'[role="tab"][id="canvas-tab-{page_id}"]')
    wait_for(lambda: browser.script('return location.hash;') == route, 'selected native canvas has its own route')
    require(browser.script('return document.querySelector(".canvas-fit-width")?.getAttribute("aria-pressed");') == 'false', 'Canvas unexpectedly opted into width fitting')
    require(browser.script('return !document.querySelector(".mastermind,.terminal-shell");'), 'Pages retained coordinator setup or an interactive terminal')

    def frame_widths():
        frame = browser.call('POST', '/element', {'using': 'css selector', 'value': '.canvas-document iframe'})
        try:
            browser.call('POST', '/frame', {'id': frame})
            return browser.script('const outer=document.querySelector(".native-page-width"),card=document.querySelector(".native-card-width");return outer&&card&&{outer:outer.getBoundingClientRect().width,card:card.getBoundingClientRect().width,viewport:innerWidth};')
        finally:
            browser.call('POST', '/frame', {'id': None})

    initial = wait_for(frame_widths, 'native fixed-width authored document renders')
    require(abs(initial['outer'] - 420) < 1 and abs(initial['card'] - 120) < 1, 'Preview changed authored widths before opt-in')
    browser.script('''
        window.relayCanvasMutations=[];
        const original=window.fetch;
        window.relayRestoreCanvasFetch=()=>{window.fetch=original;delete window.relayRestoreCanvasFetch;};
        window.fetch=function(input,init){
            const method=(init?.method||(input instanceof Request?input.method:'GET')).toUpperCase();
            if(method!=='GET'&&method!=='HEAD')window.relayCanvasMutations.push(method);
            return Reflect.apply(original,this,arguments);
        };
        window.relayNativeCanvas=document.querySelector('.canvas-document iframe');
    ''')
    try:
        browser.click_css('[aria-label="Fit page width"]')
        wait_for(lambda: browser.script('return document.querySelector(".canvas-fit-width")?.getAttribute("aria-pressed");') == 'true', 'native page width fitting enabled')
        fitted = wait_for(lambda: (lambda size: size if size and size['outer'] > initial['outer'] + 100 else None)(frame_widths()), 'fitted native wrapper uses available preview width')
        require(abs(fitted['card'] - 120) < 1 and abs(fitted['outer'] - (fitted['viewport'] - 32)) < 2, 'Fit changed nested card width or failed to fill the outer page')
        browser.click_css('[aria-label="Expand page to full window"]')
        wait_for(lambda: browser.script('return location.hash;') == route + '/window', 'expanded page keeps its exact page route')
        def expanded_geometry():
            return browser.script('''
                const frame=document.querySelector('.canvas-document iframe'),r=frame?.getBoundingClientRect();
                const visible=selector=>[...document.querySelectorAll(selector)].some(node=>node.getBoundingClientRect().width>0&&node.getBoundingClientRect().height>0);
                if(!r||r.left>2||r.right<innerWidth-2||r.bottom<innerHeight-2||r.top>64||r.height<innerHeight-64)return false;
                if(visible('.sidebar,.topbar,.workspace-heading,.workspace-view-navigation,.workspace-page-navigation'))return false;
                return {x:r.x,y:r.y,width:r.width,height:r.height,viewportWidth:innerWidth,viewportHeight:innerHeight};
            ''')
        geometry = wait_for(expanded_geometry, 'expanded native iframe fills the content window below its compact controls')
        require(browser.script('return window.relayNativeCanvas===document.querySelector(".canvas-document iframe")&&window.relayNativeCanvas.getAttribute("sandbox")==="";'), 'Expansion remounted or weakened the sandboxed document')
        expanded_widths = wait_for(frame_widths, 'expanded fitted canvas renders')
        require(abs(expanded_widths['outer'] - (expanded_widths['viewport'] - 32)) < 2 and abs(expanded_widths['card'] - 120) < 1, 'Expanded fit changed inner card layout')
        browser.screenshot(artifacts / 'native-mastermind-page-expanded.png')
        browser.click_css('[aria-label="Restore page layout"]')
        wait_for(lambda: browser.script('return location.hash;') == route, 'native page restore keeps the selected page')
        require(browser.script('return document.querySelector(".workspace-page-navigation").getBoundingClientRect().width>0&&document.querySelector(".workspace-view-navigation").getBoundingClientRect().height>0;'), 'Restore did not return normal page navigation')
        browser.click_css('[aria-label="Fit page width"]')
        restored = wait_for(lambda: (lambda size: size if size and abs(size['outer'] - 420) < 1 else None)(frame_widths()), 'turning Fit off restores authored width')
        require(abs(restored['card'] - 120) < 1, 'Turning Fit off altered the nested card')
        require(browser.script('return window.relayCanvasMutations.length;') == 0, 'Canvas preview controls issued an API mutation')
        require(second.json(page_path) == saved and second.json('/api/mastermind/workspace') == workspace, 'Canvas preview controls changed saved HTML or workspace metadata')
        (artifacts / 'native-mastermind-page-layout.json').write_text(json.dumps({'authored': initial, 'fitted': fitted, 'expanded': geometry, 'restored': restored, 'savedHTMLUnchanged': True, 'apiMutations': 0}, indent=2) + '\n')
        print('PASS: native HTML page expands without remounting, fits only outer width, preserves sandbox and makes no saved-page changes', flush=True)
    finally:
        browser.script('window.relayRestoreCanvasFetch?.();')


def check_local_mastermind(browser, second, state, artifacts):
    # The disposable codex executable below is an interactive /bin/sh fixture.
    # Exercise the agent's private CLI capability from its actual PTY; no account
    # login, provider inference or direct editing of the workspace store.
    configuration = second.json('/api/mastermind')
    require(configuration.get('local', {}).get('hostId') == 'local', 'Local Mastermind default is missing')
    status, _ = second.request('/api/mastermind/config', 'POST', {'revision':configuration['revision'], 'hostId':'local', 'harness':'codex', 'agentMode':'manual', 'cwd':configuration['local']['cwd']})
    require(status == 202, 'Could not configure the disposable local coordinator')
    live = wait_for(lambda: (lambda record: record if record.get('status') == 'running' else None)(second.json('/api/mastermind')), 'local Mastermind attaches', 30)
    browser.call('POST', '/url', {'url':second.origin+'/#mastermind'})
    wait_for(lambda: browser.script('return document.querySelector(".workspace-view-navigation button[aria-current=page] > span")?.textContent==="Overview";'), 'local Mastermind overview')
    require(browser.script('return !document.querySelector(".terminal-shell");'), 'Mastermind overview unexpectedly mounted a terminal')
    browser.select_mastermind_view('Terminal')
    wait_for(lambda: browser.terminal_contains('RELAY_LOCAL_COORDINATOR_READY'), 'dedicated native Mastermind renderer')
    browser.claim_terminal()
    nested = artifacts / 'mastermind'
    nested.mkdir(exist_ok=True)
    browser.check_terminal_geometry(nested)
    cols = browser.script('return Number(document.querySelector(".terminal-shell").dataset.terminalCols);')
    require(cols > 40, 'Wide Mastermind panel still renders a tiny terminal grid')
    fixture_html = '<main class="native-page-width" style="width:420px;max-width:420px;margin:0 auto"><h1>Native canvas proof</h1><p>Published from the local coordinator.</p><aside class="native-card-width" style="width:120px">Nested card</aside><div style="height:1800px"></div><p>End of native canvas proof</p></main>'
    browser.terminal_command("printf '%s' " + shlex.quote(fixture_html) + ' > native-plan.html')
    browser.terminal_command('"$RELAY_BIN" workspace pages create --revision 0 --title "Native plan" --html-file native-plan.html')
    workspace = wait_for(lambda: (lambda record: record if record.get('revision') == 1 else None)(second.json('/api/mastermind/workspace')), 'agent CLI publishes HTML page')
    browser.terminal_command('"$RELAY_BIN" workspace tasks create --revision 1 --title "Verify native terminal" --status done --detail "Real PTY and maximum whole-row geometry passed"')
    wait_for(lambda: second.json('/api/mastermind/workspace').get('revision') == 2, 'agent CLI publishes task')
    browser.terminal_command("printf 'RELAY_%s\\n' 'MASTERMIND_VIEW_RETURN'")
    wait_for(lambda: browser.terminal_contains('RELAY_MASTERMIND_VIEW_RETURN'), 'coordinator marker before changing views')
    browser.select_mastermind_view('Pages')
    require(browser.script('return !document.querySelector(".terminal-shell");'), 'Pages view unexpectedly retained an interactive terminal')
    wait_for(lambda: browser.script("return [...document.querySelectorAll('[role=tab]')].some(t=>t.textContent==='Native plan');"), 'native pages view lists the agent-authored page')
    sandbox = wait_for(lambda: browser.script('const frame=document.querySelector("iframe");return frame && {sandbox:frame.getAttribute("sandbox"),srcdoc:frame.getAttribute("srcdoc")};'), 'native HTML canvas loads')
    require(sandbox and sandbox['sandbox'] == '' and 'Native canvas proof' in sandbox['srcdoc'], 'HTML canvas is missing or unsandboxed')
    for theme in ('dark', 'light'):
        browser.appearance(theme)
        wait_for(lambda: browser.script('return document.documentElement.dataset.theme;') == theme, 'native canvas theme')
        wait_for(lambda: browser.script('return document.querySelector("iframe")?.getAttribute("srcdoc")?.includes("color-scheme:"+arguments[0]+";");', theme), 'new canvas document adopts the current theme')
        # The sandbox is opaque to the app. WebDriver enters the real frame;
        # do not infer rendering or scroll behavior from its srcdoc string.
        frame = browser.call('POST', '/element', {'using':'css selector', 'value':'iframe'})
        try:
            browser.call('POST', '/frame', {'id':frame})
            wait_for(lambda: browser.script("const h=document.querySelector('h1');return h?.textContent==='Native canvas proof'&&h.getBoundingClientRect().height>0&&document.readyState==='complete';"), 'native sandboxed canvas actually renders its document')
            wait_for(lambda: browser.script('return getComputedStyle(document.documentElement).colorScheme;') == theme, 'native canvas resolved palette')
            color = browser.script('return getComputedStyle(document.documentElement).scrollbarColor;')
            require(re.sub(r'\s+', '', color) == SCROLLBAR_COLORS[theme] + 'rgba(0,0,0,0)', 'Canvas scrollbar palette disagrees with Relay')
            browser.script('window.scrollTo(0,document.documentElement.scrollHeight);')
            wait_for(lambda: browser.script("const end=document.querySelector('.native-page-width > p:last-child');return document.scrollingElement.scrollTop>0&&end?.textContent==='End of native canvas proof'&&end.getBoundingClientRect().bottom<=innerHeight+1;"), 'last canvas content reachable by native scrolling')
            browser.script('window.scrollTo(0,0);')
        finally:
            browser.call('POST', '/frame', {'id':None})
    browser.screenshot(artifacts / 'native-mastermind-pages.png')
    check_canvas_layout(browser, second, workspace['pages'][0]['id'], artifacts)
    browser.select_mastermind_view('Tasks')
    wait_for(lambda: browser.script('return document.querySelector(\'article[aria-label="Verify native terminal"] select\')?.value==="done";'), 'native tasks view shows the agent-authored completed task')
    require(browser.script('return !document.querySelector(".terminal-shell,.canvas-document iframe");'), 'Tasks view unexpectedly retained another workspace view')
    browser.screenshot(artifacts / 'native-mastermind-tasks.png')
    browser.select_mastermind_view('Terminal')
    wait_for(lambda: browser.terminal_contains('RELAY_MASTERMIND_VIEW_RETURN'), 'returning from pages and tasks replays the original terminal screen')
    after = second.json('/api/mastermind')
    require(after['session']['id'] == live['session']['id'] and after['session']['createdAt'] == live['session']['createdAt'], 'Changing views replaced the local coordinator')
    require(len(second.json('/api/hosts/local/runtime/sessions')) == 2, 'Mastermind created duplicate sessions')
    browser.screenshot(artifacts / 'native-mastermind-workspace.png')
    status, _ = second.request('/api/mastermind/disable', 'POST', {'revision':live['revision']})
    require(status == 202, 'Could not pause disposable Mastermind')
    status, _ = second.request('/api/hosts/local/runtime/sessions/'+live['session']['id'], 'DELETE')
    require(status == 204, 'Could not remove disposable coordinator')
    print('PASS: native Mastermind terminal geometry, separate pages/tasks views, private agent CLI and persistent coordinator', flush=True)


def check_terminal_panes(browser, second, original, artifacts):
    """Four disposable real PTYs: independent input, native layout and view-only close."""
    originals = second.json('/api/hosts/local/runtime/sessions')
    first = next(item for item in originals if item['id'] == original)
    records = [first]
    added = []
    pane = lambda session: f'.terminal-pane[data-pane-session="{session}"]'
    def text(session):
        return browser.script('return document.querySelector(arguments[0]+" .xterm-rows")?.textContent||"";', pane(session))
    def controlled(session):
        return browser.script('return document.querySelector(arguments[0]+" .terminal-link")?.textContent.trim()==="Controlling";', pane(session))
    def send(session, command):
        element = browser.find('css selector', pane(session) + ' textarea.xterm-helper-textarea')
        keys = command + '\ue007'
        browser.call('POST', f'/element/{element}/value', {'text': keys, 'value': list(keys)})
    def geometry(session=None):
        return browser.script('''
            const panes=[...document.querySelectorAll('.terminal-pane:not([hidden])')].filter(pane=>!arguments[0]||pane.dataset.paneSession===arguments[0]);
            if(!panes.length)return false;
            const result=[];
            for(const pane of panes){
                const shell=pane.querySelector('.terminal-shell'),canvas=pane.querySelector('.terminal-canvas'),screen=pane.querySelector('.xterm-screen');
                if(!shell||!canvas||!screen)return false;
                const cols=Number(shell.dataset.terminalCols),rows=Number(shell.dataset.terminalRows),b=canvas.getBoundingClientRect(),s=screen.getBoundingClientRect(),css=getComputedStyle(canvas);
                if(!cols||!rows||pane.querySelectorAll('.xterm-rows > div').length!==rows||!s.width||!s.height)return false;
                if(s.left<b.left+parseFloat(css.paddingLeft)-1||s.top<b.top+parseFloat(css.paddingTop)-1||s.right>b.left+canvas.clientWidth-parseFloat(css.paddingRight)+1||s.bottom>b.top+canvas.clientHeight-parseFloat(css.paddingBottom)+1)return false;
                result.push({id:pane.dataset.paneSession,cols,rows});
            }
            return result;
        ''', session)
    try:
        for index in range(2, 5):
            status, body = second.request('/api/hosts/local/runtime/sessions', 'POST', {'harness': 'shell', 'title': f'Native pane {index}', 'workspace': 'Native pane proof', 'cwd': first['cwd']})
            require(status == 201, 'Could not create test-owned pane session')
            created = json.loads(body)
            require(re.fullmatch(r'[0-9a-f]{32}', created['id']), 'Unexpected native pane session ID')
            added.append(created); records.append(created)
        browser.call('POST', '/url', {'url': second.origin + '/#host/local/session/' + original})
        wait_for(lambda: browser.script('return document.querySelectorAll(".terminal-pane .xterm").length;') == 1, 'original pane restored')
        for index, record in enumerate(added, 2):
            browser.click_css(pane(original) + ' [aria-label="Add terminal pane"]')
            browser.click_css(f'[aria-label="{record["title"]} on This computer"]')
            wait_for(lambda: browser.script('return document.querySelectorAll(".terminal-pane .xterm").length;') == index, 'next real PTY pane attached')
        browser.script('window.relayNativePaneNodes=[...document.querySelectorAll(".terminal-pane .xterm")];')
        for index, record in enumerate(records, 1):
            browser.click_css(pane(record['id']) + ' .terminal-canvas')
            wait_for(lambda: controlled(record['id']), 'pane owns only its terminal lease')
            wait_for(lambda: geometry(record['id']), 'controlled native grid fits its pane')
            send(record['id'], f"printf 'RELAY_PANE_%s ' '{index}_OWNED'; stty size")
            wait_for(lambda: f'RELAY_PANE_{index}_OWNED ' in second.history(record['id']), 'scoped pane PTY input and size output')
        sizes = wait_for(geometry, 'four fitted native grids after control acknowledgements')
        require(len(sizes) == 4, 'Native workspace did not retain all four panes')
        for index, record in enumerate(records, 1):
            output = second.history(record['id'])
            size = next(item for item in sizes if item['id'] == record['id'])
            require(re.search(rf'RELAY_PANE_{index}_OWNED\s+{size["rows"]}\s+{size["cols"]}(?:\r?\n)', output), 'Real PTY size disagrees with its native pane')
            require(all(f'RELAY_PANE_{other}_OWNED ' not in output for other in range(1, 5) if other != index), 'Input leaked into another native pane')
        browser.screenshot(artifacts / 'native-four-panes.png')

        watched = records[-1]['id']
        browser.click_css(pane(watched) + ' .terminal-toolbar > .text-button:last-child')
        wait_for(lambda: browser.script('return document.querySelector(arguments[0]+" .terminal-link")?.textContent.trim()==="Watching";', pane(watched)), 'fourth pane releases control')
        status, _ = second.request(f'/api/hosts/local/runtime/sessions/{watched}/input', 'POST', {'data': "i=0; while [ \"$i\" -lt 120 ]; do printf 'NATIVE_SCROLL_%03d\\n' \"$i\"; i=$((i+1)); done; printf 'SCROLL_%s\\n' 'DONE'\r"})
        require(status == 204, 'Could not write test-owned scrollback after releasing its lease')
        wait_for(lambda: 'SCROLL_DONE' in text(watched), 'scrollback output reaches the fourth pane')
        scroll_geometry = browser.script('const p=document.querySelector(arguments[0]);const t=p.querySelector(".terminal-shell"),bar=p.querySelector(".scrollbar.vertical").getBoundingClientRect(),thumb=p.querySelector(".scrollbar.vertical > .slider").getBoundingClientRect();return {cols:t.dataset.terminalCols,rows:t.dataset.terminalRows,x:thumb.x+thumb.width/2,from:thumb.y+thumb.height/2,to:bar.y+thumb.height/2};', pane(watched))
        browser.call('POST', '/actions', {'actions': [{'type': 'pointer', 'id': 'native-pane-scroll', 'parameters': {'pointerType': 'mouse'}, 'actions': [
            {'type': 'pointerMove', 'duration': 0, 'x': round(scroll_geometry['x']), 'y': round(scroll_geometry['from'])},
            {'type': 'pause', 'duration': 150}, {'type': 'pointerDown', 'button': 0},
            {'type': 'pointerMove', 'duration': 250, 'x': round(scroll_geometry['x']), 'y': round(scroll_geometry['to'])},
            {'type': 'pointerUp', 'button': 0}]}]})
        wait_for(lambda: 'NATIVE_SCROLL_000' in text(watched), 'actual native scrollbar drag reveals historical output')
        require(not controlled(watched), 'Dragging a native scrollbar claimed the terminal writer')
        require(browser.script('const t=document.querySelector(arguments[0]+" .terminal-shell");return [t.dataset.terminalCols,t.dataset.terminalRows];', pane(watched)) == [scroll_geometry['cols'], scroll_geometry['rows']], 'Scrollbar drag resized the watched PTY')

        hidden_sizes = {record['id']: browser.script('const t=document.querySelector(arguments[0]+" .terminal-shell");return [t.dataset.terminalCols,t.dataset.terminalRows];', pane(record['id'])) for record in records[1:]}
        before_zoom = browser.script('return Number(document.querySelector(arguments[0]+" .terminal-shell").dataset.terminalCols);', pane(original))
        browser.click_css(pane(original) + ' [aria-label="Expand terminal to full window"]')
        wait_for(lambda: browser.script('return document.querySelectorAll(".terminal-pane:not([hidden])").length;') == 1, 'one pane zoomed')
        wait_for(lambda: browser.script('return Number(document.querySelector(arguments[0]+" .terminal-shell").dataset.terminalCols);', pane(original)) > before_zoom, 'zoomed owner receives its expanded PTY geometry')
        require(browser.script('return window.relayNativePaneNodes.every(node=>node.isConnected)&&document.querySelectorAll(".terminal-pane[hidden][inert]").length===3;'), 'Zoom remounted or left background panes interactive')
        for session, size in hidden_sizes.items():
            require(browser.script('const t=document.querySelector(arguments[0]+" .terminal-shell");return [t.dataset.terminalCols,t.dataset.terminalRows];', pane(session)) == size, 'Zoom resized a hidden PTY to zero')
        browser.click_css(pane(original) + ' [aria-label="Restore workspace layout"]')
        wait_for(lambda: browser.script('return document.querySelectorAll(".terminal-pane:not([hidden])").length;') == 4, 'four-pane layout restored')
        wait_for(geometry, 'restored native grids fit again')
        browser.click_css(pane(watched) + ' [aria-label="Close pane"]')
        wait_for(lambda: browser.script('return document.querySelectorAll(".terminal-pane").length;') == 3, 'one viewer closed')
        alive = second.json('/api/hosts/local/runtime/sessions')
        require(all(any(item['id'] == record['id'] and item['createdAt'] == record['createdAt'] and item['status'] == 'running' for item in alive) for record in records), 'Closing a pane removed or replaced a real session')
        require(browser.script('return window.relayNativePaneNodes.slice(0,3).every(node=>node.isConnected);'), 'Closing one pane remounted another')
        (artifacts / 'native-four-panes.json').write_text(json.dumps({'geometry': sizes, 'scrollbarDidNotClaim': True, 'zoomKeptFourMounted': True, 'closedPaneKeptAllSessions': True}, indent=2) + '\n')
        print('PASS: four real native PTY panes isolate input, fit geometry, scroll without claiming, preserve mounts on zoom and keep sessions on view close', flush=True)
    finally:
        for record in added:
            with contextlib.suppress(Exception):
                second.request('/api/hosts/local/runtime/sessions/' + record['id'], 'DELETE')


def run(args):
    require(platform.system() == "Linux", "Native smoke currently requires Linux WebKitWebDriver")
    architecture = {"x86_64": "amd64", "aarch64": "arm64"}.get(platform.machine())
    require(architecture is not None, "Unsupported desktop test architecture")
    desktop, bundle = args.desktop.resolve(), args.bundle.resolve()
    binary = bundle / ("relay-linux-" + architecture)
    require(desktop.is_file() and binary.is_file(), "Build the desktop executable and runtime bundle first")
    driver_path = shutil.which(args.driver)
    require(driver_path is not None, "tauri-driver is missing")
    require(shutil.which("WebKitWebDriver") is not None, "WebKitWebDriver is missing")
    require(shutil.which("Xvfb") is not None, "Xvfb is missing")
    artifacts = args.artifacts.resolve() if args.artifacts else Path(tempfile.mkdtemp(prefix="relay-desktop-evidence-"))
    artifacts.mkdir(parents=True, exist_ok=True)
    daemon = driver = xvfb = None
    browser = None
    identity = None
    with tempfile.TemporaryDirectory(prefix="relay-native-") as directory:
        root = Path(directory)
        state, runtime = root / "ui", root / "rt"
        fixture_bin = runtime / 'bin'
        fixture_bin.mkdir(parents=True, mode=0o700)
        fixture = fixture_bin / 'codex'
        fixture.write_text('#!/bin/sh\ncase "$1" in\n--version) echo fixture-1; exit 0;;\n--help) echo "[PROMPT] --dangerously-bypass-approvals-and-sandbox"; exit 0;;\nlogin) exit 0;;\nesac\necho RELAY_LOCAL_COORDINATOR_READY\nexec /bin/sh -i\n')
        fixture.chmod(0o700)
        # Harness discovery checks both providers. Shadow Claude as well so
        # background status reads never run an installed personal CLI.
        claude_fixture = fixture_bin / 'claude'
        claude_fixture.write_text('#!/bin/sh\ncase "$1" in\n--version) echo fixture-1; exit 0;;\n--help) echo "--append-system-prompt --dangerously-skip-permissions"; exit 0;;\nauth) [ "$2" = status ] && exit 1;;\nesac\nexit 2\n')
        claude_fixture.chmod(0o700)
        for config in (root / "claude-config", root / "codex-config"):
            config.mkdir(mode=0o700)
        control_socket = state / "run" / "controller.sock"
        environment = dict(os.environ,
            RELAY_DESKTOP_STATE_DIR=str(state), RELAY_DESKTOP_RUNTIME_DIR=str(runtime),
            RELAY_DESKTOP_LOCAL="1", XDG_DATA_HOME=str(root / "data"),
            XDG_CONFIG_HOME=str(root / "config"), XDG_CACHE_HOME=str(root / "cache"),
            CLAUDE_CONFIG_DIR=str(root / "claude-config"), CODEX_HOME=str(root / "codex-config"),
            GDK_BACKEND="x11", SHELL="/bin/sh")
        for key in ("ENV", "BASH_ENV", "WAYLAND_DISPLAY", "WAYLAND_SOCKET"):
            environment.pop(key, None)
        if args.packaged_resources:
            environment.pop("RELAY_DESKTOP_BUNDLE", None)
        else:
            environment["RELAY_DESKTOP_BUNDLE"] = str(bundle)
        with (root / "process.log").open("wb") as log:
            try:
                # Own the daemon PID ourselves: cleanup never searches for user daemons.
                daemon = subprocess.Popen([str(binary), "daemon", "--state-dir", str(runtime)],
                    env=environment, stdin=subprocess.DEVNULL, stdout=log, stderr=log)
                wait_for(lambda: unix_ready(runtime / "run" / "daemon.sock"), "test runtime startup")
                read_fd, write_fd = os.pipe()
                try:
                    # Linux abstract sockets avoid WSLg's read-only /tmp/.X11-unix.
                    # TCP and filesystem listeners stay disabled.
                    xvfb = subprocess.Popen(["Xvfb", "-displayfd", str(write_fd), "-screen", "0",
                        "1280x900x24", "-nolisten", "tcp", "-nolisten", "unix", "-listen", "local"], pass_fds=(write_fd,),
                        stdin=subprocess.DEVNULL, stdout=log, stderr=log)
                    os.close(write_fd)
                    write_fd = None
                    require(select.select([read_fd], [], [], 20)[0], "Xvfb did not provide a display")
                    display = os.read(read_fd, 32).decode().strip()
                    require(display.isdigit(), "Invalid Xvfb display")
                    environment["DISPLAY"] = ":" + display
                finally:
                    os.close(read_fd)
                    if write_fd is not None:
                        os.close(write_fd)
                port, native_port = available_port(), available_port()
                require(port != native_port, "Driver port allocation collided; retry the test")
                driver = subprocess.Popen([driver_path, "--port", str(port), "--native-port", str(native_port)],
                    env=environment, stdin=subprocess.DEVNULL, stdout=log, stderr=log)
                wait_for(lambda: tcp_ready(port), "tauri-driver startup")
                browser = WebDriver(port, native_port)
                browser.start(desktop)
                wait_for(lambda: browser.script("return document.querySelector('.page-heading h1')?.textContent==='Fleet overview';") and
                         browser.body_contains("This computer"), "native fleet rendering", 60)
                browser.check_frame()
                browser.check_appearance(artifacts)
                require(json.loads((state / "desktop" / "appearance.json").read_text()) == {"format": 1, "preference": "light"}, "Native preference was not durably saved")
                browser.check_sidebar_attention(state, artifacts)
                browser.check_scrollbar_colors(artifacts)
                identity = controller_identity(control_socket)
                launch = launch_handshake(binary, bundle, state, runtime, environment)
                require(launch["pid"] == identity[0], "Desktop helper did not reuse the native controller")
                second = BrowserClient(launch)
                require(second.json("/api/state")["hosts"][0]["id"] == "local", "Missing shared local host")
                harnesses = second.json("/api/hosts/local/runtime/harnesses")
                require({item['id']: item.get('path') for item in harnesses} == {'claude': str(claude_fixture), 'codex': str(fixture)}, "Native fixture resolved a provider outside its disposable directory")
                print("PASS: native WebKit fleet and independent browser authentication", flush=True)

                browser.click_css(".host-select")
                wait_for(lambda: browser.body_contains("Make room for your next idea."), "local host view")
                check_machine_resources(browser, second, artifacts)
                browser.click_button("New session")
                project = root / "native project with spaces"
                project.mkdir()
                browser.fill("//input[@id=//label[starts-with(normalize-space(.),'Project folder')]/@for]", str(root) + "/", "xpath")
                wait_for(lambda: browser.body_contains("native project with spaces"), "native folder listing")
                browser.click_css('[aria-label="Open folder native project with spaces"]')
                wait_for(lambda: browser.body_contains("This folder has no subfolders."), "native folder navigation")
                browser.click_button("Use this folder")
                browser.fill("//label[starts-with(normalize-space(.),'Workspace')]/input", "Native smoke", "xpath")
                browser.fill("//label[starts-with(normalize-space(.),'Session name')]/input", "Desktop smoke", "xpath")
                browser.click_button("Start session")
                wait_for(lambda: browser.body_contains("Watching"), "native terminal attachment")
                sessions = second.json("/api/hosts/local/runtime/sessions")
                require(len(sessions) == 1 and sessions[0]["title"] == "Desktop smoke" and sessions[0]["cwd"] == str(project), "Native form did not create session in the selected folder")
                session = sessions[0]["id"]
                session_created = sessions[0]["createdAt"]
                input_path = f"/api/hosts/local/runtime/sessions/{session}/input"
                require(browser.script("return !document.querySelector('.terminal-composer, textarea[aria-label=\"Message or command\"]');"), "Separate command composer still exists")
                browser.claim_terminal()
                denied, _ = second.request(input_path, "POST", {"data": "printf 'RELAY_DENIED_INPUT\\n'\r"})
                require(denied == 409, "Second client bypassed native exclusive control")
                browser.check_terminal_keys(session)
                browser.check_scrollbar_colors(artifacts, terminal=True)
                browser.check_terminal_geometry(artifacts)
                browser.check_terminal_clipboard()
                browser.check_terminal_image_clipboard(state, second, session, artifacts)
                browser.terminal_command("printf 'RELAY_%s\\n' 'NATIVE_INPUT_OK'")
                wait_for(lambda: "RELAY_NATIVE_INPUT_OK" in second.history(session), "native PTY command output")
                wait_for(lambda: browser.terminal_contains("RELAY_NATIVE_INPUT_OK"), "native terminal paints command output")
                require("RELAY_DENIED_INPUT" not in second.history(session), "Rejected input reached the PTY")
                browser.click_button("Release control")
                wait_for(lambda: browser.body_contains("Watching"), "native lease release")
                accepted, _ = second.request(input_path, "POST", {"data": "printf 'RELAY_%s\\n' 'BROWSER_INPUT_OK'\r"})
                require(accepted == 204, "Second client could not send after lease release")
                wait_for(lambda: "RELAY_BROWSER_INPUT_OK" in second.history(session), "second client PTY output")
                browser.claim_terminal()
                browser.screenshot(artifacts / "native-session.png")
                print("PASS: native session creation, PTY input and cross-client exclusive control", flush=True)

                check_detached_terminal(browser, desktop, driver_path, environment, log, second, session, artifacts)
                require(daemon.poll() is None, "Closing the native window stopped the runtime")
                require(process_start_time(identity[0]) == identity[1], "Closing the native window stopped the controller")
                current = second.json("/api/hosts/local/runtime/sessions")
                require(current[0]["id"] == session and current[0]["status"] == "running", "Session did not survive native closure")
                browser.start(desktop)
                wait_for(lambda: browser.script("return document.querySelector('.page-heading h1')?.textContent==='Fleet overview';") and
                         browser.body_contains("This computer"), "reopened native fleet", 60)
                require(controller_identity(control_socket) == identity, "Reopening native app replaced the shared controller")
                appearance = browser.appearance()
                require(appearance == {"preference": "light", "resolved": "light"}, "Appearance did not survive native close/reopen")
                browser.check_appearance_views(appearance)
                require(browser.sidebar_collapsed(), "Sidebar preference did not survive native close/reopen")
                browser.call("POST", "/url", {"url": second.origin + "/#host/local/session/" + session})
                wait_for(lambda: browser.body_contains("Watching") and browser.body_contains("Desktop smoke"), "reopened persistent session")
                wait_for(lambda: browser.terminal_contains("RELAY_NATIVE_INPUT_OK") and
                         browser.terminal_contains("RELAY_BROWSER_INPUT_OK"), "native terminal repaints saved screen")
                browser.claim_terminal()
                browser.terminal_command("printf 'RELAY_%s\\n' 'REOPEN_INPUT_OK'")
                wait_for(lambda: "RELAY_REOPEN_INPUT_OK" in second.history(session), "reopened native PTY command output")
                browser.screenshot(artifacts / "native-reopened.png")
                print("PASS: native close/reopen preserves controller, runtime, terminal and input", flush=True)
                check_local_mastermind(browser, second, state, artifacts)
                check_terminal_panes(browser, second, session, artifacts)
                # The pane test changes the canonical grid and writes newer
                # output. Verify earlier output in history, then use the latest
                # visible marker for the subsequent snapshot-replay assertion.
                require('RELAY_REOPEN_INPUT_OK' in second.history(session), 'Pane resizing lost earlier session output')
                # A new controller origin must not lose native preferences or
                # the live shell. Stop only the PID + creation time owned above.
                old_origin = second.origin
                browser.close()
                stop_controller(identity, state)
                identity = None
                new_port = available_port()
                require(new_port != urllib.parse.urlsplit(old_origin).port, "Test controller port allocation unexpectedly reused the old origin")
                restarted = launch_handshake(binary, bundle, state, runtime, environment, f"127.0.0.1:{new_port}")
                identity = controller_identity(control_socket)
                require(restarted['address'] != old_origin, "Controller did not change origin for native preference test")
                second = BrowserClient(restarted)
                require(daemon.poll() is None and any(item['id'] == session and item['createdAt'] == session_created and item['status'] == 'running' for item in second.json('/api/hosts/local/runtime/sessions')), "Controller restart lost the live test shell")
                browser.start(desktop)
                wait_for(lambda: browser.body_contains('Fleet overview') and browser.body_contains('This computer'), 'native fleet at new controller origin', 60)
                wait_for(browser.sidebar_collapsed, 'native sidebar preference survives controller port change')
                require(browser.script("return localStorage.getItem('relay.attention-collapsed.v1');") is None, "New-origin preference was restored from browser storage instead of the native file")
                require(browser.appearance() == {'preference': 'light', 'resolved': 'light'}, "Appearance changed during controller restart")
                browser.screenshot(artifacts / 'native-attention-new-origin.png')
                browser.call('POST', '/url', {'url': second.origin + '/#host/local/session/' + session})
                wait_for(lambda: browser.terminal_contains('RELAY_PANE_1_OWNED'), 'same terminal restores its latest screen after controller origin change')
                require('RELAY_REOPEN_INPUT_OK' in second.history(session), 'Controller origin change lost earlier session output')
                print('PASS: native sidebar preference survives close/reopen and controller origin change while the PTY remains running', flush=True)
                status, _ = second.request(f"/api/hosts/local/runtime/sessions/{session}", "DELETE")
                require(status == 204, "Could not remove test-owned shell session")
            except Exception:
                if browser and browser.session:
                    with contextlib.suppress(Exception):
                        browser.screenshot(artifacts / "failure.png")
                diagnostic = (root / "process.log").read_bytes()[-12000:].decode(errors="replace")
                diagnostic = re.sub(r"\b[0-9a-fA-F]{48}\b", "[redacted]", diagnostic)
                diagnostic = re.sub(r"token=[^\s&\"']+", "token=[redacted]", diagnostic)
                (artifacts / "failure.log").write_text(diagnostic)
                (artifacts / "failure.log").chmod(0o600)
                print("Native failure evidence: " + str(artifacts), flush=True)
                raise
            finally:
                if browser:
                    browser.close()
                stop_process(driver)
                if identity is None and control_socket.exists():
                    with contextlib.suppress(OSError):
                        identity = controller_identity(control_socket)
                try:
                    stop_controller(identity, state)
                finally:
                    stop_process(daemon)
                    stop_process(xvfb)
    print("Native screenshots: " + str(artifacts), flush=True)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--desktop", type=Path, required=True)
    parser.add_argument("--bundle", type=Path, required=True)
    parser.add_argument("--driver", default="tauri-driver")
    parser.add_argument("--artifacts", type=Path)
    parser.add_argument("--packaged-resources", action="store_true",
                        help="exercise installed Tauri resource lookup without a bundle environment override")
    run(parser.parse_args())
