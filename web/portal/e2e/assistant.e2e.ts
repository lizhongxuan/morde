import { expect, test } from "@playwright/test";
import { createServer } from "node:http";

test.describe("Assistant desktop workspace", () => {
  test.skip(process.env.CH_REAL_E2E === "1", "synthetic assistant scenario is disabled for real-host runs");

  test("native EventSource resumes after the snapshot-required cursor", async ({ browser }) => {
    const lastEventIDs: string[] = [];
    const server = createServer((request, response) => {
      if (request.url === "/events") {
        const lastEventID = request.headers["last-event-id"];
        lastEventIDs.push(Array.isArray(lastEventID) ? lastEventID[0] ?? "" : lastEventID ?? "");
        if (lastEventIDs.length === 1) {
          response.writeHead(200, { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" });
          response.write("retry: 20\nid: 8\nevent: task.snapshot_required\ndata: {\"sequence\":8}\n\n");
          setTimeout(() => response.end(), 50);
          return;
        }
        response.writeHead(204);
        response.end();
        return;
      }
      response.writeHead(200, { "Content-Type": "text/html" });
      response.end("<!doctype html><html><body></body></html>");
    });
    const context = await browser.newContext();
    try {
      await new Promise<void>((resolve, reject) => {
        server.once("error", reject);
        server.listen(0, "127.0.0.1", resolve);
      });
      const address = server.address();
      if (!address || typeof address === "string") throw new Error("test SSE server did not bind to a TCP port");
      const page = await context.newPage();
      await page.goto(`http://127.0.0.1:${address.port}/`);
      await page.evaluate(() => { new EventSource("/events"); });
      await expect.poll(() => lastEventIDs.length, { timeout: 5_000 }).toBe(2);
      expect(lastEventIDs).toEqual(["", "8"]);
    } finally {
      await context.close();
      await new Promise<void>((resolve) => server.close(() => resolve()));
    }
  });

  test("keeps the primary workspace within desktop viewports", async ({ page }) => {
    await page.goto("/");
    await page.getByLabel("用户名").fill("demo");
    await page.getByLabel("密码").fill("demo");
    await page.getByRole("button", { name: "登录" }).click();
    await page.getByRole("link", { name: "AI 助手", exact: true }).click();
    await expect(page.getByRole("heading", { name: "AI 助手" })).toBeVisible();

    for (const viewport of [{ width: 1280, height: 720 }, { width: 1920, height: 1080 }]) {
      await page.setViewportSize(viewport);
      const dimensions = await page.evaluate(() => ({ width: document.documentElement.clientWidth, scrollWidth: document.documentElement.scrollWidth }));
      expect(dimensions.scrollWidth, `page must not scroll horizontally at ${viewport.width}×${viewport.height}`).toBeLessThanOrEqual(dimensions.width);
      await expect(page.getByRole("navigation", { name: "工作导航" })).toBeVisible();
      await expect(page.getByRole("link", { name: "AI 助手" })).toBeVisible();
      await expect(page.getByRole("link", { name: "任务与计划", exact: true })).toBeVisible();
      await expect(page.getByRole("link", { name: "云端电脑", exact: true })).toBeVisible();
      await expect(page.getByRole("link", { name: "模型连接", exact: true })).toBeVisible();
    }
  });

  test("keeps navigation and assistant controls usable with larger text and 200% zoom", async ({ page }) => {
    await page.goto("/");
    await page.getByLabel("用户名").fill("demo");
    await page.getByLabel("密码").fill("demo");
    await page.getByRole("button", { name: "登录" }).click();
    await page.getByRole("link", { name: "AI 助手", exact: true }).click();
    await expect(page.getByRole("heading", { name: "AI 助手" })).toBeVisible();

    for (const scenario of [
      { name: "150% text", width: 1280, height: 720, fontSize: "24px" },
      { name: "200% zoom", width: 640, height: 360, fontSize: "16px" },
    ]) {
      await page.setViewportSize({ width: scenario.width, height: scenario.height });
      await page.evaluate((fontSize) => { document.documentElement.style.fontSize = fontSize; }, scenario.fontSize);

      const dimensions = await page.evaluate(() => ({
        width: document.documentElement.clientWidth,
        scrollWidth: document.documentElement.scrollWidth,
      }));
      expect(dimensions.scrollWidth, `page must not scroll horizontally with ${scenario.name}`).toBeLessThanOrEqual(dimensions.width);

      const navigation = page.getByRole("navigation", { name: "工作导航" });
      await expect(navigation).toBeVisible();
      for (const name of ["AI 助手", "任务与计划", "云端电脑", "模型连接"]) {
        await expect(navigation.getByRole("link", { name, exact: true })).toBeVisible();
      }
      const objective = page.getByRole("textbox", { name: "要完成的任务" });
      await expect(objective).toBeVisible();
      const submit = page.getByRole("button", { name: "交办任务" });
      await expect(submit).toHaveAccessibleName("交办任务");
      await objective.focus();
      await expect(objective).toBeFocused();
    }
  });

  test("opens a conversation with over 100 messages and loads older history without duplicates", async ({ browser }) => {
    const user = {
      id: "u1234567", name: "demo", role: "user",
      quota: { hosts: 3, cpu_milli: 6000, mem_mib: 12288 }, usage: { hosts: 0, cpu_milli: 0, mem_mib: 0 },
    };
    const conversationID = "chist0001";
    const taskID = "qhist0001";
    const messages = Array.from({ length: 200 }, (_, index) => ({
      id: `m${String(index + 1).padStart(7, "0")}`,
      conversation_id: conversationID,
      role: index % 2 === 0 ? "user" : "assistant",
      content: `历史消息-${String(index + 1).padStart(3, "0")}`,
      task_id: taskID,
      created_at: new Date(Date.UTC(2026, 0, 1, 0, index)).toISOString(),
    }));
    const messagePageRequests: URL[] = [];
    const context = await browser.newContext({ serviceWorkers: "block" });
    await context.route("**/src/main.tsx*", async (route) => {
      const response = await route.fetch();
      const source = await response.text();
      const withoutMockWorker = source.replace('if (import.meta.env.DEV && import.meta.env.VITE_MOCK_API === "1")', "if (false)");
      expect(withoutMockWorker).not.toBe(source);
      await route.fulfill({ response, body: withoutMockWorker });
    });
    await context.route("**/api/me", (route) => route.fulfill({
      status: 401, contentType: "application/json", body: JSON.stringify({ error: { code: "unauthenticated", message: "请先登录", details: {} } }),
    }));
    await context.route("**/api/auth/login", (route) => route.fulfill({
      status: 200, contentType: "application/json", headers: { "Set-Cookie": "ch_portal=test-session; Path=/; SameSite=Lax" }, body: JSON.stringify({ user }),
    }));
    await context.route("**/api/hosts", (route) => route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ items: [] }) }));
    await context.route("**/api/model-connections", (route) => route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ items: [] }) }));
    await context.route("**/api/me/preferences", (route) => route.fulfill({
      status: 200, contentType: "application/json", body: JSON.stringify({ assistant_name: "Morde", selected_model_id: null, default_host_id: null, updated_at: null }),
    }));
    await context.route("**/api/onboarding/status", (route) => route.fulfill({
      status: 200, contentType: "application/json", body: JSON.stringify({
        model_status: "not_configured", models: [], selected_model_id: null, computer_status: "not_created", computers: [],
        default_host_id: null, can_submit: false, next_action: "configure_model",
      }),
    }));
    await context.route("**/api/conversations*", (route) => route.fulfill({
      status: 200, contentType: "application/json", body: JSON.stringify({
        items: [{ id: conversationID, host_id: "hhist0001", title: "长对话历史", created_at: messages[0]!.created_at, updated_at: messages[199]!.created_at }],
        next_cursor: null,
      }),
    }));
    await context.route(`**/api/conversations/${conversationID}/messages*`, async (route) => {
      const url = new URL(route.request().url());
      messagePageRequests.push(url);
      if (url.searchParams.get("limit") !== "100") {
        return route.fulfill({ status: 400, contentType: "application/json", body: JSON.stringify({ error: { code: "invalid_argument", message: "limit 无效", details: {} } }) });
      }
      const cursor = url.searchParams.get("cursor");
      if (cursor === null) {
        return route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ items: messages.slice(100), next_cursor: "older-page-cursor" }) });
      }
      if (cursor === "older-page-cursor") {
        return route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ items: messages.slice(0, 100), next_cursor: null }) });
      }
      return route.fulfill({ status: 400, contentType: "application/json", body: JSON.stringify({ error: { code: "invalid_argument", message: "cursor 无效", details: {} } }) });
    });
    await context.route(`**/api/tasks/${taskID}`, (route) => route.fulfill({
      status: 200, contentType: "application/json", body: JSON.stringify({
        id: taskID, owner_id: user.id, host_id: "hhist0001", conversation_id: conversationID, model_id: "mhist0001",
        objective: "本地合成历史记录", status: "completed", current_step: null, result: null, waiting_reason: null,
        budget: { active_seconds: 1800, tool_calls: 100, model_tokens: 50000 }, usage: { active_seconds: 0, tool_calls: 0, model_tokens: null },
        steps: [], batch_items: [], artifacts: [], pending_request: null, clarification_request: null, stop_request: null,
        created_at: messages[0]!.created_at, updated_at: messages[199]!.created_at, finished_at: messages[199]!.created_at,
      }),
    }));

    const page = await context.newPage();
    try {
      await page.goto(`${process.env.CH_PORTAL_E2E_URL ?? "http://127.0.0.1:5183"}/login`);
      await page.getByLabel("用户名").fill("demo");
      await page.getByLabel("密码").fill("local-only");
      await page.getByRole("button", { name: "登录" }).click();
      await expect(page.getByRole("heading", { name: "AI 助手" })).toBeVisible();
      await page.getByRole("button", { name: /长对话历史/ }).click();

      const renderedMessages = page.locator("article.message-block .chat-message p");
      await expect(renderedMessages).toHaveCount(100);
      await expect(renderedMessages.first()).toHaveText("历史消息-101");
      await expect(renderedMessages.last()).toHaveText("历史消息-200");
      expect(messagePageRequests).toHaveLength(1);
      expect(messagePageRequests[0]!.searchParams.get("cursor")).toBeNull();
      expect(messagePageRequests[0]!.searchParams.get("limit")).toBe("100");

      await page.getByRole("button", { name: "加载更早消息" }).click();
      await expect(renderedMessages).toHaveCount(200);
      const renderedContents = await renderedMessages.allTextContents();
      expect(renderedContents).toEqual(messages.map((message) => message.content));
      expect(new Set(renderedContents).size).toBe(200);
      expect(renderedContents.filter((content) => content === "历史消息-200")).toHaveLength(1);
      await expect(page.getByRole("button", { name: "加载更早消息" })).toHaveCount(0);
      expect(messagePageRequests).toHaveLength(2);
      expect(messagePageRequests[1]!.searchParams.get("cursor")).toBe("older-page-cursor");
      expect(messagePageRequests[1]!.searchParams.get("limit")).toBe("100");
    } finally {
      await context.close();
    }
  });

  test("creates a local demo task, persists its conversation, then reopens its task details", async ({ page }) => {
    const objective = `本地验收任务 ${Date.now()}`;
    await page.goto("/");
    await page.getByLabel("用户名").fill("demo");
    await page.getByLabel("密码").fill("demo");
    await page.getByRole("button", { name: "登录" }).click();
    await page.getByRole("link", { name: "AI 助手", exact: true }).click();
    await expect(page.getByRole("heading", { name: "AI 助手" })).toBeVisible();

    await page.getByRole("combobox", { name: "AI 模型" }).selectOption("pmodel01");
    await expect(page.getByText("默认模型已保存。")).toBeVisible();
    await page.getByRole("button", { name: "创建我的云端电脑" }).click();
    await expect(page.getByRole("button", { name: "创建我的云端电脑" })).toBeHidden({ timeout: 10_000 });
    await expect(page.getByRole("option", { name: /我的云端电脑 · 运行中/ })).toBeAttached({ timeout: 10_000 });

    await page.getByRole("textbox", { name: "要完成的任务" }).fill(objective);
    await page.getByRole("button", { name: /交办任务/ }).click();
    await expect(page.getByText("任务已接收，可在此查看进度。")).toBeVisible();
    await expect(page.getByRole("heading", { name: objective })).toBeVisible();
    await expect(page.getByText("排队中").first()).toBeVisible();

    await page.reload();
    await expect(page.getByRole("heading", { name: "AI 助手" })).toBeVisible();
    await page.getByRole("button", { name: new RegExp(objective) }).click();
    await expect(page.getByRole("heading", { name: objective })).toBeVisible();
    await page.getByRole("link", { name: "任务与计划", exact: true }).click();
    await page.getByRole("link", { name: `查看任务 ${objective}` }).click();
    await expect(page.getByRole("heading", { name: objective })).toBeVisible();
    await expect(page.getByRole("heading", { name: "执行进度" })).toBeVisible();
    await expect(page.getByRole("heading", { name: "用量" })).toBeVisible();
  });
});
