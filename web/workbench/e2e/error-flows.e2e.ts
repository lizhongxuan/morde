import { expect, test } from "@playwright/test";

test("[mock] shows readable file read, upload, and download errors", async ({ browser }) => {
  const context = await browser.newContext({ serviceWorkers: "block" });
  let listRequests = 0;
  const readError = "读取失败：文件权限不足";
  const uploadError = "存储服务暂时不可用，请稍后重试。";

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
    await context.route("**/__ch/api/host", (route) => route.fulfill({
      contentType: "application/json",
      body: JSON.stringify({
        id: "h1234567",
        name: "本地测试主机",
        portal_origin: "http://127.0.0.1:5183",
        state: "running",
        expires_at: new Date(Date.now() + 60 * 60 * 1000).toISOString(),
      }),
    }));
    await context.route("**/__ch/api/control", (route) => route.fulfill({
      contentType: "application/json",
      body: JSON.stringify({ host_id: "h1234567", control_state: "user", control_version: 1, active_transition: null }),
    }));
    await context.route("**/vnc/index.html*", (route) => route.fulfill({
      contentType: "text/html",
      body: "<!doctype html><html class=\"noVNC_connected\"><title>local desktop mock</title></html>",
    }));
    await context.route("**/v1/file/list", async (route) => {
      listRequests += 1;
      const request = route.request();
      expect(request.method()).toBe("POST");
      expect(request.postDataJSON()).toMatchObject({ path: "/home/gem/workspace" });
      await route.fulfill({
        contentType: "application/json",
        body: JSON.stringify({
          success: true,
          message: "ok",
          data: {
            path: "/home/gem/workspace",
            files: [{
              name: "README.md",
              path: "/home/gem/workspace/README.md",
              is_directory: false,
              size: 12,
              extension: ".md",
            }],
          },
        }),
      });
    });
    await context.route("**/v1/file/read", (route) => route.fulfill({
      contentType: "application/json",
      body: JSON.stringify({
        success: false,
        message: readError,
        data: { error_type: "permission_denied" },
      }),
    }));
    await context.route("**/v1/file/upload", async (route) => {
      expect(route.request().method()).toBe("POST");
      expect(route.request().postData() ?? "").toContain("failed-upload.txt");
      await route.fulfill({
        contentType: "application/json",
        body: JSON.stringify({ success: false, message: uploadError, data: {} }),
      });
    });
    await context.route("**/v1/file/download?**", (route) => route.fulfill({
      status: 503,
      contentType: "text/plain",
      body: "local synthetic download failure",
    }));

    const page = await context.newPage();
    await page.goto("/");
    await expect(page.getByText("本地测试主机")).toBeVisible();
    await page.getByRole("button", { name: "文件", exact: true }).click();
    await expect(page.getByRole("button", { name: "README.md" })).toBeVisible();
    expect(listRequests).toBeGreaterThanOrEqual(1);

    await page.getByRole("button", { name: "README.md" }).click();
    await expect(page.getByText(readError, { exact: true })).toBeVisible();

    const listRequestsBeforeUpload = listRequests;
    const preflightList = page.waitForRequest((request) =>
      new URL(request.url()).pathname === "/v1/file/list" && request.postDataJSON().show_hidden === true,
    );
    const refreshedList = page.waitForRequest((request) =>
      new URL(request.url()).pathname === "/v1/file/list" && request.postDataJSON().show_hidden === false,
    );
    await page.locator("#file-upload").setInputFiles({
      name: "failed-upload.txt",
      mimeType: "text/plain",
      buffer: Buffer.from("synthetic local upload"),
    });
    const [preflightRequest, refreshRequest] = await Promise.all([preflightList, refreshedList]);
    expect(preflightRequest.postDataJSON()).toMatchObject({ path: "/home/gem/workspace", show_hidden: true });
    expect(refreshRequest.postDataJSON()).toMatchObject({ path: "/home/gem/workspace", show_hidden: false });
    await expect(page.getByText(uploadError, { exact: true })).toBeVisible();
    await expect(page.getByRole("button", { name: "README.md" })).toBeVisible();
    await expect(page.getByRole("button", { name: "failed-upload.txt" })).toHaveCount(0);
    expect(listRequests).toBe(listRequestsBeforeUpload + 2);

    const downloadResponse = page.waitForResponse((response) =>
      new URL(response.url()).pathname === "/v1/file/download" && response.status() === 503,
    );
    await page.getByRole("row").filter({ hasText: "README.md" })
      .getByRole("button", { name: "下载" }).click();
    expect((await downloadResponse).status()).toBe(503);
    await expect(page.getByText("下载失败（HTTP 503）", { exact: true })).toBeVisible();
  } finally {
    await context.close();
  }
});
