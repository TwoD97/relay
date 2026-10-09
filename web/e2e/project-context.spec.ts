import { expect, test, type Page } from "@playwright/test";
import { mockFleet, work } from "./fixture";

const projectPath = "/home/dev/projects/api service/$literal [draft]";

async function openCodexForm(page: Page) {
  await page.goto("/#host/dev");
  await page.getByRole("button", { name: "New session", exact: true }).first().click();
  const dialog = page.getByRole("dialog");
  await dialog.getByRole("radio", { name: /Codex/ }).check();
  await dialog.getByLabel("Project folder").fill(projectPath);
  await dialog.getByLabel("Workspace", { exact: false }).fill("Client project");
  await dialog.getByLabel("Session name").fill("Implement review");
  return dialog;
}

test("shared project instructions are opt-in and ordinary agent creation writes no context files", async ({ page }) => {
  const fixture = await mockFleet(page);
  const dialog = await openCodexForm(page);
  await expect(dialog.getByRole("checkbox", { name: /Shared instructions and memory/ })).not.toBeChecked();
  await dialog.getByRole("button", { name: "Start session", exact: true }).click();
  await expect(page.getByRole("heading", { name: "Implement review", exact: true })).toBeVisible();
  expect(fixture.mutations).toEqual([{
    method: "POST", path: "/api/hosts/dev/runtime/sessions", csrf: "fixture-csrf",
    body: { title: "Implement review", workspace: "Client project", cwd: projectPath, harness: "codex" },
  }]);
});

test("opting in prepares literal project paths before creating the agent without terminal input", async ({ page }, testInfo) => {
  const fixture = await mockFleet(page);
  const dialog = await openCodexForm(page);
  await dialog.getByRole("checkbox", { name: /Shared instructions and memory/ }).check();
  await expect(dialog).toContainText("Existing content is kept");
  await expect(dialog).toContainText("Native chat histories stay separate");
  await page.screenshot({ path: testInfo.outputPath("shared-project-context.png"), fullPage: true, animations: "disabled" });
  await dialog.getByRole("button", { name: "Start session", exact: true }).click();
  await expect(page.getByRole("heading", { name: "Implement review", exact: true })).toBeVisible();
  await expect(page.getByRole("status")).toContainText("Shared project context is ready.");
  expect(fixture.mutations).toEqual([
    { method: "POST", path: "/api/hosts/dev/project-context", csrf: "fixture-csrf", body: { path: projectPath } },
    { method: "POST", path: "/api/hosts/dev/runtime/sessions", csrf: "fixture-csrf", body: { title: "Implement review", workspace: "Client project", cwd: projectPath, harness: "codex" } },
  ]);
  expect(fixture.terminalMessages.filter((message) => message.type === "input")).toEqual([]);
  expect(fixture.sessions.dev.find((session) => session.id === work.id)).toEqual(work);
});

test("failed context preparation keeps the form open and cannot start or replay agent creation", async ({ page }) => {
  const fixture = await mockFleet(page);
  let attempts = 0;
  await page.route("**/api/hosts/dev/project-context", async (route) => {
    attempts++;
    expect(route.request().postDataJSON()).toEqual({ path: projectPath });
    expect(route.request().headers()["x-relay-csrf"]).toBe("fixture-csrf");
    await route.fulfill({ status: 409, json: { error: "An existing project file cannot be updated safely." } });
  });
  const dialog = await openCodexForm(page);
  await dialog.getByRole("checkbox", { name: /Shared instructions and memory/ }).check();
  await dialog.getByRole("button", { name: "Start session", exact: true }).click();
  await expect(dialog.getByRole("alert").filter({ hasText: "An existing project file" })).toContainText("An existing project file cannot be updated safely.");
  await expect(dialog.getByRole("button", { name: "Start session", exact: true })).toBeEnabled();
  await expect(dialog.getByLabel("Project folder")).toHaveValue(projectPath);
  expect(attempts).toBe(1);
  expect(fixture.mutations).toEqual([]);
  expect(fixture.sessions.dev).toEqual([work]);
  expect(fixture.attachments).toEqual([]);
});

test("closing the form during context preparation prevents a late agent launch", async ({ page }) => {
  const fixture = await mockFleet(page);
  let release!: () => void;
  const reply = new Promise<void>((resolve) => { release = resolve; });
  let attempts = 0;
  await page.route("**/api/hosts/dev/project-context", async (route) => {
    attempts++;
    await reply;
    await route.fallback();
  });
  const dialog = await openCodexForm(page);
  await dialog.getByRole("checkbox", { name: /Shared instructions and memory/ }).check();
  await dialog.getByRole("button", { name: "Start session", exact: true }).click();
  await expect.poll(() => attempts).toBe(1);
  await expect(dialog.getByRole("button", { name: "Starting…", exact: true })).toBeDisabled();
  await dialog.getByRole("button", { name: "Cancel", exact: true }).click();
  await expect(dialog).toHaveCount(0);
  release();
  await expect.poll(() => fixture.mutations.filter((request) => request.path.endsWith("/project-context")).length).toBe(1);
  const polls = fixture.stateRequests();
  await expect.poll(() => fixture.stateRequests()).toBeGreaterThan(polls);
  await expect(page.getByRole("heading", { name: "Development", exact: true })).toBeVisible();
  expect(fixture.mutations.filter((request) => request.path.endsWith("/runtime/sessions"))).toEqual([]);
  expect(fixture.sessions.dev).toEqual([work]);
  expect(fixture.attachments).toEqual([]);
});

test("switching from an opted-in agent to a shell does not prepare project context", async ({ page }) => {
  const fixture = await mockFleet(page);
  const dialog = await openCodexForm(page);
  await dialog.getByRole("checkbox", { name: /Shared instructions and memory/ }).check();
  await dialog.getByRole("radio", { name: /Terminal/ }).check();
  await expect(dialog.getByRole("checkbox", { name: /Shared instructions and memory/ })).toHaveCount(0);
  await dialog.getByRole("button", { name: "Start session", exact: true }).click();
  await expect(page.getByRole("heading", { name: "Implement review", exact: true })).toBeVisible();
  expect(fixture.mutations).toHaveLength(1);
  expect(fixture.mutations[0]).toMatchObject({ path: "/api/hosts/dev/runtime/sessions", body: { harness: "shell", cwd: projectPath } });
});
