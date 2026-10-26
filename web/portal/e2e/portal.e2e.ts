import { expect, test } from "@playwright/test";

function isLocalUrl(value: string): boolean {
  try {
    const hostname = new URL(value).hostname;
    return hostname === "localhost" || hostname === "127.0.0.1" || hostname === "[::1]" || hostname.endsWith(".localhost");
  } catch {
    return false;
  }
}

if (
  process.env.CH_REAL_E2E === "1" &&
  process.env.CH_PORTAL_URL &&
  !isLocalUrl(process.env.CH_PORTAL_URL)
) {
  throw new Error("real-host tests are restricted to local portal URLs");
}

test.describe("portal mock flow", () => {
  test.skip(process.env.CH_REAL_E2E === "1", "mock scenario is disabled for a real-host run");

  test("[mock] local-only target guard accepts loopback and rejects remote hosts", () => {
    for (const [url, allowed] of [
      ["http://localhost", true],
      ["http://127.0.0.1", true],
      ["http://[::1]", true],
      ["http://portal.localhost", true],
      ["http://example.com", false],
    ] as const) {
      expect(isLocalUrl(url)).toBe(allowed);
    }
  });

  test("creates, enters, and deletes a host in at most three post-create clicks", async ({ page }) => {
    await page.goto("/");
    const username = page.getByLabel("用户名");
    await username.focus();
    await page.keyboard.type("demo");
    const password = page.getByLabel("密码");
    await password.focus();
    await page.keyboard.type("demo");
    await password.press("Enter");
    await page.evaluate(() => {
      if (document.activeElement instanceof HTMLElement) document.activeElement.blur();
    });
    await page.keyboard.press("Tab");
    const skipLink = page.getByRole("link", { name: "跳到主要内容" });
    await expect(skipLink).toBeFocused();
    await page.keyboard.press("Enter");
    await expect(page.locator("#main-content")).toBeFocused();

    await page.getByRole("link", { name: "云端电脑", exact: true }).click();
    await expect(page.getByRole("heading", { name: "云端电脑", exact: true })).toBeVisible();

    const createLink = page.getByRole("link", { name: "创建电脑" });
    await createLink.focus();
    await page.keyboard.press("Enter");
    const hostName = page.getByLabel(/主机名称/);
    await hostName.focus();
    await page.keyboard.type("Playwright 本地验收");
    const createRequestPromise = page.waitForRequest((request) =>
      request.url() === new URL("/api/hosts", process.env.CH_PORTAL_E2E_URL ?? "http://127.0.0.1:5183").href && request.method() === "POST",
    );
    await page.evaluate(() => {
      const target = window as typeof window & { postCreateClicks?: number };
      target.postCreateClicks = 0;
      document.addEventListener(
        "click",
        () => {
          target.postCreateClicks = (target.postCreateClicks ?? 0) + 1;
        },
        true,
      );
    });
    const createHost = page.getByRole("button", { name: "创建主机" });
    await createHost.focus();
    await page.keyboard.press("Enter");
    const createRequest = await createRequestPromise;
    expect(createRequest.headers()["idempotency-key"]).toMatch(/^[A-Za-z0-9._~-]{8,128}$/);
    const host = page.locator(".host-card").filter({ hasText: "Playwright 本地验收" });
    await expect(host.getByText("运行中")).toBeVisible({ timeout: 10_000 });

    const popupPromise = page.waitForEvent("popup");
    const enter = host.getByRole("button", { name: "进入主机 Playwright 本地验收" });
    await enter.focus();
    await page.keyboard.press("Enter");
    const popup = await popupPromise;
    await expect(popup.getByRole("navigation", { name: "工作台功能" })).toBeVisible({
      timeout: 10_000,
    });
    await expect(popup.getByText("本地测试主机")).toBeVisible();
    expect(new URL(popup.url()).origin).toBe(
      new URL(process.env.CH_WORKBENCH_E2E_URL ?? "http://127.0.0.1:5184").origin,
    );
    const postCreateClicks = await page.evaluate(
      () => (window as typeof window & { postCreateClicks?: number }).postCreateClicks ?? 0,
    );
    expect(postCreateClicks).toBeLessThanOrEqual(3);

    const deleteHost = host.getByRole("button", { name: "删除主机 Playwright 本地验收" });
    await deleteHost.focus();
    await page.keyboard.press("Enter");
    const dialog = page.getByRole("dialog");
    await expect(dialog.getByRole("button", { name: "关闭" })).toBeFocused();
    await page.keyboard.press("Shift+Tab");
    await expect(dialog.getByRole("button", { name: "删除主机" })).toBeFocused();
    await page.keyboard.press("Enter");
    await expect(page.getByText("还没有主机")).toBeVisible();
    await popup.close();
  });

  test("creates and revokes an API Key through the contracted endpoint", async ({ page }) => {
    const deletePaths: string[] = [];
    page.on("request", (request) => {
      if (request.method() === "DELETE") deletePaths.push(new URL(request.url()).pathname);
    });

    await page.goto("/");
    await page.getByLabel("用户名").fill("demo");
    await page.getByLabel("密码").fill("demo");
    await page.getByRole("button", { name: "登录" }).click();
    await page.getByRole("link", { name: "AI 助手", exact: true }).click();
    await expect(page.getByRole("heading", { name: "AI 助手" })).toBeVisible();
    await page.getByRole("link", { name: "API Key", exact: true }).click();
    await page.getByLabel("名称").fill("门户吊销验收");
    await page.getByRole("checkbox", { name: "创建新主机 host:create" }).check();
    await page.getByRole("button", { name: "创建 API Key" }).click();
    const secretDialog = page.getByRole("dialog", { name: "API Key 已创建" });
    await expect(secretDialog).toBeVisible();
    await expect(secretDialog.getByLabel("凭证")).toHaveValue("chk_mock-one-time-key");
    await secretDialog.getByRole("button", { name: "关闭并清除" }).click();
    await expect(secretDialog).toHaveCount(0);
    await expect(page.getByRole("row").filter({ hasText: "门户吊销验收" })).toBeVisible();
    await expect(page.getByText("chk_mock-one-time-key", { exact: true })).toHaveCount(0);
    await page.getByRole("button", { name: "吊销 API Key 门户吊销验收" }).click();
    await page.getByRole("dialog").getByRole("button", { name: "吊销 Key" }).click();
    const revokedRow = page.getByRole("row").filter({ hasText: "门户吊销验收" });
    await expect(revokedRow.getByText("已吊销")).toBeVisible();
    await expect(revokedRow.getByRole("button", { name: "吊销 API Key 门户吊销验收" })).toHaveCount(0);

    expect(deletePaths).toHaveLength(1);
    expect(deletePaths[0]).toMatch(/^\/api\/apikeys\//);
  });

  test("hides admin pages and returns 403 from admin APIs for a regular user", async ({ page }) => {
    await page.goto("/");
    await page.getByLabel("用户名").fill("member");
    await page.getByLabel("密码").fill("member");
    await page.getByRole("button", { name: "登录" }).click();
    await page.getByRole("link", { name: "AI 助手", exact: true }).click();
    await expect(page.getByRole("heading", { name: "AI 助手" })).toBeVisible();
    await expect(page.getByRole("link", { name: "管理后台" })).toHaveCount(0);
    await page.goto("/admin/users");
    await expect(page.getByRole("heading", { name: "没有管理权限" })).toBeVisible();

    const statuses = await page.evaluate(async () => {
      const requests: Array<[string, RequestInit?]> = [
        ["/api/admin/hosts"], ["/api/admin/users"], ["/api/admin/capacity"], ["/api/admin/audit"],
        ["/api/admin/users/u1a2b3c4", { method: "PATCH", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ disabled: true }) }],
      ];
      return Promise.all(requests.map(async ([url, init]) => (await fetch(url, init)).status));
    });
    expect(statuses).toEqual([403, 403, 403, 403, 403]);
  });
});

test("real single-host flow creates, uses, and cleans up only its own host", async ({ page }) => {
  test.skip(
    process.env.CH_REAL_E2E !== "1" ||
      !process.env.CH_PORTAL_URL ||
      !process.env.CH_USERNAME ||
      !process.env.CH_PASSWORD,
    "set CH_REAL_E2E=1 and local CH_PORTAL_URL/CH_USERNAME/CH_PASSWORD to opt in",
  );

  test.setTimeout(5 * 60_000);

  const portalUrl = process.env.CH_PORTAL_URL!;
  const hostName = `codex-e2e-${Date.now()}-${Math.random().toString(36).slice(2, 8)}`;
  let hostId: string | undefined;
  let cleanupError: unknown;

  await page.goto("/");
  await page.getByLabel("用户名").fill(process.env.CH_USERNAME!);
  await page.getByLabel("密码").fill(process.env.CH_PASSWORD!);
  await page.getByRole("button", { name: "登录" }).click();
  await page.getByRole("link", { name: "AI 助手", exact: true }).click();
  await expect(page.getByRole("heading", { name: "AI 助手" })).toBeVisible();

  try {
    await page.getByRole("link", { name: "云端电脑", exact: true }).click();
    await page.getByRole("link", { name: "创建电脑" }).click();
    await page.getByLabel(/主机名称/).fill(hostName);
    const createResponsePromise = page.waitForResponse(
      (response) => {
        const request = response.request();
        return (
          response.url() === new URL("/api/hosts", portalUrl).href &&
          request.method() === "POST" &&
          request.postDataJSON().name === hostName
        );
      },
    );
    await page.getByRole("button", { name: "创建主机" }).click();
    const createResponse = await createResponsePromise;
    if (createResponse.status() === 201) {
      const createdHost = (await createResponse.json()) as { id?: unknown };
      if (typeof createdHost.id === "string" && createdHost.id) hostId = createdHost.id;
    }
    expect(hostId, "host creation response must identify the created host").toBeTruthy();
    const host = page.locator(".host-card").filter({ hasText: hostName });
    await expect(host).toBeVisible();
    await expect(host.getByText("运行中")).toBeVisible({ timeout: 10 * 60_000 });
    const hostResponse = await page.request.get(
      new URL("/api/hosts/" + hostId, portalUrl).href,
    );
    expect(hostResponse.ok()).toBeTruthy();
    const hostData = (await hostResponse.json()) as { url?: string };
    if (!hostData.url || !isLocalUrl(hostData.url)) {
      throw new Error("refusing to open a nonlocal host URL during local-only verification");
    }

    const popupPromise = page.waitForEvent("popup");
    await host.getByRole("button", { name: "进入主机 " + hostName }).click();
    const popup = await popupPromise;
    await expect(popup.getByRole("navigation", { name: "工作台功能" })).toBeVisible({
      timeout: 120_000,
    });
    await popup.getByRole("button", { name: "终端", exact: true }).click();
    const terminal = popup.frameLocator('iframe[title="主机终端"]');
    await expect(popup.getByTitle("主机终端")).toBeVisible({ timeout: 30_000 });
    const fileName = "codex-e2e-" + Date.now() + ".txt";
    const filePath = "/home/gem/workspace/" + fileName;
    await terminal.locator(".xterm-screen").click();
    await popup.keyboard.type("printf playwright-e2e > '" + filePath + "' && printf terminal-e2e-ready");
    await popup.keyboard.press("Enter");
    await popup.getByRole("button", { name: "文件", exact: true }).click();
    await expect(async () => {
      await popup.getByRole("button", { name: "刷新" }).click();
      await expect(popup.getByRole("button", { name: fileName })).toBeVisible();
    }).toPass({ timeout: 30_000 });
    await popup.getByRole("button", { name: fileName }).click();
    await expect(popup.getByLabel("文件内容", { exact: true })).toHaveValue("playwright-e2e");
    await popup.close();
  } finally {
    if (!hostId) {
      try {
        const listResponse = await page.request.get(new URL("/api/hosts", portalUrl).href);
        if (!listResponse.ok()) throw new Error("failed to locate e2e host for cleanup: HTTP " + listResponse.status());
        const payload = await listResponse.json() as { items?: Array<{ id?: unknown; name?: unknown }> };
        if (!Array.isArray(payload.items)) throw new Error("host list response is invalid during e2e cleanup");
        const matches = payload.items.filter((host) => host.name === hostName);
        if (matches.length > 1) throw new Error("multiple hosts match this e2e run; refusing ambiguous cleanup");
        if (matches.length === 1) {
          if (typeof matches[0]?.id !== "string" || !matches[0].id) throw new Error("matched e2e host has no ID");
          hostId = matches[0].id;
        }
      } catch (error) {
        cleanupError = error;
      }
    }
    if (hostId) {
      try {
        const response = await page.request.delete(
          new URL("/api/hosts/" + encodeURIComponent(hostId) + "?keepWorkspace=false", portalUrl)
            .href,
          { headers: { Origin: new URL(portalUrl).origin, "Content-Type": "application/json" } },
        );
        if (!response.ok() && response.status() !== 404) {
          cleanupError = new Error("failed to clean up e2e host: HTTP " + response.status());
        }
      } catch (error) {
        cleanupError = error;
      }
    }
    if (cleanupError) throw cleanupError;
  }
});
