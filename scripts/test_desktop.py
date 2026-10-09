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
import http.cookiejar
import json
import os
from pathlib import Path
import platform
import re
import select
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


ELEMENT = "element-6066-11e4-a52e-4f735466cecf"


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

    def start(self, application):
        result = self.request("POST", "/session", {"capabilities": {"alwaysMatch": {
            "tauri:options": {"application": str(application)}
        }}})
        self.session = result["sessionId"]
        self.call("POST", "/timeouts", {"script": 10000, "pageLoad": 30000, "implicit": 0})

    def call(self, method, path, data=None):
        require(self.session is not None, "No active native WebDriver session")
        return self.request(method, "/session/" + self.session + path, data)

    def script(self, source, *arguments):
        return self.call("POST", "/execute/sync", {"script": source, "args": list(arguments)})

    def body_contains(self, text):
        return self.script("return document.body && document.body.innerText.includes(arguments[0]);", text)

    def check_frame(self):
        current = self.call("GET", "/window")
        checked = False
        try:
            for handle in self.call("GET", "/window/handles"):
                self.call("POST", "/window", {"handle": handle})
                if self.call("GET", "/url") == "tauri://localhost/chrome.html":
                    size = self.script("return {width:innerWidth,height:innerHeight};")
                    require(size["height"] == 40 and size["width"] >= 390,
                            "GTK custom title bar has incorrect geometry")
                    checked = True
        finally:
            self.call("POST", "/window", {"handle": current})
        require(checked, "Trusted custom title bar was not created")

    def terminal_contains(self, text):
        return self.script("const rows = document.querySelector('.xterm-rows'); return !!rows && rows.textContent.includes(arguments[0]);", text)

    def find(self, using, value):
        return self.call("POST", "/element", {"using": using, "value": value})[ELEMENT]

    def click_button(self, text):
        # All test labels are static ASCII, so no XPath interpolation of user data.
        element = self.find("xpath", f"//button[normalize-space(.)='{text}']")
        self.call("POST", f"/element/{element}/click", {})

    def click_css(self, selector):
        element = self.find("css selector", selector)
        self.call("POST", f"/element/{element}/click", {})

    def fill(self, selector, text, using="css selector"):
        element = self.find(using, selector)
        self.call("POST", f"/element/{element}/clear", {})
        self.call("POST", f"/element/{element}/value", {"text": text, "value": list(text)})

    def terminal_keys(self, keys):
        # xterm owns this textarea. Clearing/filling its DOM value would bypass
        # terminal editing, so send actual WebDriver key events only.
        element = self.find("css selector", ".session-view textarea.xterm-helper-textarea")
        self.call("POST", f"/element/{element}/value", {"text": keys, "value": list(keys)})

    def terminal_command(self, command):
        self.terminal_keys(command + "\ue007")

    def claim_terminal(self):
        self.click_css(".session-view .terminal-canvas")
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


def launch_handshake(binary, bundle, state, runtime, environment):
    result = subprocess.run([str(binary), "desktop", "--state-dir", str(state),
                             "--runtime-dir", str(runtime), "--binaries", str(bundle)],
                            env=environment, capture_output=True, timeout=30, check=False)
    require(result.returncode == 0, "Desktop helper failed; stdout/stderr withheld to protect login secrets")
    require(len(result.stdout) <= 16384, "Oversized desktop handshake")
    data = json.loads(result.stdout)
    require(data.get("protocol") == 1 and isinstance(data.get("pid"), int), "Unexpected desktop protocol")
    return data


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
        control_socket = state / "run" / "controller.sock"
        environment = dict(os.environ,
            RELAY_DESKTOP_STATE_DIR=str(state), RELAY_DESKTOP_RUNTIME_DIR=str(runtime),
            RELAY_DESKTOP_LOCAL="1", XDG_DATA_HOME=str(root / "data"),
            XDG_CONFIG_HOME=str(root / "config"), XDG_CACHE_HOME=str(root / "cache"),
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
                wait_for(lambda: browser.body_contains("A home for your fleet.") and
                         browser.body_contains("This computer"), "native fleet rendering", 60)
                browser.check_frame()
                identity = controller_identity(control_socket)
                launch = launch_handshake(binary, bundle, state, runtime, environment)
                require(launch["pid"] == identity[0], "Desktop helper did not reuse the native controller")
                second = BrowserClient(launch)
                require(second.json("/api/state")["hosts"][0]["id"] == "local", "Missing shared local host")
                print("PASS: native WebKit fleet and independent browser authentication", flush=True)

                browser.click_css(".host-select")
                wait_for(lambda: browser.body_contains("Make room for your next idea."), "local host view")
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
                input_path = f"/api/hosts/local/runtime/sessions/{session}/input"
                require(browser.script("return !document.querySelector('.terminal-composer, textarea[aria-label=\"Message or command\"]');"), "Separate command composer still exists")
                browser.claim_terminal()
                denied, _ = second.request(input_path, "POST", {"data": "printf 'RELAY_DENIED_INPUT\\n'\r"})
                require(denied == 409, "Second client bypassed native exclusive control")
                browser.check_terminal_keys(session)
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

                browser.close_window()
                require(daemon.poll() is None, "Closing the native window stopped the runtime")
                require(process_start_time(identity[0]) == identity[1], "Closing the native window stopped the controller")
                current = second.json("/api/hosts/local/runtime/sessions")
                require(current[0]["id"] == session and current[0]["status"] == "running", "Session did not survive native closure")
                browser.start(desktop)
                wait_for(lambda: browser.body_contains("A home for your fleet.") and
                         browser.body_contains("This computer"), "reopened native fleet", 60)
                require(controller_identity(control_socket) == identity, "Reopening native app replaced the shared controller")
                browser.call("POST", "/url", {"url": second.origin + "/#host/local/session/" + session})
                wait_for(lambda: browser.body_contains("Watching") and browser.body_contains("Desktop smoke"), "reopened persistent session")
                wait_for(lambda: browser.terminal_contains("RELAY_NATIVE_INPUT_OK") and
                         browser.terminal_contains("RELAY_BROWSER_INPUT_OK"), "native terminal repaints saved screen")
                browser.claim_terminal()
                browser.terminal_command("printf 'RELAY_%s\\n' 'REOPEN_INPUT_OK'")
                wait_for(lambda: "RELAY_REOPEN_INPUT_OK" in second.history(session), "reopened native PTY command output")
                browser.screenshot(artifacts / "native-reopened.png")
                print("PASS: native close/reopen preserves controller, runtime, terminal and input", flush=True)
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
