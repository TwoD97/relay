import { expect, test } from "@playwright/test";
import { local, mockFleet, openNavigation, remote, work } from "./fixture";

test("empty fleet is usable and stays within a phone viewport", async ({ page }, testInfo) => {
  await mockFleet(page, { hosts: [], sessions: {} });
  await page.goto("/");
  await expect(page.getByRole("heading", { name: "A home for your fleet." })).toBeVisible();
  await expect(page.getByRole("button", { name: "Connect your first machine" })).toBeVisible();
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBeTruthy();
  await page.screenshot({ path: testInfo.outputPath("fleet-empty.png"), fullPage: true });
});

test("host enrollment uses CSRF and opens native SSH prompts", async ({ page }) => {
  const fixture = await mockFleet(page, { hosts: [], sessions: {} });
  await page.goto("/");
  await page.getByRole("button", { name: "Connect your first machine" }).click();
  await page.getByLabel("SSH destination").fill("deploy@example.test");
  await page.getByLabel("Display name").fill("Production tools");
  await page.getByLabel("Port override (optional)", { exact: true }).fill("2222");
  await page.getByRole("dialog").getByRole("button", { name: "Connect machine", exact: true }).click();
  await expect(page.getByRole("dialog")).toContainText("Connecting to Production tools");
  await expect(page.getByRole("region", { name: "SSH setup terminal" })).toBeVisible();
  await expect.poll(() => fixture.attachments.length).toBe(1);
  expect(fixture.mutations[0]).toMatchObject({ method: "POST", path: "/api/hosts", csrf: "fixture-csrf", body: { name: "Production tools", target: "deploy@example.test", port: 2222 } });
  fixture.hosts[0].status = "online";
  fixture.hosts[0].stage = "Runtime ready";
  await expect(page.getByRole("heading", { name: "Production tools is ready" })).toBeVisible();
  await page.getByRole("button", { name: "Open machine", exact: true }).click();
  await expect(page.getByRole("heading", { name: "Production tools", exact: true })).toBeVisible();
});

test("terminal starts watching and a deliberate canvas click claims exclusive control", async ({ page }, testInfo) => {
  const fixture = await mockFleet(page, { replayQueriesBeforeClaim: true });
  await page.goto("/#host/dev/session/session-1");
  await expect(page.getByText("Watching", { exact: true })).toBeVisible();
  await expect(page.getByRole("button", { name: "Take control", exact: true })).toBeEnabled();
  const originalWidth = await page.locator(".xterm-screen").evaluate((node) => node.getBoundingClientRect().width);
  const originalViewport = page.viewportSize()!;
  await page.setViewportSize({ width: originalViewport.width - 35, height: originalViewport.height });
  await expect.poll(() => page.locator(".xterm-screen").evaluate((node) => node.getBoundingClientRect().width)).toBe(originalWidth);
  await page.setViewportSize(originalViewport);
  await expect(page.getByLabel("Message or command")).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Send message", exact: true })).toHaveCount(0);
  await page.keyboard.type("never sent before focusing the terminal");
  await page.keyboard.press("Enter");
  expect(fixture.terminalMessages.filter((message) => message.type === "input" || message.type === "resize")).toEqual([]);
  expect(fixture.terminalMessages.filter((message) => message.type === "claim")).toEqual([]);
  await page.locator(".terminal-canvas").click();
  await expect(page.getByText("Controlling", { exact: true })).toBeVisible();
  await page.keyboard.type("printf hello");
  await page.keyboard.press("Enter");
  await expect.poll(() => fixture.terminalMessages.filter((message) => message.type === "input").map((message) => message.data).join("")).toBe("printf hello\r");
  expect(fixture.terminalMessages.filter((message) => message.type === "resize")).toHaveLength(1);
  await page.getByRole("button", { name: "Release control", exact: true }).click();
  await expect(page.getByText("Watching", { exact: true })).toBeVisible();
  await page.screenshot({ path: testInfo.outputPath("session-terminal.png"), fullPage: true });
});

test("direct terminal keyboard preserves editing keys and Ctrl-C", async ({ page }) => {
  const fixture = await mockFleet(page);
  await page.goto("/#host/dev/session/session-1");
  // The explicit control button remains a keyboard-accessible alternative.
  await page.getByRole("button", { name: "Take control", exact: true }).click();
  await expect(page.getByText("Controlling", { exact: true })).toBeVisible();
  await page.keyboard.type("abc");
  for (const key of ["ArrowLeft", "Backspace", "Tab", "ArrowUp", "ArrowDown", "ArrowRight", "Control+c", "Control+v", "Enter"]) await page.keyboard.press(key);
  await expect.poll(() => fixture.terminalMessages.filter((message) => message.type === "input").map((message) => message.data).join("")).toBe("abc\u001b[D\u007f\t\u001b[A\u001b[B\u001b[C\u0003\u0016\r");
  await expect(page.locator(".xterm-helper-textarea")).toBeFocused();
});

test("terminal paste uses remote bracketed-paste mode without submitting or rewriting literals", async ({ page }) => {
  const fixture = await mockFleet(page, { terminalOutput: "\u001b[?2004hReady for bracketed paste\r\n$ " });
  await page.goto("/#host/dev/session/session-1");
  await page.locator(".terminal-canvas").click();
  await expect(page.getByText("Controlling", { exact: true })).toBeVisible();
  const pasted = "printf '%s' 'a $HOME; `literal`'\nsecond\tline ✓";
  await page.locator(".xterm-helper-textarea").evaluate((input, text) => {
    const clipboard = new DataTransfer();
    clipboard.setData("text/plain", text);
    input.dispatchEvent(new ClipboardEvent("paste", { clipboardData: clipboard, bubbles: true, cancelable: true }));
  }, pasted);
  await expect.poll(() => fixture.terminalMessages.filter((message) => message.type === "input").map((message) => message.data).join("")).toBe(`\u001b[200~${pasted.replace(/\n/g, "\r")}\u001b[201~`);
});

test("explicit copy uses the actual terminal selection without sending a remote shortcut", async ({ page }) => {
  const fixture = await mockFleet(page, { terminalOutput: "COPY_ME\r\n$ " });
  await page.goto("/#host/dev/session/session-1");
  await page.locator(".terminal-canvas").click();
  await expect(page.getByText("Controlling", { exact: true })).toBeVisible();
  await page.evaluate(() => {
    const state = window as unknown as { copiedTerminalText: string[] };
    state.copiedTerminalText = [];
    // Exercise the clipboard fallback without changing the machine clipboard.
    document.execCommand = () => false;
    Object.defineProperty(navigator, "clipboard", { configurable: true, value: { writeText: async (text: string) => { state.copiedTerminalText.push(text); } } });
  });
  await page.keyboard.press("Control+Shift+C");
  expect(await page.evaluate(() => (window as unknown as { copiedTerminalText: string[] }).copiedTerminalText)).toEqual([]);
  const screen = await page.locator(".xterm-screen").boundingBox();
  const row = await page.locator(".xterm-rows > div").first().boundingBox();
  const columns = fixture.terminalMessages.filter((message) => message.type === "resize").at(-1)!.cols!;
  if (!screen || !row) throw new Error("The real terminal renderer is unavailable");
  const cellWidth = screen.width / columns;
  await page.mouse.move(screen.x + 1, row.y + row.height / 2);
  await page.mouse.down();
  await page.mouse.move(screen.x + 7 * cellWidth + 1, row.y + row.height / 2, { steps: 7 });
  await page.mouse.up();
  await page.keyboard.press("Control+Shift+C");
  await expect.poll(() => page.evaluate(() => (window as unknown as { copiedTerminalText: string[] }).copiedTerminalText)).toEqual(["COPY_ME"]);
  expect(fixture.terminalMessages.filter((message) => message.type === "input")).toEqual([]);
});

test("focusing an occupied terminal cannot steal its lease and releasing never reclaims it", async ({ page }) => {
  const fixture = await mockFleet(page, { terminalOccupied: true });
  await page.goto("/#host/dev/session/session-1");
  await expect(page.getByRole("button", { name: "Take control", exact: true })).toBeDisabled();
  await page.locator(".terminal-canvas").click();
  await page.keyboard.type("must not reach another viewer's terminal");
  await page.keyboard.press("Control+c");
  await page.keyboard.press("Tab");
  await expect(page.locator(".xterm-helper-textarea")).not.toBeFocused();
  expect(fixture.terminalMessages).toEqual([]);
  fixture.setTerminalOccupied(false);
  await expect(page.getByRole("button", { name: "Take control", exact: true })).toBeEnabled();
  expect(fixture.terminalMessages).toEqual([]);
  await page.locator(".terminal-canvas").click();
  await expect(page.getByText("Controlling", { exact: true })).toBeVisible();
  await page.getByRole("button", { name: "Release control", exact: true }).click();
  await expect(page.getByText("Watching", { exact: true })).toBeVisible();
  // No space/Enter here: focus is on the control button after releasing, and
  // activating that button would be an intentional claim rather than a bug.
  await page.keyboard.type("stillwatching");
  fixture.sendTerminalOutput("\r\nOutput continues while watching\r\n");
  await expect(page.locator(".terminal-canvas")).toContainText("Output continues while watching");
  expect(fixture.terminalMessages.filter((message) => message.type === "claim")).toHaveLength(1);
  expect(fixture.terminalMessages.filter((message) => message.type === "input")).toEqual([]);
});

test("terminal reconnect restores observation without replaying input or implicitly reclaiming control", async ({ page }) => {
  const fixture = await mockFleet(page);
  await page.goto("/#host/dev/session/session-1");
  await page.locator(".terminal-canvas").click();
  await expect(page.getByText("Controlling", { exact: true })).toBeVisible();
  await page.keyboard.type("sent once");
  await expect.poll(() => fixture.terminalMessages.filter((message) => message.type === "input").map((message) => message.data).join("")).toBe("sent once");
  fixture.disconnectTerminals();
  await expect(page.getByText("Disconnected", { exact: true })).toBeVisible();
  await page.keyboard.type("droppedwhileoffline");
  await expect.poll(() => fixture.attachments.length).toBe(2);
  await expect(page.getByText("Watching", { exact: true })).toBeVisible();
  expect(fixture.terminalMessages.filter((message) => message.type === "claim")).toHaveLength(1);
  expect(fixture.terminalMessages.filter((message) => message.type === "input").map((message) => message.data).join("")).toBe("sent once");
  await page.locator(".terminal-canvas").click();
  await expect(page.getByText("Controlling", { exact: true })).toBeVisible();
  await page.keyboard.type("after reconnect");
  await expect.poll(() => fixture.terminalMessages.filter((message) => message.type === "input").map((message) => message.data).join("")).toBe("sent onceafter reconnect");
});

for (const transition of ["release", "reconnect", "focus change"] as const) {
  test(`a delayed clipboard response is discarded after terminal ${transition} and reacquisition`, async ({ page }) => {
    const fixture = await mockFleet(page, { terminalOutput: "\u001b[?2004hClipboard ready\r\n$ " });
    await page.goto("/#host/dev/session/session-1");
    await page.evaluate(() => {
      const state = window as unknown as { clipboardReads: ((value: string) => void)[] };
      state.clipboardReads = [];
      Object.defineProperty(navigator, "clipboard", { configurable: true, value: { readText: () => new Promise<string>((resolve) => state.clipboardReads.push(resolve)) } });
    });
    await page.locator(".terminal-canvas").click();
    await expect(page.getByText("Controlling", { exact: true })).toBeVisible();
    await page.keyboard.press("Control+Shift+V");
    await expect.poll(() => page.evaluate(() => (window as unknown as { clipboardReads: unknown[] }).clipboardReads.length)).toBe(1);
    if (transition === "focus change") {
      await page.getByRole("button", { name: "Release control", exact: true }).focus();
      await page.evaluate(() => (window as unknown as { clipboardReads: ((value: string) => void)[] }).clipboardReads[0]("stale clipboard must never be sent"));
    } else if (transition === "release") {
      await page.getByRole("button", { name: "Release control", exact: true }).click();
    } else {
      fixture.disconnectTerminals();
      await expect.poll(() => fixture.attachments.length).toBe(2);
    }
    if (transition !== "focus change") await expect(page.getByText("Watching", { exact: true })).toBeVisible();
    await page.locator(".terminal-canvas").click();
    await expect(page.getByText("Controlling", { exact: true })).toBeVisible();
    if (transition !== "focus change") await page.evaluate(() => (window as unknown as { clipboardReads: ((value: string) => void)[] }).clipboardReads[0]("stale clipboard must never be sent"));
    await page.keyboard.type("fresh input");
    await expect.poll(() => fixture.terminalMessages.filter((message) => message.type === "input").map((message) => message.data).join("")).toBe("fresh input");
    // A subsequent intentional clipboard action belongs to the current lease.
    await page.keyboard.press("Control+Shift+V");
    await expect.poll(() => page.evaluate(() => (window as unknown as { clipboardReads: unknown[] }).clipboardReads.length)).toBe(2);
    await page.evaluate(() => (window as unknown as { clipboardReads: ((value: string) => void)[] }).clipboardReads[1]("new clipboard"));
    await expect.poll(() => fixture.terminalMessages.filter((message) => message.type === "input").map((message) => message.data).join("")).toBe("fresh input\u001b[200~new clipboard\u001b[201~");
  });
}

for (const keepFocus of [true, false]) {
  test(`an early terminal click ${keepFocus ? "waits for the control handshake" : "does not claim after focus moves away"}`, async ({ page }) => {
    const fixture = await mockFleet(page, { deferTerminalControl: true });
    await page.goto("/#host/dev/session/session-1");
    await expect(page.locator(".terminal-canvas")).toContainText("Connected to Relay");
    await page.locator(".terminal-canvas").click();
    await page.keyboard.type("not queued before permission");
    expect(fixture.terminalMessages).toEqual([]);
    if (!keepFocus) await page.getByRole("button", { name: "End and remove session", exact: true }).focus();
    fixture.setTerminalOccupied(false);
    if (keepFocus) {
      await expect(page.getByText("Controlling", { exact: true })).toBeVisible();
      await page.keyboard.type("after handshake");
      await expect.poll(() => fixture.terminalMessages.filter((message) => message.type === "input").map((message) => message.data).join("")).toBe("after handshake");
      expect(fixture.terminalMessages.filter((message) => message.type === "claim")).toHaveLength(1);
    } else {
      await expect(page.getByRole("button", { name: "Take control", exact: true })).toBeEnabled();
      expect(fixture.terminalMessages).toEqual([]);
    }
  });
}

test("a delayed control grant never steals focus from another input", async ({ page }) => {
  const fixture = await mockFleet(page, { deferTerminalClaim: true });
  await page.goto("/#host/dev/session/session-1");
  await page.getByRole("button", { name: "Take control", exact: true }).click();
  await expect.poll(() => fixture.terminalMessages.filter((message) => message.type === "claim").length).toBe(1);
  await openNavigation(page);
  const search = page.getByRole("textbox", { name: "Filter machines and sessions" });
  await search.fill("focused form");
  fixture.grantTerminalControl();
  await expect(page.getByText("Controlling", { exact: true })).toBeVisible();
  await expect(search).toBeFocused();
  await page.keyboard.type(" remains here");
  await expect(search).toHaveValue("focused form remains here");
  expect(fixture.terminalMessages.filter((message) => message.type === "input")).toEqual([]);
});

test("SSH aliases inherit their configured port unless explicitly overridden", async ({ page }) => {
  const fixture = await mockFleet(page, { hosts: [], sessions: {} });
  await page.goto("/");
  await page.getByRole("button", { name: "Connect your first machine" }).click();
  await page.getByLabel("SSH destination").fill("my-development-alias");
  await expect(page.getByLabel("Port override (optional)")).toHaveValue("");
  await page.getByRole("dialog").getByRole("button", { name: "Connect machine", exact: true }).click();
  await expect(page.getByRole("dialog")).toContainText("SSH config defaults");
  expect(fixture.mutations[0]).toMatchObject({ body: { target: "my-development-alias", port: 0 } });
});

test("new session uses machine project and explicit selected harness", async ({ page }) => {
  const fixture = await mockFleet(page);
  await page.goto("/#host/local");
  await page.getByRole("button", { name: "New session", exact: true }).first().click();
  await page.getByLabel("Project folder").fill("/tmp/relay project");
  await page.getByRole("dialog").getByLabel("Workspace", { exact: false }).fill("API service");
  await page.getByLabel("Session name").fill("Database migration");
  await page.getByRole("radio", { name: /Codex/ }).check();
  await page.getByRole("button", { name: "Start session", exact: true }).click();
  await expect(page.getByRole("heading", { name: "Database migration", exact: true })).toBeVisible();
  expect(fixture.mutations.find((request) => request.path.endsWith("/runtime/sessions"))).toMatchObject({ csrf: "fixture-csrf", body: { title: "Database migration", workspace: "API service", cwd: "/tmp/relay project", harness: "codex" } });
});

test("forget and session termination require concrete confirmation; local is protected", async ({ page }) => {
  const fixture = await mockFleet(page);
  await page.goto("/#host/local");
  await expect(page.getByRole("heading", { name: "This computer", exact: true })).toBeVisible();
  await expect(page.getByRole("button", { name: "Disconnect", exact: true })).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Forget machine", exact: true })).toHaveCount(0);
  await page.goto("/#host/dev/session/session-1");
  await page.getByRole("button", { name: "End and remove session", exact: true }).click();
  await expect(page.getByRole("dialog")).toContainText("terminates the session’s process");
  await page.getByRole("button", { name: "Cancel", exact: true }).click();
  expect(fixture.mutations).toHaveLength(0);
  await page.getByRole("button", { name: "End and remove session", exact: true }).click();
  await page.getByRole("button", { name: "End and remove", exact: true }).click();
  await expect(page.getByRole("heading", { name: "Development", exact: true })).toBeVisible();
  expect(fixture.mutations.some((request) => request.method === "DELETE" && request.path.endsWith("/session-1"))).toBeTruthy();
  await page.getByRole("button", { name: "Forget machine", exact: true }).click();
  await expect(page.getByRole("dialog")).toContainText("sessions will keep running");
  await page.getByRole("dialog").getByRole("button", { name: "Forget machine", exact: true }).click();
  await expect(page.getByRole("heading", { name: "A home for your fleet." })).toBeVisible();
});

test("a stuck harness probe and failed host do not stall healthy sessions or fleet polls", async ({ page }) => {
  const fixture = await mockFleet(page, { hosts: [structuredClone(local), structuredClone(remote), { ...remote, id: "slow", name: "Slow host" }], sessions: { local: [{ ...work, id: "healthy", title: "Healthy session" }], dev: [], slow: [] }, hangHarnesses: ["local"], failHosts: ["slow"] });
  await page.goto("/#host/local");
  await expect(page.getByRole("button", { name: /Healthy session/ }).last()).toBeVisible();
  fixture.sessions.local.push({ ...work, id: "newly-discovered", title: "Newly discovered session" });
  await expect(page.getByRole("button", { name: /Newly discovered session/ }).last()).toBeVisible();
  expect(fixture.stateRequests()).toBeGreaterThan(1);
});

test("attention uses runtime events and remains pinned when the tree is filtered", async ({ page }) => {
  await mockFleet(page, { sessions: { dev: [{ ...work, attention: { kind: "permission", source: "claude-hook", updatedAt: "2026-10-07T09:00:00Z" } }] } });
  await page.goto("/");
  await openNavigation(page);
  await page.getByRole("textbox", { name: "Filter machines and sessions" }).fill("not-a-project");
  const pinned = page.getByRole("region", { name: "Needs attention" });
  await expect(pinned).toContainText("Build the dashboard");
  await pinned.getByRole("button").click();
  await expect(page.getByText("The agent reported a permission request.", { exact: false })).toBeVisible();
  await expect(page.getByRole("button", { name: "Approve", exact: true })).toHaveCount(0);
  await page.getByRole("button", { name: "Mark reviewed" }).click();
  await expect(page.getByText("The agent reported a permission request.", { exact: false })).toHaveCount(0);
});

test("harness install opens the returned real installer session", async ({ page }) => {
  const fixture = await mockFleet(page);
  await page.goto("/#host/local");
  await page.getByRole("button", { name: "Install agent", exact: true }).click();
  await expect(page.getByRole("heading", { name: "Install Claude Code", exact: true })).toBeVisible();
  expect(fixture.mutations).toContainEqual(expect.objectContaining({ method: "POST", path: "/api/hosts/local/runtime/harnesses/claude/install", csrf: "fixture-csrf" }));
});

test("authentication failure explains the one-time local login without a fake password form", async ({ page }) => {
  await mockFleet(page, { unauthorized: true });
  await page.goto("/");
  await expect(page.getByRole("heading", { name: "Connect this browser to Relay." })).toBeVisible();
  await expect(page.getByText("relay ui", { exact: true })).toBeVisible();
  await expect(page.getByLabel("Password")).toHaveCount(0);
});

test("a lost mutation reply reports uncertainty without replaying the action", async ({ page }) => {
  await mockFleet(page);
  let attempts = 0;
  await page.route("**/api/hosts/local/runtime/sessions", async (route) => {
    if (route.request().method() !== "POST") return route.fallback();
    attempts++;
    await route.abort("failed");
  });
  await page.goto("/#host/local");
  await page.getByRole("button", { name: "New session", exact: true }).first().click();
  await page.getByLabel("Project folder").fill("/tmp");
  await page.getByRole("button", { name: "Start session", exact: true }).click();
  await expect(page.getByRole("alert")).toContainText("The action may have completed");
  expect(attempts).toBe(1);
});

test("an existing tab refreshes CSRF after controller reauthentication", async ({ page }) => {
  const fixture = await mockFleet(page);
  await page.goto("/#host/local");
  await expect(page.getByRole("heading", { name: "This computer", exact: true })).toBeVisible();
  fixture.setAuthentication(false, "csrf-after-restart");
  await expect(page.getByRole("heading", { name: "Connect this browser to Relay." })).toBeVisible();
  fixture.setAuthentication(true);
  await page.getByRole("button", { name: "Check connection", exact: true }).click();
  await expect(page.getByRole("heading", { name: "This computer", exact: true })).toBeVisible();
  await expect.poll(() => fixture.reconnectRequests.length).toBe(2);
  expect(fixture.reconnectRequests.map((request) => request.csrf)).toEqual(["fixture-csrf", "csrf-after-restart"]);
  await page.getByRole("button", { name: "New session", exact: true }).first().click();
  await page.getByLabel("Project folder").fill("/tmp");
  await page.getByRole("button", { name: "Start session", exact: true }).click();
  await expect(page.getByRole("heading", { name: "Terminal", exact: true })).toBeVisible();
  expect(fixture.mutations.at(-1)?.csrf).toBe("csrf-after-restart");
});


test("completed installer shows success, saved output, and a working sign-in action", async ({ page }) => {
  const session = { ...work, title: "Install Codex", purpose: "install" as const, harness: "codex" as const, status: "exited" as const, exitCode: 0 };
  const fixture = await mockFleet(page, { sessions: { local: [session], dev: [] } });
  await page.goto("/#host/local/session/session-1");
  await expect(page.getByRole("status")).toContainText("Codex installed successfully");
  await expect(page.getByText("Saved output", { exact: true })).toBeVisible();
  await expect(page.getByRole("button", { name: "Reconnect terminal" })).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Take control", exact: true })).toHaveCount(0);
  await expect(page.getByLabel("Message or command")).toHaveCount(0);
  expect(fixture.terminalMessages).toEqual([]);
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  await expect(page.getByRole("heading", { name: "Sign in to Codex", exact: true })).toBeVisible();
  expect(fixture.mutations).toContainEqual(expect.objectContaining({ method: "POST", path: "/api/hosts/local/runtime/harnesses/codex/login", csrf: "fixture-csrf" }));
});

test("a failed installer never reports successful installation or offers terminal reconnect", async ({ page }) => {
  await mockFleet(page, { sessions: { local: [{ ...work, title: "Install Codex", purpose: "install", harness: "codex", status: "exited", exitCode: 1 }], dev: [] } });
  await page.goto("/#host/local/session/session-1");
  await expect(page.getByText("This process has exited with code 1.", { exact: false })).toBeVisible();
  await expect(page.getByText("Saved output", { exact: true })).toBeVisible();
  await expect(page.getByRole("status")).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Sign in", exact: true })).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Reconnect terminal" })).toHaveCount(0);
});
