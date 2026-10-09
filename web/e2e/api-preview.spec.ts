import { expect, test } from "@playwright/test";

test("the UI-only preview returns an actionable API error instead of its HTML application", async ({ page, request }) => {
  // Deliberately unmocked: this checks Vite's real /api fallback, not a fixture.
  const response = await request.get("/api/bootstrap");
  expect(response.status()).toBe(503);
  expect(response.headers()["content-type"]).toContain("application/json");
  expect(response.headers()["cache-control"]).toBe("no-store");
  expect(await response.json()).toEqual({ error: expect.stringContaining("UI preview server") });
  await page.goto("/");
  await expect(page.getByRole("alert")).toContainText("UI preview server");
  await expect(page.getByRole("alert")).toContainText("relay ui");
  await expect(page.getByRole("alert")).not.toContainText("Unexpected token");
  await expect(page.getByRole("alert")).not.toContainText("<!doctype");
});
