import { expect, test, type Page } from "@playwright/test";
import { local, mockFleet, remote } from "./fixture";

const digest = "a".repeat(64);
const skill = { name: "review-plan", description: "Review a project plan before publishing.", digest, bytes: 128, files: 2 };
const markdown = "---\nname: review-plan\ndescription: Review a project plan before publishing.\n---\nRead the plan.\n";
async function fixture(page: Page, populated = true) {
  const fleet = await mockFleet(page, { hosts: [structuredClone(local), structuredClone(remote)] });
  const catalog = { revision: populated ? 1 : 0, entries: populated ? [{ ...skill }] : [] as typeof skill[] };
  const writes: { path: string; method: string; body: string; csrf: string | undefined }[] = [];
  const states: Record<string, { provider: string; status: string; digest?: string; detail?: string }[]> = { local: [{ provider: "claude", status: "absent" }, { provider: "codex", status: "absent" }], dev: [{ provider: "claude", status: "absent" }, { provider: "codex", status: "absent" }] };
  await page.route("**/api/**", async (route) => {
    const request = route.request(), url = new URL(request.url()), path = url.pathname, method = request.method();
    if (path !== "/api/skills" && !path.startsWith("/api/skills/") && !/^\/api\/hosts\/[^/]+\/skills(?:\/|$)/.test(path)) return route.fallback();
    if (method !== "GET") writes.push({ path, method, body: request.postData() ?? "", csrf: request.headers()["x-relay-csrf"] });
    if (path === "/api/skills") return route.fulfill({ json: catalog });
    if (path === "/api/skills/inspect") return route.fulfill({ json: { ...skill, paths: ["SKILL.md", "references/checklist.md"] } });
    if (path === "/api/skills/import") { expect(url.searchParams.get("revision")).toBe(String(catalog.revision)); catalog.revision++; catalog.entries = [{ ...skill }]; return route.fulfill({ json: catalog }); }
    if (path.endsWith("/export")) return route.fulfill({ status: 200, contentType: "application/zip", headers: { "Content-Disposition": 'attachment; filename="review-plan.zip"' }, body: Buffer.from("isolated fixture archive") });
    if (method === "DELETE" && path === "/api/skills/review-plan") { catalog.revision++; catalog.entries = []; return route.fulfill({ json: catalog }); }
    const hostId = path.split("/")[3];
    if (path.endsWith("/deploy")) states[hostId] = ["claude", "codex"].map((provider) => ({ provider, status: "installed", digest }));
    if (method === "DELETE") states[hostId] = ["claude", "codex"].map((provider) => ({ provider, status: "absent" }));
    return route.fulfill({ json: { protocol: 1, deployments: [{ name: skill.name, providers: states[hostId] }] } });
  });
  await page.goto("/#skills");
  await expect(page.getByRole("heading", { name: "Skills", exact: true })).toBeVisible();
  return { fleet, catalog, writes, states };
}

test("skills imports are reviewed before library publication and never deploy automatically", async ({ page }) => {
  const state = await fixture(page, false);
  await page.getByLabel("Skill package", { exact: true }).setInputFiles({ name: "SKILL.md", mimeType: "text/markdown", buffer: Buffer.from(markdown) });
  const dialog = page.getByRole("dialog", { name: "Import shared skill", exact: true });
  await expect(dialog).toContainText("references/checklist.md");
  expect(state.catalog.entries).toHaveLength(0);
  expect(state.writes.map((item) => item.path)).toEqual(["/api/skills/inspect"]);
  await dialog.getByRole("button", { name: "Import into library", exact: true }).click();
  await expect(dialog).toHaveCount(0);
  await expect(page.getByRole("heading", { name: skill.name, exact: true })).toBeVisible();
  expect(state.writes.map((item) => item.path)).toEqual(["/api/skills/inspect", "/api/skills/import"]);
  expect(state.writes.every((item) => item.csrf === "fixture-csrf")).toBe(true);
  expect(state.fleet.mutations).toEqual([]);
});

test("selected machine deployment and removal use exact digest and preserve the library", async ({ page }) => {
  const state = await fixture(page);
  await page.getByLabel("Skill machine", { exact: true }).selectOption("dev");
  await expect(page.getByText("Not installed", { exact: true })).toHaveCount(2);
  await page.getByRole("button", { name: "Deploy to both", exact: true }).click();
  await expect(page.getByText("Current package installed", { exact: true })).toHaveCount(2);
  expect(state.writes[0]).toMatchObject({ path: "/api/hosts/dev/skills/review-plan/deploy", body: JSON.stringify({ digest }) });
  expect(state.states.local.every((item) => item.status === "absent")).toBe(true);
  await page.getByRole("button", { name: "Remove from machine", exact: true }).click();
  await page.getByRole("dialog").getByRole("button", { name: "Remove skill", exact: true }).click();
  await expect(page.getByText("Not installed", { exact: true })).toHaveCount(2);
  expect(state.catalog.entries).toHaveLength(1);
  expect(state.fleet.mutations).toEqual([]);
});

test("conflicting external skills disable deployment and show preserved files", async ({ page }) => {
  const state = await fixture(page);
  state.states.dev[0] = { provider: "claude", status: "conflict", detail: "An existing skill is not owned by Relay and was preserved." };
  await page.getByLabel("Skill machine", { exact: true }).selectOption("dev");
  await expect(page.getByText("Existing files preserved", { exact: true })).toBeVisible();
  await expect(page.getByRole("button", { name: "Deploy to both", exact: true })).toBeDisabled();
  await expect(page.getByRole("region", { name: "Skill deployment" })).toContainText("not owned by Relay");
  expect(state.writes).toEqual([]);
});

test("an unknown import is never replayed and only an explicit library reload unlocks writes", async ({ page }) => {
  const state = await fixture(page, false);
  let attempts = 0;
  await page.route("**/api/skills/import?*", (route) => { attempts++; state.catalog.revision = 1; state.catalog.entries = [{ ...skill }]; return route.abort("failed"); });
  await page.getByLabel("Skill package", { exact: true }).setInputFiles({ name: "SKILL.md", mimeType: "text/markdown", buffer: Buffer.from(markdown) });
  await page.getByRole("button", { name: "Import into library", exact: true }).click();
  await expect(page.getByRole("status")).toContainText("unconfirmed and was not repeated");
  await expect(page.getByRole("button", { name: "Import skill", exact: true })).toBeDisabled();
  await page.getByRole("button", { name: "Reload skill library", exact: true }).click();
  await expect(page.getByRole("heading", { name: skill.name, exact: true })).toBeVisible();
  await expect(page.getByRole("button", { name: "Import skill", exact: true })).toBeEnabled();
  expect(attempts).toBe(1);
});

test("a delayed host read cannot replace the newly selected machine's deployment", async ({ page }) => {
  const state = await fixture(page);
  let release!: () => void;
  const wait = new Promise<void>((resolve) => { release = resolve; });
  let requested = false;
  await page.route("**/api/hosts/dev/skills?*", async (route) => { requested = true; await wait; await route.fulfill({ json: { protocol: 1, deployments: [{ name: skill.name, providers: [{ provider: "claude", status: "installed", digest }, { provider: "codex", status: "installed", digest }] }] } }).catch(() => {}); });
  await page.getByLabel("Skill machine", { exact: true }).selectOption("dev");
  await expect.poll(() => requested).toBe(true);
  await page.getByLabel("Skill machine", { exact: true }).selectOption("local");
  await expect(page.getByText("Not installed", { exact: true })).toHaveCount(2);
  release();
  await expect(page.getByText("Current package installed", { exact: true })).toHaveCount(0);
  expect(state.writes).toEqual([]);
});

test("export and library removal do not remove deployed skills", async ({ page }) => {
  const state = await fixture(page);
  const download = page.waitForEvent("download");
  await page.getByRole("link", { name: "Export review-plan", exact: true }).click();
  expect((await download).suggestedFilename()).toBe("review-plan.zip");
  await page.getByRole("button", { name: "Remove review-plan from library", exact: true }).click();
  await expect(page.getByRole("dialog")).toContainText("already deployed on machines");
  await page.getByRole("dialog").getByRole("button", { name: "Remove skill", exact: true }).click();
  await expect(page.getByRole("heading", { name: "Your shared skill library", exact: true })).toBeVisible();
  expect(state.writes.map((item) => item.path)).toEqual(["/api/skills/review-plan"]);
});
