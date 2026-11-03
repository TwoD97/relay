import { expect, test, type Page } from "@playwright/test";
import { local, mockFleet } from "./fixture";
import type { Harness } from "../src/types";

const diagnostic = "The local sign-in check timed out. Open agent setup to check again.";
async function localSetup(page: Page, harness: Harness) {
  const fixture = await mockFleet(page, { hosts: [structuredClone(local)], sessions: { local: [] }, mastermind: { revision: 0, enabled: false, status: "unconfigured", stage: "Choose your coordinator", local: { hostId: "local", cwd: "/tmp/local coordinator" }, updatedAt: "2026-11-03T08:00:00Z" } });
  let fail = false;
  await page.route("**/api/hosts/local/runtime/harnesses", (route) => fail ? route.fulfill({ status: 503, json: { error: "Agent status is unavailable" } }) : route.fulfill({ json: [harness, { id: "codex", name: "Codex", installed: false }] }));
  await page.goto("/#mastermind");
  await page.getByRole("button", { name: "Set up Mastermind", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "Set up Mastermind", exact: true });
  await expect(dialog.getByLabel("Machine", { exact: true })).toHaveValue("local");
  await expect(dialog.getByLabel("Project folder", { exact: true })).toHaveValue("/tmp/local coordinator");
  return { fixture, dialog, failProbe: () => { fail = true; } };
}

test("an unknown local sign-in shows the safe diagnostic and waits for a confirmed result", async ({ page }) => {
  const harness: Harness = { id: "claude", name: "Claude Code", installed: true, authDetail: diagnostic };
  const { fixture, dialog } = await localSetup(page, harness);
  await expect(dialog).toContainText("Claude Code sign-in could not be verified");
  await expect(dialog.getByRole("status")).toHaveText(diagnostic);
  const start = dialog.getByRole("button", { name: "Save and start Mastermind", exact: true });
  await expect(start).toBeDisabled();
  await expect(dialog).not.toContainText("needs sign-in before Mastermind");
  harness.authenticated = true;
  await expect(start).toBeEnabled();
  await expect(dialog.getByText(diagnostic, { exact: true })).toHaveCount(0);
  expect(fixture.mutations).toEqual([]);
});

test("unknown local sign-in without a diagnostic keeps an actionable setup link without inventing an error", async ({ page }) => {
  const { fixture, dialog } = await localSetup(page, { id: "claude", name: "Claude Code", installed: true });
  await expect(dialog).toContainText("sign-in could not be verified");
  await expect(dialog.getByRole("button", { name: "Save and start Mastermind", exact: true })).toBeDisabled();
  await expect(dialog.getByRole("status")).toHaveCount(0);
  await dialog.getByRole("button", { name: "Sign in to Claude Code", exact: true }).click();
  await expect(page).toHaveURL(/#host\/local$/);
  await expect(page.getByRole("heading", { name: "This computer", exact: true })).toBeVisible();
  expect(fixture.mutations).toEqual([]);
});

test("a confirmed signed-out local agent is distinct from a failed sign-in check", async ({ page }) => {
  const { fixture, dialog } = await localSetup(page, { id: "claude", name: "Claude Code", installed: true, authenticated: false, authDetail: "The CLI reports no active login" });
  await expect(dialog).toContainText("Claude Code needs sign-in before Mastermind can start");
  await expect(dialog).not.toContainText("could not be verified");
  await expect(dialog.getByRole("button", { name: "Save and start Mastermind", exact: true })).toBeDisabled();
  expect(fixture.mutations).toEqual([]);
});

test("a missing local agent requires installation rather than an invented sign-in status", async ({ page }) => {
  const { fixture, dialog } = await localSetup(page, { id: "claude", name: "Claude Code", installed: false });
  await expect(dialog.getByRole("radio", { name: "Claude Code", exact: true })).toBeDisabled();
  await expect(dialog).toContainText("Choose an installed coding agent");
  await expect(dialog.getByRole("button", { name: "Sign in to Claude Code", exact: true })).toHaveCount(0);
  await expect(dialog.getByRole("button", { name: "Save and start Mastermind", exact: true })).toBeDisabled();
  expect(fixture.mutations).toEqual([]);
});

test("confirmed local sign-in permits explicit setup and does not display stale unknown-status detail", async ({ page }) => {
  const { fixture, dialog } = await localSetup(page, { id: "claude", name: "Claude Code", installed: true, authenticated: true, authDetail: diagnostic });
  await expect(dialog.getByRole("button", { name: "Save and start Mastermind", exact: true })).toBeEnabled();
  await expect(dialog.getByText(diagnostic, { exact: true })).toHaveCount(0);
  await expect(dialog.getByRole("button", { name: "Sign in to Claude Code", exact: true })).toHaveCount(0);
  expect(fixture.mutations).toEqual([]);
});

test("a failed fresh agent read cannot reuse a cached signed-in result to enable Mastermind", async ({ page }) => {
  const { fixture, dialog, failProbe } = await localSetup(page, { id: "claude", name: "Claude Code", installed: true, authenticated: true });
  const start = dialog.getByRole("button", { name: "Save and start Mastermind", exact: true });
  await expect(start).toBeEnabled();
  failProbe();
  await expect(dialog).toContainText("Waiting for current agent capabilities");
  await expect(start).toBeDisabled();
  await expect(dialog).not.toContainText("needs sign-in before Mastermind");
  expect(fixture.mutations).toEqual([]);
});
