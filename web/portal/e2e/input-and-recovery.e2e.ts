import { expect, test, type BrowserContext, type Locator, type Page } from "@playwright/test";

const portalUrl = process.env.CH_PORTAL_E2E_URL ?? "http://127.0.0.1:5183";
const user = {
  id: "u1234567",
  name: "clipboard-user",
  role: "member",
  status: "approved",
  must_change_password: false,
  quota: { hosts: 3, cpu_milli: 6000, mem_mib: 12288 },
  usage: { hosts: 0, cpu_milli: 0, mem_mib: 0 },
};

function json(body: unknown, status = 200) {
  return { status, contentType: "application/json", body: JSON.stringify(body) };
}

async function disableDevelopmentMockWorker(context: BrowserContext) {
  await context.route("**/src/main.tsx*", async (route) => {
    const response = await route.fetch();
    const source = await response.text();
    const withoutMockWorker = source.replace(
      'if (import.meta.env.DEV && import.meta.env.VITE_MOCK_API === "1")',
      "if (false)",
    );
    expect(withoutMockWorker).not.toBe(source);
    await route.fulfill({ response, body: withoutMockWorker });
  });
}

async function installPasteEvidence(page: Page) {
  await page.addInitScript(() => {
    window.addEventListener("paste", (event) => {
      const pasteEvent = event as ClipboardEvent;
      document.documentElement.setAttribute("data-last-paste", pasteEvent.clipboardData?.getData("text/plain") ?? "");
      document.documentElement.setAttribute("data-last-paste-trusted", String(pasteEvent.isTrusted));
    }, true);
  });
}

async function pasteFromClipboard(page: Page, field: Locator, value: string) {
  await page.evaluate(async (clipboardValue) => navigator.clipboard.writeText(clipboardValue), value);
  await field.focus();
  await page.keyboard.press(process.platform === "darwin" ? "Meta+V" : "Control+V");
  await expect(field).toHaveValue(value);
  await expect(page.locator("html")).toHaveAttribute("data-last-paste", value);
  await expect(page.locator("html")).toHaveAttribute("data-last-paste-trusted", "true");
}

test.describe("local PC input and API recovery", () => {
  test.skip(process.env.CH_REAL_E2E === "1", "local mock scenario is disabled for a real-host run");

  test("supports clipboard paste across text, password, numeric, API key, and multiline fields", async ({ browser }) => {
    const context = await browser.newContext({
      permissions: ["clipboard-read", "clipboard-write"],
      serviceWorkers: "block",
    });
    const page = await context.newPage();
    try {
      await installPasteEvidence(page);
      await disableDevelopmentMockWorker(context);
      let authenticated = false;
      await context.route("**/api/me", (route) => route.fulfill(authenticated
        ? json(user)
        : json({ error: { code: "unauthenticated", message: "请先登录", details: {} } }, 401)));
      await context.route("**/api/auth/login", (route) => { authenticated = true; return route.fulfill(json({ user })); });
      await context.route((url) => url.pathname === "/api/goals", (route) => route.fulfill(json({ items: [], next_cursor: null })));
      await context.route((url) => url.pathname === "/api/schedules", (route) => route.fulfill(json({ items: [], next_cursor: null })));
      await context.route((url) => url.pathname === "/api/hosts", (route) => route.fulfill(json({ items: [] })));
      await context.route((url) => url.pathname === "/api/available-models", (route) => route.fulfill(json({ items: [] })));
      await context.route((url) => url.pathname === "/api/model-connections", (route) => route.fulfill(json({ items: [], configuration_status: "not_configured" })));
      await context.route((url) => url.pathname === "/api/apikeys", (route) => route.fulfill(json({ items: [] })));

      await page.goto(`${portalUrl}/login?next=${encodeURIComponent("/goals?view=goals")}`);
      const username = page.getByLabel("用户名");
      await pasteFromClipboard(page, username, "clipboard-user");
      await username.focus();
      await page.keyboard.press(process.platform === "darwin" ? "Meta+A" : "Control+A");
      await page.keyboard.press(process.platform === "darwin" ? "Meta+C" : "Control+C");
      await expect.poll(() => page.evaluate(() => navigator.clipboard.readText())).toBe("clipboard-user");
      await pasteFromClipboard(page, page.getByLabel("密码"), "clipboard-password-123");
      await page.getByRole("button", { name: "登录" }).click();
      await expect(page).toHaveURL(/\/goals\?view=goals$/);
      await page.getByRole("button", { name: /新建目标/ }).click();
      await pasteFromClipboard(page, page.getByLabel("说明"), "从剪贴板粘贴到多行说明\n第二行内容");

      await page.goto(`${portalUrl}/connections`);
      await page.getByRole("button", { name: /添加个人模型/ }).click();
      await pasteFromClipboard(page, page.getByLabel("API 密钥"), "local-test-model-secret");
      await pasteFromClipboard(page, page.getByLabel("上下文窗口（tokens）"), "128000");

      await page.goto(`${portalUrl}/apikeys`);
      await expect(page.getByRole("heading", { name: "我的密钥" })).toBeVisible();
      await page.getByRole("button", { name: /创建 API Key/ }).click();
      await pasteFromClipboard(page, page.getByLabel("名称"), "local-test-key");
      await pasteFromClipboard(page, page.getByLabel("有效期（天，可选）"), "30");
    } finally {
      await context.close();
    }
  });

  test("keeps the current page on API failure, hides raw JSON, and recovers on retry", async ({ browser }) => {
    const context = await browser.newContext({ serviceWorkers: "block" });
    const page = await context.newPage();
    let hostReads = 0;

    try {
      await disableDevelopmentMockWorker(context);
      await context.route("**/api/me", (route) => route.fulfill(json(user)));
      await context.route((url) => url.pathname === "/api/hosts", (route) => {
        hostReads += 1;
        if (hostReads <= 2) {
          return route.fulfill(json({
            error: {
              code: "internal",
              message: "RAW_SERVER_JSON_MESSAGE_SHOULD_NOT_APPEAR",
              details: { stack: "RAW_SERVER_JSON_STACK_SHOULD_NOT_APPEAR" },
            },
          }, 503));
        }
        return route.fulfill(json({ items: [] }));
      });
      await context.route((url) => url.pathname === "/api/shared-hosts", (route) => route.fulfill(json({ items: [] })));

      await page.goto(`${portalUrl}/hosts`);
      await expect(page.getByRole("heading", { name: "我的电脑", exact: true })).toBeVisible();
      await expect(page.getByRole("alert")).toContainText("服务暂时不可用，请稍后重试");
      expect(hostReads).toBe(2);
      await expect(page).toHaveURL(/\/hosts$/);
      await expect(page.locator("body")).not.toContainText("RAW_SERVER_JSON_MESSAGE_SHOULD_NOT_APPEAR");
      await expect(page.locator("body")).not.toContainText("RAW_SERVER_JSON_STACK_SHOULD_NOT_APPEAR");
      await expect(page.locator("body")).not.toContainText('"error"');

      await page.getByRole("button", { name: "重试" }).click();
      await expect(page.getByRole("heading", { name: "还没有主机" })).toBeVisible();
      await expect(page).toHaveURL(/\/hosts$/);
      expect(hostReads).toBe(3);
    } finally {
      await context.close();
    }
  });
});
