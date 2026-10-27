import { defineConfig } from "@playwright/test";

const realRun = process.env.CH_REAL_E2E === "1";
const portalE2eUrl = realRun
  ? process.env.CH_PORTAL_URL ?? "http://127.0.0.1:5183"
  : "http://127.0.0.1:5183";
const workbenchE2eUrl = "http://127.0.0.1:5184";

process.env.CH_PORTAL_E2E_URL = portalE2eUrl;
process.env.CH_WORKBENCH_E2E_URL = workbenchE2eUrl;

export default defineConfig({
  testDir: "./e2e",
  testMatch: "**/*.e2e.ts",
  fullyParallel: false,
  retries: 0,
  reporter: "list",
  use: {
    baseURL: portalE2eUrl,
    browserName: "chromium",
    channel: "chrome",
    trace: realRun ? "off" : "retain-on-failure",
  },
  webServer: realRun
    ? undefined
    : [
        {
          command: "npm run dev -- --port 5183",
          url: portalE2eUrl,
          env: { VITE_MOCK_API: "1", VITE_WORKBENCH_URL: "https://workbench.mock.invalid" },
          reuseExistingServer: false,
          timeout: 60_000,
        },
        {
          command: "cd ../workbench && npm run dev -- --port 5184",
          env: { VITE_MOCK_API: "1" },
          url: workbenchE2eUrl,
          reuseExistingServer: false,
          timeout: 60_000,
        },
      ],
});
