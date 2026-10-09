import { defineConfig, devices } from "@playwright/test";

export default defineConfig({
  testDir: "./e2e",
  fullyParallel: true,
  workers: 2,
  timeout: 30000,
  expect: { timeout: 8000 },
  reporter: process.env.CI ? [["list"], ["html", { outputFolder: `playwright-report/${process.env.RELAY_TEST_RUN || "fixtures"}`, open: "never" }], ["junit", { outputFile: `test-results/${process.env.RELAY_TEST_RUN || "fixtures"}.xml` }]] : "list",
  outputDir: `test-results/${process.env.RELAY_TEST_RUN || "fixtures"}`,
  use: { baseURL: "http://127.0.0.1:5198", trace: "retain-on-failure", screenshot: "only-on-failure" },
  projects: [
    { name: "desktop", use: { ...devices["Desktop Chrome"], viewport: { width: 1280, height: 900 } } },
    { name: "phone", use: { ...devices["iPhone 13"], defaultBrowserType: "chromium", viewport: { width: 390, height: 844 } } },
  ],
  webServer: { command: "npm run dev", url: "http://127.0.0.1:5198", reuseExistingServer: !process.env.CI, timeout: 30000 },
});
