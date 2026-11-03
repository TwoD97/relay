import { createHash } from "node:crypto";
import { expect, test, type Page, type Request } from "@playwright/test";
import { mockFleet, work } from "./fixture";

// A real one-pixel PNG, kept in the disposable browser fixture only.
const png = Buffer.from("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAAC0lEQVR4nGP4DwQACfsD/fteaysAAAAASUVORK5CYII=", "base64");
const imagePath = "/tmp/relay attachments/capture.png";
const imageReply = { path: imagePath, mime: "image/png", bytes: png.length, sha256: createHash("sha256").update(png).digest("hex") };
const imageRoute = /\/api\/hosts\/[^/]+\/sessions\/[^/]+\/images(?:\?|$)/;
type Fleet = Awaited<ReturnType<typeof mockFleet>>;
type ClipboardProbe = { imageReads: number; textReads: number; resume?: () => void };

function input(fixture: Fleet) { return fixture.terminalMessages.filter((message) => message.type === "input").map((message) => message.data).join(""); }
async function openTerminal(page: Page, options: Parameters<typeof mockFleet>[1] = {}) {
  const fixture = await mockFleet(page, { terminalOutput: "\u001b[?2004hReady\r\n$ ", ...options });
  await page.goto("/#host/dev/session/session-1");
  await page.locator(".terminal-canvas").click();
  if (!options.terminalOccupied) await expect(page.getByText("Controlling", { exact: true })).toBeVisible();
  return fixture;
}
async function clipboard(page: Page, options: { mime?: string; bytes?: number[]; size?: number; delayed?: boolean; empty?: boolean; extractionError?: boolean; text?: string } = {}) {
  await page.evaluate(({ bytes, ...config }) => {
    const state = window as unknown as { imageClipboard: ClipboardProbe };
    state.imageClipboard = { imageReads: 0, textReads: 0 };
    const items = () => config.empty ? [] : [{ types: ["text/plain", config.mime ?? "image/png"], getType: async () => {
      if (config.extractionError) throw new Error("The image could not be read");
      return new Blob([config.size ? new Uint8Array(config.size) : new Uint8Array(bytes)], { type: config.mime ?? "image/png" });
    } }];
    Object.defineProperty(navigator, "clipboard", { configurable: true, value: {
      read: () => { state.imageClipboard.imageReads++; return config.delayed ? new Promise((resolve) => { state.imageClipboard.resume = () => resolve(items()); }) : Promise.resolve(items()); },
      readText: async () => { state.imageClipboard.textReads++; return config.text ?? "must not paste the image's text alternative"; },
    } });
  }, { bytes: options.bytes ?? [...png], ...options });
}
async function upload(page: Page, options: { delayed?: boolean; status?: number; reply?: unknown; abort?: boolean } = {}) {
  const requests: Request[] = [];
  let release!: () => void;
  const pending = new Promise<void>((resolve) => { release = resolve; });
  await page.route(imageRoute, async (route) => {
    requests.push(route.request());
    if (options.delayed) await pending;
    if (options.abort) { await route.abort("failed").catch(() => {}); return; }
    await route.fulfill({ status: options.status ?? 201, json: options.reply ?? imageReply }).catch(() => {});
  });
  return { requests, release };
}
async function noTextFallback(page: Page) {
  expect(await page.evaluate(() => (window as unknown as { imageClipboard: ClipboardProbe }).imageClipboard.textReads)).toBe(0);
}

for (const action of ["shortcut", "button"] as const) {
  test(`image ${action} uploads exact bytes to the selected session and pastes one quoted path without Enter`, async ({ page }) => {
    const fixture = await openTerminal(page);
    await clipboard(page);
    const recorded = await upload(page);
    if (action === "shortcut") await page.keyboard.press("Control+v");
    else await page.getByRole("button", { name: "Paste clipboard into terminal", exact: true }).click();
    await expect.poll(() => input(fixture)).toBe(`\u001b[200~ '${imagePath}' \u001b[201~`);
    expect(recorded.requests).toHaveLength(1);
    const request = recorded.requests[0], url = new URL(request.url());
    expect(request.method()).toBe("POST");
    expect(url.pathname).toBe("/api/hosts/dev/sessions/session-1/images");
    expect(url.searchParams.get("createdAt")).toBe(work.createdAt);
    expect(request.headers()["content-type"]).toBe("image/png");
    expect(request.headers()["x-relay-csrf"]).toBe("fixture-csrf");
    expect(request.postDataBuffer()).toEqual(png);
    expect(input(fixture)).not.toMatch(/[\r\n]/);
    await noTextFallback(page);
    expect(fixture.mutations).toEqual([]);
  });
}

test("text clipboard still uses bracketed paste when there is no image", async ({ page }) => {
  const fixture = await openTerminal(page);
  const text = "printf '%s' '$HOME; `literal`'\nnext\tline ✓";
  await clipboard(page, { empty: true, text });
  const recorded = await upload(page);
  await page.keyboard.press("Control+v");
  await expect.poll(() => input(fixture)).toBe(`\u001b[200~${text.replace(/\n/g, "\r")}\u001b[201~`);
  expect(recorded.requests).toEqual([]);
  expect(await page.evaluate(() => (window as unknown as { imageClipboard: ClipboardProbe }).imageClipboard.textReads)).toBe(1);
});

test("a JPEG clipboard representation retains its MIME and exact encoded bytes", async ({ page }) => {
  const fixture = await openTerminal(page);
  const encoded = await page.evaluate(() => {
    const canvas = document.createElement("canvas");
    canvas.width = canvas.height = 2;
    canvas.getContext("2d")!.fillRect(0, 0, 2, 2);
    return canvas.toDataURL("image/jpeg").split(",")[1];
  });
  const jpeg = Buffer.from(encoded, "base64");
  await clipboard(page, { mime: "image/jpeg", bytes: [...jpeg] });
  const recorded = await upload(page, { reply: { ...imageReply, path: "/tmp/relay attachments/capture.jpg", mime: "image/jpeg", bytes: jpeg.length, sha256: createHash("sha256").update(jpeg).digest("hex") } });
  await page.keyboard.press("Control+v");
  await expect.poll(() => input(fixture)).toBe("\u001b[200~ '/tmp/relay attachments/capture.jpg' \u001b[201~");
  expect(recorded.requests).toHaveLength(1);
  expect(recorded.requests[0].headers()["content-type"]).toBe("image/jpeg");
  expect(recorded.requests[0].postDataBuffer()).toEqual(jpeg);
  await noTextFallback(page);
});

for (const [path, quoted] of [
  ["/tmp/relay's shots/$capture.png", " '/tmp/relay'\\''s shots/$capture.png' "],
  ["C:\\Users\\O'Brien\\capture.png", " 'C:/Users/O''Brien/capture.png' "],
]) {
  test(`image paths quote spaces and apostrophes for ${path.startsWith("/") ? "Linux" : "Windows"} without a command`, async ({ page }) => {
    const fixture = await openTerminal(page);
    await clipboard(page);
    await upload(page, { reply: { ...imageReply, path } });
    await page.keyboard.press("Control+v");
    await expect.poll(() => input(fixture)).toBe(`\u001b[200~${quoted}\u001b[201~`);
    expect(input(fixture)).not.toMatch(/[\r\n]/);
  });
}

test("a trusted browser image paste event uploads once without a text alternative", async ({ page, context }) => {
  await context.grantPermissions(["clipboard-read", "clipboard-write"]);
  const fixture = await openTerminal(page);
  // Headless Chromium's test clipboard, never the user's native desktop clipboard.
  await page.evaluate(async (bytes) => {
    const canvas = document.createElement("canvas");
    canvas.width = canvas.height = 2;
    const image = await createImageBitmap(new Blob([new Uint8Array(bytes)], { type: "image/png" }));
    canvas.getContext("2d")!.drawImage(image, 0, 0);
    const png = await new Promise<Blob>((resolve) => canvas.toBlob((value) => resolve(value!), "image/png"));
    document.addEventListener("paste", (event) => { (window as unknown as { trustedImagePaste: boolean }).trustedImagePaste = event.isTrusted; }, { capture: true });
    await navigator.clipboard.write([new ClipboardItem({ "image/png": png, "text/plain": new Blob(["do not paste this alternative"], { type: "text/plain" }) })]);
  }, [...png]);
  const requests: Request[] = [];
  await page.route(imageRoute, async (route) => {
    const request = route.request(); requests.push(request);
    const data = request.postDataBuffer()!;
    await route.fulfill({ status: 201, json: { ...imageReply, bytes: data.length, sha256: createHash("sha256").update(data).digest("hex") } });
  });
  await page.keyboard.press("Shift+Insert");
  await expect.poll(() => input(fixture)).toBe(`\u001b[200~ '${imagePath}' \u001b[201~`);
  expect(await page.evaluate(() => (window as unknown as { trustedImagePaste: boolean }).trustedImagePaste)).toBe(true);
  expect(requests).toHaveLength(1);
  expect(requests[0].headers()["content-type"]).toBe("image/png");
});

for (const invalid of ["oversize", "unsupported type", "unreadable image"] as const) {
  test(`an ${invalid} clipboard image never uploads or falls back to text`, async ({ page }) => {
    const fixture = await openTerminal(page);
    await clipboard(page, { ...(invalid === "oversize" ? { size: 10 * 1024 * 1024 + 1 } : {}), ...(invalid === "unsupported type" ? { mime: "image/gif" } : {}), extractionError: invalid === "unreadable image" });
    const recorded = await upload(page);
    await page.keyboard.press("Control+v");
    await expect(page.locator(".terminal-image-notice")).toBeVisible();
    await expect(page.locator(".terminal-image-notice")).toContainText(invalid === "oversize" ? /10\s*(?:MiB|MB)/ : invalid === "unsupported type" ? /PNG|JPEG/ : /could not|unable|read/i);
    expect(recorded.requests).toEqual([]);
    expect(input(fixture)).toBe("");
    await noTextFallback(page);
  });
}

for (const failure of ["rejected", "unknown"] as const) {
  test(`a ${failure} image upload stays visible without retry or clipboard fallback`, async ({ page }) => {
    const fixture = await openTerminal(page);
    await clipboard(page);
    const recorded = await upload(page, failure === "rejected" ? { status: 409, reply: { error: "The session identity changed" } } : { abort: true });
    await page.keyboard.press("Control+v");
    await expect(page.locator(".terminal-image-notice")).toContainText(failure === "rejected" ? "The session identity changed" : /may have|not.*repeat|before.*answer|connection/i);
    expect(recorded.requests).toHaveLength(1);
    expect(input(fixture)).toBe("");
    await noTextFallback(page);
  });
}

for (const phase of ["clipboard", "upload"] as const) for (const transition of ["focus", "lease", "session", "typing"] as const) {
  test(`a delayed ${phase} image cannot insert after ${transition} changes`, async ({ page }) => {
    const fixture = await openTerminal(page, { sessions: { local: [], dev: [structuredClone(work), { ...work, id: "second", title: "Other terminal" }] } });
    await clipboard(page, { delayed: phase === "clipboard" });
    const recorded = await upload(page, { delayed: phase === "upload" });
    await page.keyboard.press("Control+v");
    if (phase === "clipboard") await expect.poll(() => page.evaluate(() => (window as unknown as { imageClipboard: ClipboardProbe }).imageClipboard.imageReads)).toBe(1);
    else await expect.poll(() => recorded.requests.length).toBe(1);
    if (transition === "focus") {
      await page.getByRole("button", { name: "Release control", exact: true }).focus();
      await page.locator(".xterm-helper-textarea").focus();
    } else if (transition === "lease") {
      await page.getByRole("button", { name: "Release control", exact: true }).click();
      await expect(page.getByText("Watching", { exact: true })).toBeVisible();
      await page.locator(".terminal-canvas").click();
      await expect(page.getByText("Controlling", { exact: true })).toBeVisible();
    } else if (transition === "session") {
      await page.evaluate(() => { location.hash = "#host/dev/session/second"; });
      await expect(page.getByRole("heading", { name: "Other terminal", exact: true })).toBeVisible();
      await page.locator(".terminal-canvas").click();
      await expect(page.getByText("Controlling", { exact: true })).toBeVisible();
    } else await page.keyboard.type("keep this draft");
    if (phase === "clipboard") await page.evaluate(() => (window as unknown as { imageClipboard: ClipboardProbe }).imageClipboard.resume!());
    else {
      const response = page.waitForResponse((value) => imageRoute.test(value.url()));
      recorded.release();
      await (await response).finished();
    }
    // Process the released promise and its UI work without a timing-only sleep.
    await page.evaluate(() => new Promise<void>((resolve) => requestAnimationFrame(() => requestAnimationFrame(() => resolve()))));
    expect(recorded.requests).toHaveLength(phase === "clipboard" ? 0 : 1);
    expect(input(fixture)).toBe(transition === "typing" ? "keep this draft" : "");
    await noTextFallback(page);
  });
}

test("reconnecting during an upload never inserts its late path into the new socket", async ({ page }) => {
  const fixture = await openTerminal(page);
  await clipboard(page);
  const recorded = await upload(page, { delayed: true });
  await page.keyboard.press("Control+v");
  await expect.poll(() => recorded.requests.length).toBe(1);
  fixture.disconnectTerminals();
  await expect.poll(() => fixture.attachments.length).toBe(2);
  await page.locator(".terminal-canvas").click();
  await expect(page.getByText("Controlling", { exact: true })).toBeVisible();
  const response = page.waitForResponse((value) => imageRoute.test(value.url()));
  recorded.release();
  await (await response).finished();
  await page.evaluate(() => new Promise<void>((resolve) => requestAnimationFrame(() => requestAnimationFrame(() => resolve()))));
  expect(input(fixture)).toBe("");
  expect(recorded.requests).toHaveLength(1);
});

test("an intervening literal Ctrl+V key invalidates a pending image paste", async ({ page }) => {
  const fixture = await openTerminal(page);
  await clipboard(page);
  const recorded = await upload(page, { delayed: true });
  await page.keyboard.press("Control+v");
  await expect.poll(() => recorded.requests.length).toBe(1);
  await page.keyboard.press("Control+Alt+v");
  await expect.poll(() => input(fixture)).toBe("\u0016");
  const response = page.waitForResponse((value) => imageRoute.test(value.url()));
  recorded.release(); await (await response).finished();
  await page.evaluate(() => new Promise<void>((resolve) => requestAnimationFrame(() => requestAnimationFrame(() => resolve()))));
  expect(input(fixture)).toBe("\u0016");
  expect(recorded.requests).toHaveLength(1);
});

test("the footer Enter key invalidates a pending image even when terminal focus is preserved", async ({ page }) => {
  const fixture = await openTerminal(page);
  await clipboard(page);
  const recorded = await upload(page, { delayed: true });
  await page.keyboard.press("Control+v");
  await expect.poll(() => recorded.requests.length).toBe(1);
  // Some embedded/touch clients activate a button without transferring focus.
  // Exercise the same action on desktop, where this touch toolbar is hidden.
  await page.getByRole("button", { name: "Enter", exact: true, includeHidden: true }).evaluate((button: HTMLButtonElement) => button.click());
  await expect(page.locator(".xterm-helper-textarea")).toBeFocused();
  await expect.poll(() => input(fixture)).toBe("\r");
  const response = page.waitForResponse((value) => imageRoute.test(value.url()));
  recorded.release(); await (await response).finished();
  await page.evaluate(() => new Promise<void>((resolve) => requestAnimationFrame(() => requestAnimationFrame(() => resolve()))));
  expect(input(fixture)).toBe("\r");
  expect(recorded.requests).toHaveLength(1);
});

for (const invalid of ["path", "unicode path", "mime", "bytes", "sha256"] as const) {
  test(`an invalid upload ${invalid} is never pasted`, async ({ page }) => {
    const fixture = await openTerminal(page);
    await clipboard(page);
    const reply = { ...imageReply, ...(invalid === "path" ? { path: "/tmp/image.png\nexecute" } : invalid === "unicode path" ? { path: "/tmp/image\u0085name.png" } : invalid === "mime" ? { mime: "text/html" } : invalid === "bytes" ? { bytes: -1 } : { sha256: "invalid" }) };
    const recorded = await upload(page, { reply });
    await page.keyboard.press("Control+v");
    await expect(page.locator(".terminal-image-notice")).toContainText(/invalid|verify|verified|unexpected|unsafe/i);
    expect(recorded.requests).toHaveLength(1);
    expect(input(fixture)).toBe("");
    await noTextFallback(page);
  });
}

test("the explicit image picker uploads a selected PNG without reading the clipboard", async ({ page }) => {
  const fixture = await openTerminal(page);
  await clipboard(page);
  const recorded = await upload(page);
  const chooser = page.waitForEvent("filechooser");
  await page.getByRole("button", { name: "Attach image", exact: true }).click();
  await (await chooser).setFiles({ name: "screenshot.png", mimeType: "image/png", buffer: png });
  await expect.poll(() => input(fixture)).toBe(`\u001b[200~ '${imagePath}' \u001b[201~`);
  expect(recorded.requests).toHaveLength(1);
  expect(await page.evaluate(() => (window as unknown as { imageClipboard: ClipboardProbe }).imageClipboard.imageReads)).toBe(0);
  await noTextFallback(page);
});

test("repeated paste while an image upload is pending does not duplicate the upload or path", async ({ page }) => {
  const fixture = await openTerminal(page);
  await clipboard(page);
  const recorded = await upload(page, { delayed: true });
  await page.keyboard.press("Control+v");
  await expect.poll(() => recorded.requests.length).toBe(1);
  await page.keyboard.press("Control+v");
  await page.keyboard.press("Control+Shift+V");
  expect(await page.evaluate(() => (window as unknown as { imageClipboard: ClipboardProbe }).imageClipboard.imageReads)).toBe(1);
  recorded.release();
  await expect.poll(() => input(fixture)).toBe(`\u001b[200~ '${imagePath}' \u001b[201~`);
  expect(recorded.requests).toHaveLength(1);
});

test("a file selected after losing the writer lease is not uploaded", async ({ page }) => {
  const fixture = await openTerminal(page);
  const recorded = await upload(page);
  const chooser = page.waitForEvent("filechooser");
  await page.getByRole("button", { name: "Attach image", exact: true }).click();
  const selected = await chooser;
  await page.getByRole("button", { name: "Release control", exact: true }).click();
  await expect(page.getByText("Watching", { exact: true })).toBeVisible();
  await selected.setFiles({ name: "screenshot.png", mimeType: "image/png", buffer: png });
  await page.evaluate(() => new Promise<void>((resolve) => requestAnimationFrame(() => requestAnimationFrame(() => resolve()))));
  expect(recorded.requests).toEqual([]);
  expect(input(fixture)).toBe("");
});

test("an occupied terminal cannot read images, upload files, or steal control", async ({ page }) => {
  const fixture = await openTerminal(page, { terminalOccupied: true });
  await clipboard(page);
  const recorded = await upload(page);
  await expect(page.getByRole("button", { name: "Attach image", exact: true })).toBeDisabled();
  await expect(page.getByRole("button", { name: "Paste clipboard into terminal", exact: true })).toBeDisabled();
  await page.keyboard.press("Control+v");
  expect(await page.evaluate(() => (window as unknown as { imageClipboard: ClipboardProbe }).imageClipboard.imageReads)).toBe(0);
  expect(recorded.requests).toEqual([]);
  expect(fixture.terminalMessages).toEqual([]);
});

test("Mastermind images use the exact verified local session target", async ({ page }) => {
  const session = { ...work, id: "coordinator", title: "Mastermind", harness: "codex" as const, agentMode: "manual" as const, mastermindId: "c".repeat(32) };
  const fixture = await mockFleet(page, { terminalOutput: "\u001b[?2004hReady\r\n", sessions: { local: [session], dev: [structuredClone(work)] }, mastermind: { revision: 1, enabled: true, status: "running", stage: "Connected", config: { hostId: "local", harness: "codex", agentMode: "manual", cwd: session.cwd }, session: structuredClone(session), updatedAt: session.updatedAt } });
  await page.goto("/#mastermind/terminal");
  await page.locator(".terminal-canvas").click();
  await expect(page.getByText("Controlling", { exact: true })).toBeVisible();
  await clipboard(page);
  const recorded = await upload(page);
  await page.keyboard.press("Control+v");
  await expect.poll(() => input(fixture)).toBe(`\u001b[200~ '${imagePath}' \u001b[201~`);
  expect(recorded.requests).toHaveLength(1);
  const url = new URL(recorded.requests[0].url());
  expect(url.pathname).toBe("/api/hosts/local/sessions/coordinator/images");
  expect(url.searchParams.get("createdAt")).toBe(session.createdAt);
  expect(fixture.terminalMessages.filter((message) => message.type === "input").every((message) => message.path.includes("/hosts/local/"))).toBe(true);
});
