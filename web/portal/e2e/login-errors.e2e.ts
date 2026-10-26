import { expect, test } from "@playwright/test";

test("[mock] shows login rejection and Retry-After feedback as alerts", async ({ browser }) => {
  const context = await browser.newContext({ serviceWorkers: "block" });
  let loginAttempts = 0;

  try {
    await context.route("**/src/main.tsx*", async (route) => {
      const response = await route.fetch();
      const source = await response.text();
      const withoutDevWorker = source.replace(
        'if (import.meta.env.DEV && import.meta.env.VITE_MOCK_API === "1")',
        "if (false)",
      );
      expect(withoutDevWorker).not.toBe(source);
      await route.fulfill({ response, body: withoutDevWorker });
    });
    await context.route("**/api/me", (route) => route.fulfill({
      status: 401,
      contentType: "application/json",
      body: JSON.stringify({ error: { code: "unauthenticated", message: "请先登录", details: {} } }),
    }));
    await context.route("**/api/auth/login", (route) => {
      loginAttempts += 1;
      const rateLimited = loginAttempts === 2;
      return route.fulfill({
        status: rateLimited ? 429 : 401,
        ...(rateLimited ? { headers: { "Retry-After": "7" } } : {}),
        contentType: "application/json",
        body: JSON.stringify({
          error: {
            code: rateLimited ? "rate_limited" : "unauthenticated",
            message: rateLimited ? "登录尝试过于频繁" : "用户名或密码错误",
            details: {},
          },
        }),
      });
    });

    const page = await context.newPage();
    await page.goto(new URL("/login", process.env.CH_PORTAL_E2E_URL ?? "http://127.0.0.1:5183").href);
    await page.getByLabel("用户名").fill("wrong-user");
    await page.getByLabel("密码").fill("wrong-password");
    await page.getByRole("button", { name: "登录" }).click();
    const alert = page.getByRole("alert");
    await expect(alert).toHaveText("用户名或密码错误，请重试。");

    await page.getByRole("button", { name: "登录" }).click();
    await expect(alert).toHaveText("登录尝试过于频繁。请在 7 秒后重试。");
    expect(loginAttempts).toBe(2);
  } finally {
    await context.close();
  }
});
