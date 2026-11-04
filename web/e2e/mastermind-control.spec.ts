import { expect, test } from "@playwright/test";
import { mockFleet } from "./fixture";

test("durable instructions are queued once, never typed into a terminal, and can be cancelled", async ({ page }) => {
  const fleet = await mockFleet(page);
  const state = { revision: 0, recovery: true, messages: [] as { id: string; text: string; source: string; status: string }[], lessons: [], listenerAt: new Date().toISOString(), telegram: { configured: false, paired: false, bot: "", status: "" } };
  const writes: unknown[] = [];
  await page.route("**/api/mastermind/control**", async route => {
    const request = route.request(); const path = new URL(request.url()).pathname;
    if (request.method() === "GET") return route.fulfill({ json: state });
    writes.push({ path, body: request.postDataJSON() });
    if (path.endsWith("/messages")) { const body = request.postDataJSON(); state.messages.push({ ...body, source: "desktop", status: "queued" }); return route.fulfill({ json: state.messages[0], status: 201 }); }
    if (path.endsWith("/cancel")) state.messages[0].status = "cancelled";
    if (path.endsWith("/config")) state.recovery = request.postDataJSON().recovery;
    return route.fulfill({ json: state });
  });
  await page.goto("/#mastermind"); await page.locator(".mastermind-control summary").click();
  await expect(page.locator(".mastermind-control summary")).toContainText("Listening");
  await page.getByLabel("Instruction for Mastermind").fill("Inspect the blocked test job");
  await page.getByRole("button", { name: "Send to Mastermind", exact: true }).click();
  await expect(page.getByLabel("Mastermind inbox")).toContainText("queued");
  await expect(page.getByLabel("Instruction for Mastermind")).toHaveValue("");
  await page.getByLabel("Mastermind inbox").getByRole("button", { name: "Cancel", exact: true }).click();
  await expect(page.getByLabel("Mastermind inbox")).toContainText("cancelled");
  await page.getByRole("checkbox", { name: /Diagnose blocked sessions/ }).click();
  await expect(page.getByRole("checkbox", { name: /Diagnose blocked sessions/ })).not.toBeChecked();
  expect(writes).toHaveLength(3);
  expect(fleet.mutations).toEqual([]);
  expect(fleet.terminalMessages.filter(message => message.type === "input")).toEqual([]);
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
});
test("Telegram token is cleared after pairing setup and lessons stay reviewable", async ({ page }) => {
  await mockFleet(page);
  const state = { revision: 0, recovery: true, messages: [], lessons: [{ id: "1".repeat(32), problem: "Known stale lock", action: "Read owner PID before touching it", outcome: "success", evidence: "Verified dead owner" }], listenerAt: "", telegram: { configured: false, paired: false, bot: "", status: "" } };
  await page.route("**/api/mastermind/control**", route => {
    const request = route.request(); const path = new URL(request.url()).pathname;
    if (request.method() === "GET") return route.fulfill({ json: state });
    if (path.endsWith("/telegram") && request.method() === "POST") { state.telegram = { configured: true, paired: false, bot: "fixture_bot", status: "Waiting for private-chat pairing" }; return route.fulfill({ json: { bot: "fixture_bot", pairCommand: "/start fixture-pair-code", expiresAt: new Date(Date.now() + 600000).toISOString() } }); }
    if (request.method() === "DELETE" && path.includes("/lessons/")) state.lessons = [];
    return route.fulfill({ json: state });
  });
  await page.goto("/#mastermind"); await page.locator(".mastermind-control summary").click();
  await page.getByRole("navigation", { name: "Mastermind control", exact: true }).getByRole("button", { name: "Telegram", exact: true }).click();
  await page.getByLabel("BotFather token").fill("123456:fixture-token-is-not-a-real-secret");
  await page.getByRole("button", { name: "Connect Telegram", exact: true }).click();
  await expect(page.getByLabel("Telegram pairing command")).toHaveValue("/start fixture-pair-code");
  await expect(page.getByLabel("BotFather token")).toHaveCount(0);
  await expect(page.locator(".control-body")).not.toContainText("fixture-token-is-not-a-real-secret");
  await page.getByRole("button", { name: "Lessons 1", exact: true }).click();
  await expect(page.locator(".control-lessons")).toContainText("Verified dead owner");
  await page.getByRole("button", { name: "Remove lesson", exact: true }).click();
  await expect(page.locator(".control-lessons")).toContainText("after a recovery has been checked");
});
test("unknown instruction response retains its identity and does not replay automatically", async ({ page }) => {
  await mockFleet(page);
  const submissions: { id: string; text: string }[] = [];
  await page.route("**/api/mastermind/control/messages", route => { submissions.push(route.request().postDataJSON()); return route.abort("failed"); });
  await page.goto("/#mastermind"); await page.locator(".mastermind-control summary").click();
  await page.getByLabel("Instruction for Mastermind").fill("Read the latest deployment state");
  await page.getByRole("button", { name: "Send to Mastermind", exact: true }).click();
  await expect(page.locator(".control-body").getByRole("alert")).toContainText("may have completed");
  await expect(page.getByLabel("Instruction for Mastermind")).toBeDisabled();
  expect(submissions).toHaveLength(1);
  await page.getByRole("button", { name: "Check / retry same instruction", exact: true }).click();
  await expect.poll(() => submissions.length).toBe(2);
  expect(submissions[1]).toEqual(submissions[0]);
});
