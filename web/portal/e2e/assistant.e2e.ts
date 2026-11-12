import { expect, test } from "@playwright/test";

test.describe("Assistant durable tasks", () => {
  test.skip(process.env.CH_REAL_E2E === "1", "mock chat scenario is disabled for real-host runs");

  test("keeps topbar forward navigation after returning to the prior page", async ({ page }) => {
    await page.goto("/");
    await page.getByLabel("用户名").fill("demo");
    await page.getByLabel("密码").fill("demo");
    await page.getByRole("button", { name: "登录" }).click();
    await page.goto("/assistant");

    await page.getByRole("link", { name: "个人数据", exact: true }).click();
    await expect(page).toHaveURL(/\/personal$/);
    await page.getByRole("button", { name: "返回上一页" }).click();
    await expect(page).toHaveURL(/\/assistant$/);
    await expect(page.getByRole("button", { name: "前进" })).toBeEnabled();
    await page.getByRole("button", { name: "前进" }).click();
    await expect(page).toHaveURL(/\/personal$/);
  });

  test("keeps portal pages within the viewport at desktop and narrow widths", async ({ page }) => {
    await page.goto("/");
    await page.getByLabel("用户名").fill("demo");
    await page.getByLabel("密码").fill("demo");
    await page.getByRole("button", { name: "登录" }).click();

    const routes = ["/assistant", "/personal", "/goals", "/hosts", "/clients", "/apikeys", "/notifications", "/connections", "/settings", "/projects", "/workspaces"];
    const routesWithContextSidebar = new Set(["/assistant", "/personal", "/goals", "/hosts", "/clients", "/apikeys", "/connections", "/settings"]);
    for (const viewport of [{ width: 1440, height: 900 }, { width: 1024, height: 768 }, { width: 800, height: 600 }, { width: 480, height: 720 }]) {
      await page.setViewportSize(viewport);
      for (const route of routes) {
        await page.goto(route);
        await expect(page.locator(".morde-shell")).toBeVisible();
        const sidebarToggle = page.getByRole("button", { name: /折叠侧边栏|展开侧边栏/ });
        if (routesWithContextSidebar.has(route)) {
          await expect(sidebarToggle).toBeVisible();
          const expanded = await sidebarToggle.getAttribute("aria-expanded");
          await sidebarToggle.click();
          await expect(sidebarToggle).toHaveAttribute("aria-expanded", expanded === "true" ? "false" : "true");
          await sidebarToggle.click();
        } else {
          await expect(sidebarToggle).toHaveCount(0);
        }
        const dimensions = await page.evaluate(() => ({
          width: document.documentElement.clientWidth,
          height: document.documentElement.clientHeight,
          scrollWidth: document.documentElement.scrollWidth,
          scrollHeight: document.documentElement.scrollHeight,
        }));
        expect(dimensions.scrollWidth, `${route} at ${viewport.width}px`).toBeLessThanOrEqual(dimensions.width + 1);
        expect(dimensions.scrollHeight, `${route} at ${viewport.width}x${viewport.height}`).toBeLessThanOrEqual(dimensions.height + 1);
      }
    }
  });

  test("starts with one stopped 1-core 2-GiB computer and shows the quota message", async ({ page }) => {
    await page.goto("/");
    await page.getByLabel("用户名").fill("demo");
    await page.getByLabel("密码").fill("demo");
    await page.getByRole("button", { name: "登录" }).click();
    await page.goto("/hosts");
    await expect(page.locator(".host-card").first()).toContainText("1 核 / 2 GiB");
    await expect(page.locator(".host-card").first()).toContainText("已停止");
    const layoutBefore = await page.evaluate(() => [".morde-topbar", ".morde-sidebar", ".morde-main"].map((selector) => {
      const { x, y, width, height } = document.querySelector(selector)!.getBoundingClientRect();
      return [x, y, width, height];
    }));
    await page.getByRole("button", { name: "＋ 创建电脑", exact: true }).click();
    const alert = page.getByRole("alert").filter({ hasText: "达到云端电脑上限,无法再创建" });
    await expect(alert).toBeVisible();
    await expect(page.locator(".toast-region")).toHaveCSS("position", "fixed");
    expect(await alert.evaluate((node) => node.closest(".morde-shell"))).toBeNull();
    const layoutAfter = await page.evaluate(() => [".morde-topbar", ".morde-sidebar", ".morde-main"].map((selector) => {
      const { x, y, width, height } = document.querySelector(selector)!.getBoundingClientRect();
      return [x, y, width, height];
    }));
    expect(layoutAfter).toEqual(layoutBefore);
  });

  test("keeps assistant error notifications above the composer at compact viewport sizes", async ({ page }) => {
    await page.goto("/");
    await page.getByLabel("用户名").fill("demo");
    await page.getByLabel("密码").fill("demo");
    await page.getByRole("button", { name: "登录" }).click();

    for (const viewport of [{ width: 1440, height: 900 }, { width: 1024, height: 768 }, { width: 800, height: 600 }]) {
      await page.setViewportSize(viewport);
      await page.goto("/assistant");
      const composer = page.locator(".assistant-composer");
      const region = page.locator(".toast-region");
      await expect(composer).toBeVisible();
      await region.evaluate((element) => {
        for (const message of ["网络异常，部分对话数据暂时无法读取。", "模型连接暂不可用。"]) {
          const error = document.createElement("div");
          error.className = "toast toast-error";
          error.setAttribute("role", "alert");
          error.textContent = message;
          element.append(error);
        }
      });
      const errors = region.getByRole("alert");
      await expect(errors).toHaveCount(2);
      await expect(errors.first()).toBeVisible();
      const geometry = await page.evaluate(() => ({
        errorBottoms: Array.from(document.querySelectorAll(".toast-region [role=alert]"), (element) => element.getBoundingClientRect().bottom),
        composerTop: document.querySelector(".assistant-composer")!.getBoundingClientRect().top,
        regionPosition: getComputedStyle(document.querySelector(".toast-region")!).position,
      }));
      expect(geometry.regionPosition).toBe("fixed");
      expect(geometry.errorBottoms.every((bottom) => bottom <= geometry.composerTop - 8), `${viewport.width}x${viewport.height}`).toBeTruthy();
    }
  });

  test("keeps the selected icon centered in the collapsed navigation rail", async ({ page }) => {
    await page.goto("/");
    await page.getByLabel("用户名").fill("demo");
    await page.getByLabel("密码").fill("demo");
    await page.getByRole("button", { name: "登录" }).click();
    await page.goto("/assistant");
    const activeLink = page.locator("#primary-navigation .morde-nav-link.active").first();
    const positions = await activeLink.evaluate((element) => {
      const icon = element.querySelector<SVGElement>(".morde-icon")!;
      const position = () => {
        const { x, y } = icon.getBoundingClientRect();
        return { x, y };
      };
      const before = position();
      element.classList.remove("active");
      const unselected = position();
      element.classList.add("active");
      return { before, unselected, selected: position() };
    });
    expect(positions.before.x).toBeCloseTo(positions.unselected.x, 2);
    expect(positions.before.y).toBeCloseTo(positions.unselected.y, 2);
    expect(positions.selected.x).toBeCloseTo(positions.unselected.x, 2);
    expect(positions.selected.y).toBeCloseTo(positions.unselected.y, 2);
  });

  test("keeps the cached main conversation visible when network history reads fail", async ({ page }) => {
    await page.addInitScript(() => {
      localStorage.setItem(`morde.assistant.mainConversation.v1:${location.origin}:u1a2b3c4`, JSON.stringify({
        conversationID: "cmain0001", serviceInstanceID: "00000000-0000-4000-8000-000000000001",
      }));
      const nativeFetch = window.fetch.bind(window);
      window.fetch = (input, init) => {
        const requestURL = typeof input === "string" ? input : input instanceof Request ? input.url : input.href;
        const path = new URL(requestURL, location.href).pathname;
        if (path === "/api/me/main-conversation" || path === "/api/conversations" || path.startsWith("/api/conversations/")) {
          return Promise.reject(new TypeError("Failed to fetch"));
        }
        return nativeFetch(input, init);
      };
    });
    await page.goto("/");
    await page.getByLabel("用户名").fill("demo");
    await page.getByLabel("密码").fill("demo");
    await page.getByRole("button", { name: "登录" }).click();
    await page.goto("/assistant");

    const sidebar = page.getByRole("complementary", { name: "对话" });
    await expect(sidebar.getByRole("button", { name: /^主会话/ })).toBeVisible();
    await expect(sidebar.getByText("旁聊历史暂不可用")).toBeVisible();
    await expect(sidebar.getByRole("button", { name: "新建旁聊" })).toBeEnabled();
    await expect(sidebar.locator(".conversation-list .conversation-item")).toHaveCount(0);
  });

  test("accepts consecutive messages and restores independent Task workchains", async ({ page }) => {
    await page.goto("/");
    await page.getByLabel("用户名").fill("demo");
    await page.getByLabel("密码").fill("demo");
    await page.getByRole("button", { name: "登录" }).click();
    await page.goto("/assistant");
    await expect(page.getByRole("link", { name: "任务与计划", exact: true })).toHaveCount(0);

    await page.getByRole("button", { name: "新建旁聊" }).click();
    await page.getByRole("combobox", { name: "选择目标电脑" }).selectOption("");
    const firstPrompt = `普通文字问题一 ${Date.now()}`;
    const secondPrompt = `普通文字问题二 ${Date.now()}`;
    const sendButton = page.getByRole("button", { name: "发送", exact: true });
    await page.getByRole("textbox", { name: "消息内容" }).fill(firstPrompt);
    await expect(sendButton).toBeEnabled();
    await sendButton.click();
    await expect(page.locator(".chat-message-user p").filter({ hasText: firstPrompt })).toBeVisible();
    await expect(page.locator(".chat-message-user").filter({ hasText: "已受理，等待处理" })).toContainText(firstPrompt);

    await page.getByRole("textbox", { name: "消息内容" }).fill(secondPrompt);
    await expect(sendButton).toBeEnabled();
    await sendButton.click();
    await expect(page.locator(".chat-message-user p").filter({ hasText: secondPrompt })).toBeVisible();
    await expect(page.locator(".chat-message-user").filter({ hasText: "已受理，等待处理" })).toHaveCount(2);
    await expect(page.getByRole("button", { name: "打开侧边栏" })).toHaveAttribute("aria-expanded", "false");
    await expect(page.locator(".task-activity")).toHaveCount(0);

    const taskIDs = await page.evaluate(({ firstPrompt, secondPrompt }) => {
      const storageKey = "cloudhost:portal-mock-assistant-state-v3";
      const stored = localStorage.getItem(storageKey);
      if (!stored) throw new Error("本地演示状态缺失");
      const state = JSON.parse(stored) as {
        conversationMessages: Array<Record<string, unknown>>;
        tasks?: Array<Record<string, unknown>>;
      };
      const first = state.conversationMessages.find((message) => message.content === firstPrompt);
      const second = state.conversationMessages.find((message) => message.content === secondPrompt);
      if (!first || !second || first.conversation_id !== second.conversation_id) throw new Error("演示消息不在同一对话");
      const conversationID = String(first.conversation_id);
      const timestamp = new Date().toISOString();
      const firstTaskID = "qdemo001";
      const secondTaskID = "qdemo002";
      const firstReplyID = "mdemo001";
      const secondReplyID = "mdemo002";
      const firstReplyText = "第一项任务已完成首轮建议。";
      const secondReplyText = "第二项任务正在读取网页。";
      Object.assign(first, { sequence: 1, task_id: firstTaskID, routing_status: "routed", delivery_status: "applied" });
      Object.assign(second, { sequence: 3, task_id: secondTaskID, routing_status: "routed", delivery_status: "applied" });
      const firstReply = { id: firstReplyID, conversation_id: conversationID, sequence: 2, role: "assistant", content: firstReplyText,
        task_id: firstTaskID, delivery_status: "applied", output_status: "completed", output_task_revision: 1, created_at: timestamp };
      const secondReply = { id: secondReplyID, conversation_id: conversationID, sequence: 4, role: "assistant", content: secondReplyText,
        task_id: secondTaskID, delivery_status: "applied", output_status: "streaming", output_task_revision: 1, created_at: timestamp };
      const makeTask = (id: string, objective: string, status: "waiting_user" | "running", stepID: string, stepTitle: string,
        replyID: string, sequence: number, replyText: string, replyStatus: "completed" | "streaming") => ({
        id, owner_id: "u1a2b3c4", initiator_id: "u1a2b3c4", execution_location: "server", conversation_id: conversationID,
        model_id: "pmodel01", objective, status, current_step: stepID, event_sequence: 5,
        current_revision: 1, desired_revision: 1,
        budget: { active_seconds: 1800, tool_calls: 100, model_tokens: 50000 },
        usage: { active_seconds: 0, tool_calls: 0, model_tokens: null }, created_at: timestamp, updated_at: timestamp,
        result: null, waiting_reason: status === "waiting_user" ? "approval" : null,
        steps: [{ id: stepID, sequence: 1, kind: "tool", status: status === "waiting_user" ? "waiting_user" : "running",
          title: stepTitle, summary: "", created_at: timestamp }],
        batch_items: [], artifacts: [],
        pending_request: status === "waiting_user" ? { id: "kdem0001", version: 1, kind: "approval", title: "写入文件",
          object: "报告.md", content: "准备创建报告。", impact: "会新增一个文件。" } : null,
        clarification_request: null, stop_request: null,
        assistant_message: { id: replyID, conversation_id: conversationID, sequence, task_id: id, task_revision: 1,
          role: "assistant", content: replyText, status: replyStatus, created_at: timestamp },
        finished_at: null,
      });
      const firstTask = makeTask(firstTaskID, firstPrompt, "waiting_user", "sdemo001",
        "Run cloud dsh MCP tool: morde_workspace_write", firstReplyID, 2, firstReplyText, "completed");
      const secondTask = makeTask(secondTaskID, secondPrompt, "running", "sdemo002",
        "Run cloud dsh MCP tool: morde_web_read", secondReplyID, 4, secondReplyText, "streaming");
      state.conversationMessages = [
        ...state.conversationMessages.filter((message) => message.conversation_id !== conversationID),
        first, firstReply, second, secondReply,
      ];
      state.tasks = [
        ...(state.tasks ?? []).filter((task) => task.conversation_id !== conversationID), firstTask, secondTask,
      ];
      localStorage.setItem(storageKey, JSON.stringify(state));
      return [firstTaskID, secondTaskID] as const;
    }, { firstPrompt, secondPrompt });

    await page.reload();
    await page.locator(".conversation-group-details > summary").click();
    await page.getByRole("button", { name: /新对话 本机最近查看/ }).click();
    await expect(page.locator(".chat-message-user p").filter({ hasText: firstPrompt })).toBeVisible();
    await expect(page.locator(".chat-message-user p").filter({ hasText: secondPrompt })).toBeVisible();
    const assistantReply = page.locator(".chat-message-assistant").filter({ hasText: "第一项任务已完成首轮建议。" });
    const replyTime = assistantReply.locator("..").locator(".assistant-reply-time");
    await expect(replyTime).toHaveCSS("opacity", "0");
    await assistantReply.hover();
    await expect(replyTime).toHaveCSS("opacity", "1");
    await expect(replyTime).toHaveAttribute("datetime", /.+/);
    await page.getByRole("button", { name: "打开侧边栏" }).click();
    const firstCard = page.locator(`.task-activity-card[data-task-id="${taskIDs[0]}"]`);
    const secondCard = page.locator(`.task-activity-card[data-task-id="${taskIDs[1]}"]`);
    await firstCard.getByRole("button", { name: /查看并处理/ }).click();
    const firstPanel = page.locator(`.task-panel-detail .task-activity[data-task-id="${taskIDs[0]}"]`);
    await expect(firstPanel).toContainText("写入文件");
    await expect(firstPanel.getByRole("region", { name: "需要批准的操作" })).toContainText("准备创建报告。");
    await expect(page.getByText("第一项任务已完成首轮建议。")).toBeVisible();
    await expect(page.getByText("第二项任务正在读取网页。")).toBeVisible();
    await expect(page.locator(".message-list article").first()).toHaveClass(/chat-message-user/);
    await expect(page.locator(".message-list .task-activity")).toHaveCount(0);

    await expect(firstPanel.getByRole("button", { name: "暂停任务" })).toBeVisible();
    await firstPanel.getByRole("button", { name: "停止任务" }).click();
    await expect(firstPanel).toContainText("正在等待服务端确认停止结果。");
    await page.getByRole("button", { name: "‹ 返回任务列表" }).click();
    await secondCard.getByRole("button", { name: /查看详情/ }).click();
    const secondPanel = page.locator(`.task-panel-detail .task-activity[data-task-id="${taskIDs[1]}"]`);
    await expect(secondPanel).toContainText("读取网页");
    await expect(secondPanel.getByRole("button", { name: "暂停任务" })).toBeVisible();
    await expect(page.locator(".assistant-composer").getByRole("button", { name: "停止任务" })).toHaveCount(0);
    await expect(page.locator(".assistant-composer").getByRole("button", { name: "暂停任务" })).toHaveCount(0);
    await expect(page.getByRole("button", { name: "设为主会话" })).toHaveCount(0);
    await expect(page.locator(".assistant-composer .assistant-progress")).toHaveCount(0);
    await expect(page.locator(".assistant-composer")).not.toContainText("AI 正在处理…");
  });

  test("keeps paginated task and file lists isolated between the main conversation and a side chat", async ({ page }) => {
    await page.setViewportSize({ width: 1024, height: 768 });
    await page.goto("/");
    await page.getByLabel("用户名").fill("demo");
    await page.getByLabel("密码").fill("demo");
    await page.getByRole("button", { name: "登录" }).click();
    await page.goto("/assistant");
    await expect(page.getByRole("button", { name: /主会话/ })).toBeVisible();
    await page.getByRole("button", { name: "新建旁聊" }).click();
    await expect(page.getByRole("heading", { name: "新旁聊" })).toBeVisible();
    await page.getByRole("combobox", { name: "选择目标电脑" }).selectOption("");
    const prompt = `会话文件分页隔离 ${Date.now()}`;
    await page.getByRole("textbox", { name: "消息内容" }).fill(prompt);
    await page.getByRole("button", { name: "发送", exact: true }).click();
    await expect(page.locator(".chat-message-user").filter({ hasText: prompt })).toContainText("已受理，等待处理");

    const seeded = await page.evaluate((prompt) => {
      const storageKey = "cloudhost:portal-mock-assistant-state-v3";
      const stored = localStorage.getItem(storageKey);
      if (!stored) throw new Error("本地演示状态缺失");
      const state = JSON.parse(stored) as {
        conversations: Array<Record<string, unknown>>;
        conversationMessages: Array<Record<string, unknown>>;
        mainConversations: Array<[string, { conversation_id?: string | null }]>;
        tasks?: Array<Record<string, unknown>>;
      };
      const mainID = state.mainConversations.find(([userID]) => userID === "u1a2b3c4")?.[1].conversation_id
        ?? state.conversations.find((item) => item.is_main === true)?.id;
      const sideMessage = state.conversationMessages.find((item) => item.content === prompt);
      const sideID = sideMessage?.conversation_id;
      const side = state.conversations.find((item) => item.id === sideID);
      if (typeof mainID !== "string" || typeof sideID !== "string" || !side || mainID === sideID) throw new Error("主会话或独立旁聊创建失败");
      side.title = "分页隔离旁聊";
      const timestamp = new Date().toISOString();
      const makeTask = (id: string, conversationID: string, objective: string, artifactLabel: string) => ({
        id, owner_id: "u1a2b3c4", initiator_id: "u1a2b3c4", execution_location: "server", conversation_id: conversationID,
        model_id: "pmodel01", objective, status: "running", current_step: `s${id}`, event_sequence: 1,
        current_revision: 1, desired_revision: 1,
        budget: { active_seconds: 1800, tool_calls: 100, model_tokens: 50000 },
        usage: { active_seconds: 0, tool_calls: 0, model_tokens: null }, created_at: timestamp, updated_at: timestamp,
        result: null, waiting_reason: null,
        steps: [{ id: `s${id}`, sequence: 1, kind: "tool", status: "running", title: "读取工作区", summary: "", created_at: timestamp }],
        batch_items: [],
        artifacts: [{ id: `f${id}`, step_id: `s${id}`, label: artifactLabel, path: "reports/summary.txt", media_type: "text/plain",
          workspace_id: null, workspace_revision: null, size_bytes: 12, sha256: null, verified: true,
          batch_item_id: null, batch_attempt: 1, created_at: timestamp }],
        pending_request: null, clarification_request: null, stop_request: null,
        assistant_message: { id: `m${id}`, conversation_id: conversationID, sequence: 2, task_id: id, task_revision: 1,
          role: "assistant", content: `${objective}进度已保存。`, status: "streaming", created_at: timestamp }, finished_at: null,
      });
      const mainTaskID = "qmain0001";
      const sideTaskIDs = Array.from({ length: 10 }, (_, index) => `qside${String(index + 1).padStart(4, "0")}`);
      const makeMessage = (conversationID: string, id: string, content: string, attachmentIDs: string[], taskID: string) => ({
        id, conversation_id: conversationID, sequence: 1, role: "user", content, task_id: taskID,
        attachment_ids: attachmentIDs, routing_status: "accepted", delivery_status: "applied", created_at: timestamp,
      });
      const mainAttachmentIDs = ["amain001"];
      const sideAttachmentIDs = Array.from({ length: 6 }, (_, index) => `aside${String(index + 1).padStart(3, "0")}`);
      const replaced = new Set([mainID, sideID]);
      state.conversationMessages = [
        ...state.conversationMessages.filter((item) => !replaced.has(String(item.conversation_id))),
        makeMessage(String(mainID), "mmain001", "主会话文件", mainAttachmentIDs, mainTaskID),
        makeMessage(sideID, "mside001", "旁聊文件", sideAttachmentIDs, sideTaskIDs[0]!),
      ];
      state.tasks = [
        ...(state.tasks ?? []).filter((item) => !replaced.has(String(item.conversation_id))),
        makeTask(mainTaskID, String(mainID), "主会话任务", "主会话成果"),
        ...sideTaskIDs.map((id, index) => makeTask(id, sideID, `旁聊任务 ${index + 1}`, `旁聊成果 ${index + 1}`)),
      ];
      localStorage.setItem(storageKey, JSON.stringify(state));
      return { mainID, sideID, sideTitle: String(side.title) };
    }, prompt);

    await page.reload();
    const conversationSidebar = page.getByRole("complementary", { name: "对话" });
    await conversationSidebar.getByText("旁聊", { exact: true }).click();
    await conversationSidebar.locator(".conversation-item").filter({ hasText: seeded.sideTitle }).click();
    await expect(page.getByRole("heading", { name: seeded.sideTitle })).toBeVisible();
    await page.getByRole("button", { name: "打开侧边栏" }).click();
    const sidePanel = page.locator("#assistant-task-panel");
    await sidePanel.getByRole("button", { name: "文件" }).click();
    const files = sidePanel.locator(".task-panel-files");
    await expect(files.getByRole("link", { name: "下载消息附件 1" })).toHaveAttribute("href", "/api/personal-data/attachments/aside001");
    await expect(files.getByRole("link", { name: "下载消息附件 1" })).toBeVisible();
    await expect(files.getByText("旁聊成果 10")).toBeAttached();
    const lastSideArtifact = files.getByText("旁聊成果 10");
    for (let pageIndex = 0; pageIndex < 10 && !(await lastSideArtifact.isVisible()); pageIndex += 1) {
      const nextPage = files.getByRole("button", { name: "下一页" });
      await expect(nextPage).toBeEnabled();
      await nextPage.click();
    }
    await expect(lastSideArtifact).toBeVisible();
    await expect(files.getByRole("link", { name: "下载消息附件 1" })).toHaveCount(0);

    await sidePanel.getByRole("button", { name: "任务", exact: true }).click();
    await expect(sidePanel.getByText("旁聊任务 10", { exact: true })).toBeVisible();
    await conversationSidebar.getByRole("button", { name: "主会话" }).click();
    await expect(page.getByRole("heading", { name: "主会话" })).toBeVisible();
    await expect(sidePanel.getByText("主会话任务", { exact: true })).toBeVisible();
    await expect(sidePanel.locator(".task-activity-card:visible").filter({ hasText: /旁聊任务/ })).toHaveCount(0);
    await sidePanel.getByRole("button", { name: "文件", exact: true }).click();
    await expect(sidePanel.getByRole("link", { name: "下载消息附件 1" })).toHaveAttribute("href", "/api/personal-data/attachments/amain001");
    await expect(sidePanel.getByText("主会话成果", { exact: true })).toBeVisible();
    await expect(sidePanel.locator(".task-file-card:visible").filter({ hasText: /旁聊成果/ })).toHaveCount(0);
  });

  test("keeps the conversation usable and allows retry when cloud computer authorization fails", async ({ page }) => {
    await page.goto("/");
    await page.getByLabel("用户名").fill("demo");
    await page.getByLabel("密码").fill("demo");
    await page.getByRole("button", { name: "登录" }).click();
    await page.goto("/assistant");
    await expect(page.getByRole("button", { name: /主会话/ })).toBeVisible();

    await page.evaluate(() => {
      const storageKey = "cloudhost:portal-mock-assistant-state-v3";
      const state = JSON.parse(localStorage.getItem(storageKey) ?? "null") as { hosts?: Array<Record<string, unknown>> } | null;
      const host = state?.hosts?.[0];
      if (!state || !host) throw new Error("本地演示电脑不存在");
      Object.assign(host, { state: "running", state_reason: "", control_state: "ai", url: "https://h1234567.hosts.test" });
      localStorage.setItem(storageKey, JSON.stringify(state));
    });
    await page.reload();
    await expect(page.getByRole("button", { name: /主会话/ })).toBeVisible();
    await page.getByRole("button", { name: "打开侧边栏" }).click();
    const sidebar = page.getByRole("complementary", { name: "当前会话侧边栏" });
    await sidebar.getByRole("button", { name: "云端电脑" }).click();
    const connect = sidebar.getByRole("button", { name: "连接到此会话" });
    await expect(connect).toBeEnabled();
    await connect.click();
    const alert = page.getByRole("alert").filter({ hasText: "本地演示模式无法连接真实云端电脑" });
    await expect(alert).toBeVisible();
    await expect(page.getByRole("textbox", { name: "消息内容" })).toBeVisible();
    await expect(page).toHaveURL(/\/assistant$/);

    await expect(connect).toBeEnabled();
    await alert.getByRole("button", { name: "关闭错误提示" }).click();
    await connect.click();
    await expect(alert).toBeVisible();
  });

  test("accepts a long multilingual message through resumable upload", async ({ page }) => {
    await page.goto("/");
    await page.getByLabel("用户名").fill("demo");
    await page.getByLabel("密码").fill("demo");
    await page.getByRole("button", { name: "登录" }).click();
    await page.goto("/assistant");
    await page.getByRole("button", { name: "新建旁聊" }).click();
    await page.getByRole("combobox", { name: "选择目标电脑" }).selectOption("");

    const prompt = "长消息中文🙂".repeat(4_500);
    expect(new TextEncoder().encode(prompt).length).toBeGreaterThan(32 * 1024);
    await page.getByRole("textbox", { name: "消息内容" }).fill(prompt);
    await page.getByRole("button", { name: "发送", exact: true }).click();
    const message = page.locator(".chat-message-user").filter({ hasText: prompt.slice(0, 24) }).last();
    await expect(message).toContainText("已受理，等待处理");
    await expect(page.getByRole("textbox", { name: "消息内容" })).toHaveValue("");
    const saved = await page.evaluate((prefix) => {
      const state = JSON.parse(localStorage.getItem("cloudhost:portal-mock-assistant-state-v3") ?? "null") as {
        conversationMessages?: Array<{ content: string; client_message_id?: string }>;
      } | null;
      const matches = (state?.conversationMessages ?? []).filter((item) => item.content.startsWith(prefix));
      return { count: matches.length, contentLength: matches[0]?.content.length, clientMessageID: matches[0]?.client_message_id };
    }, prompt.slice(0, 24));
    expect(saved).toMatchObject({ count: 1, contentLength: prompt.length });
    expect(saved.clientMessageID).toBeTruthy();
  });

  test("keeps chat controls usable at desktop and compact widths", async ({ page }) => {
    await page.goto("/");
    await page.getByLabel("用户名").fill("demo");
    await page.getByLabel("密码").fill("demo");
    await page.getByRole("button", { name: "登录" }).click();
    await page.goto("/assistant");

    for (const scenario of [{ width: 1280, height: 720 }, { width: 640, height: 360 }]) {
      await page.setViewportSize({ width: scenario.width, height: scenario.height });
      const dimensions = await page.evaluate(() => ({ width: document.documentElement.clientWidth, scrollWidth: document.documentElement.scrollWidth }));
      expect(dimensions.scrollWidth).toBeLessThanOrEqual(dimensions.width);
      await expect(page.getByRole("textbox", { name: "消息内容" })).toBeVisible();
      await expect(page.getByRole("button", { name: "发送", exact: true })).toBeVisible();
      const layout = page.locator(".assistant-layout");
      const beforeHeight = await layout.evaluate((element) => element.getBoundingClientRect().height);
      const composer = page.locator(".assistant-composer");
      const beforeBottom = await composer.evaluate((element) => element.getBoundingClientRect().bottom);
      await page.getByRole("textbox", { name: "消息内容" }).fill("测试输入不会拉长页面。".repeat(400));
      const afterHeight = await layout.evaluate((element) => element.getBoundingClientRect().height);
      const inputHeight = await page.getByRole("textbox", { name: "消息内容" }).evaluate((element) => element.getBoundingClientRect().height);
      const afterBottom = await composer.evaluate((element) => element.getBoundingClientRect().bottom);
      expect(afterHeight).toBe(beforeHeight);
      expect(inputHeight).toBeLessThanOrEqual(120);
      expect(afterBottom).toBe(beforeBottom);
      await page.getByRole("button", { name: "打开侧边栏" }).click();
      await expect(page.getByRole("button", { name: "关闭侧边栏" })).toBeVisible();
      await page.getByRole("button", { name: "关闭侧边栏" }).click();
      const sendIsUsable = await page.getByRole("button", { name: "发送", exact: true }).evaluate((button) => {
        const rect = button.getBoundingClientRect();
        const target = document.elementFromPoint(rect.left + rect.width / 2, rect.top + rect.height / 2);
        return rect.top >= 0 && rect.bottom <= window.innerHeight && (target === button || button.contains(target));
      });
      expect(sendIsUsable).toBeTruthy();
    }
  });

  test("paginates a long notification list with enlarged text inside the viewport", async ({ page }) => {
    await page.addInitScript(() => {
      const timestamp = Date.now();
      const notifications = Array.from({ length: 32 }, (_, index) => ({
        id: `nlarge${String(index + 1).padStart(3, "0")}`, kind: index % 2 ? "failed" : "completed",
        task_id: `qlarge${index + 1}`, event_sequence: index + 1,
        summary: `放大文字分页验收第 ${index + 1} 条：这是一段较长的通知说明，用于检查条目高度变化、分页重算和可读性。`,
        redacted: false, actionable: false, created_at: new Date(timestamp - index * 1000).toISOString(), read_at: null,
      }));
      localStorage.setItem("cloudhost:portal-mock-assistant-state-v3", JSON.stringify({
        hosts: [], workspaces: [], workspaceOwners: [], userPreferences: [], notifications,
        notificationOwners: notifications.map((item) => [item.id, "u1a2b3c4"]),
      }));
    });
    await page.goto("/");
    await page.getByLabel("用户名").fill("demo");
    await page.getByLabel("密码").fill("demo");
    await page.getByRole("button", { name: "登录" }).click();
    await page.setViewportSize({ width: 1280, height: 720 });
    await page.goto("/notifications");
    await page.addStyleTag({ content: ".notification-card, .notification-card * { font-size: 24px !important; line-height: 1.35 !important; }" });

    const rows = page.locator(".screen-pagination__item");
    await expect(rows).toHaveCount(32);
    await expect(rows.first()).toBeVisible();
    expect(await page.locator(".notification-card p").first().evaluate((element) => parseFloat(getComputedStyle(element).fontSize))).toBe(24);
    const pagination = page.getByRole("navigation", { name: "通知列表分页" });
    await expect(pagination).toBeVisible();
    await expect(pagination.locator(".screen-pagination__status")).toHaveText(/^1 \/ \d+$/);

    const dimensions = await page.evaluate(() => ({
      width: document.documentElement.clientWidth,
      height: document.documentElement.clientHeight,
      scrollWidth: document.documentElement.scrollWidth,
      scrollHeight: document.documentElement.scrollHeight,
    }));
    expect(dimensions.scrollWidth).toBeLessThanOrEqual(dimensions.width + 1);
    expect(dimensions.scrollHeight).toBeLessThanOrEqual(dimensions.height + 1);
    await page.locator(".screen-pagination__viewport").focus();
    await page.keyboard.press("PageDown");
    await expect(pagination.locator(".screen-pagination__status")).toHaveText(/^2 \/ \d+$/);
    await expect(rows.first()).toHaveAttribute("hidden", "");
    const hiddenAction = rows.first().locator("button");
    await expect(hiddenAction).toHaveCount(1);
    await expect(hiddenAction).not.toBeVisible();
  });
});
