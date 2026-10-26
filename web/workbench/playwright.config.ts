import { defineConfig } from "@playwright/test";

const workbenchE2ePort = process.env.CH_WORKBENCH_E2E_PORT ?? "5184";
const workbenchE2eUrl = `http://127.0.0.1:${workbenchE2ePort}`;
const realRun = process.env.CH_REAL_E2E === "1";

process.env.CH_WORKBENCH_E2E_URL = workbenchE2eUrl;

export default defineConfig({
  testDir: "./e2e",
  testMatch: "**/*.e2e.ts",
  fullyParallel: false,
  retries: 0,
  reporter: "list",
  use: {
    baseURL: workbenchE2eUrl,
    browserName: "chromium",
    channel: "chrome",
    acceptDownloads: true,
    permissions: ["clipboard-read", "clipboard-write"],
    trace: realRun ? "off" : "retain-on-failure",
  },
  webServer: {
    command: `npm run dev -- --port ${workbenchE2ePort}`,
    env: {
      CH_LOCAL_AIO_URL: "",
      CH_LOCAL_AIO_API_KEY: "",
      VITE_MOCK_API: "1",
      VITE_WORKBENCH_E2E: "1",
    },
    url: workbenchE2eUrl,
    reuseExistingServer: false,
    timeout: 60_000,
  },
});
