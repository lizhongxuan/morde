import { expect, test } from "@playwright/test";

async function expectPersonalPageFitsViewport(page: import("@playwright/test").Page) {
  const fits = await page.evaluate(() => {
    const content = document.querySelector<HTMLElement>("#main-content");
    return content !== null
      && document.documentElement.scrollHeight <= document.documentElement.clientHeight + 1
      && content.scrollHeight <= content.clientHeight + 1;
  });
  expect(fits).toBe(true);
}

test.describe("personal data auto-save", () => {
  test.skip(process.env.CH_REAL_E2E === "1", "local MSW fixture scenario is disabled for real-host runs");

  test("serializes initial personal-data sync across two browser tabs", async ({ page }) => {
    await page.context().addInitScript(() => {
      const nativeFetch = window.fetch.bind(window);
      Object.assign(window, { e2ePersonalSnapshotCalls: 0, e2ePersonalChangeCalls: 0, e2ePersonalQuotaCalls: 0, e2eSnapshotBlocked: false });
      window.fetch = (input, init) => {
        const url = typeof input === "string" || input instanceof URL ? input : input.url;
        const method = init?.method ?? (input instanceof Request ? input.method : "GET");
        const path = new URL(url, location.href).pathname;
        if (method.toUpperCase() === "GET" && path === "/api/sync/changes") {
          const instrumented = window as Window & { e2ePersonalChangeCalls?: number };
          instrumented.e2ePersonalChangeCalls = (instrumented.e2ePersonalChangeCalls ?? 0) + 1;
        }
        if (method.toUpperCase() === "GET" && path === "/api/personal-data/quota") {
          const instrumented = window as Window & { e2ePersonalQuotaCalls?: number };
          instrumented.e2ePersonalQuotaCalls = (instrumented.e2ePersonalQuotaCalls ?? 0) + 1;
        }
        if (method.toUpperCase() === "GET" && path === "/api/sync/snapshot") {
          const instrumented = window as Window & { e2ePersonalSnapshotCalls?: number; e2eSnapshotBlocked?: boolean };
          instrumented.e2ePersonalSnapshotCalls = (instrumented.e2ePersonalSnapshotCalls ?? 0) + 1;
          if (sessionStorage.getItem("e2e-block-personal-snapshot") === "1") {
            sessionStorage.removeItem("e2e-block-personal-snapshot");
            instrumented.e2eSnapshotBlocked = true;
            return new Promise<Response>((resolve) => {
              window.addEventListener("e2e-release-personal-snapshot", () => { void nativeFetch(input, init).then(resolve); }, { once: true });
            });
          }
        }
        return nativeFetch(input, init);
      };
    });
    await page.goto("/");
    await page.getByLabel("用户名").fill("demo");
    await page.getByLabel("密码").fill("demo");
    await page.getByRole("button", { name: "登录" }).click();
    await page.evaluate(() => sessionStorage.setItem("e2e-block-personal-snapshot", "1"));
    await page.getByRole("link", { name: "个人数据", exact: true }).click();
    await expect.poll(() => page.evaluate(() => (window as Window & { e2eSnapshotBlocked?: boolean }).e2eSnapshotBlocked)).toBe(true);

    const secondTab = await page.context().newPage();
    await secondTab.goto("/personal");
    await expect(secondTab.locator(".personal-sync-status")).toBeVisible();
    await expect(secondTab.getByText("正在更新云端数据…")).toBeVisible();
    await secondTab.waitForTimeout(300);
    expect(await secondTab.evaluate(() => (window as Window & { e2ePersonalSnapshotCalls?: number }).e2ePersonalSnapshotCalls)).toBe(0);

    await page.getByRole("link", { name: "对话", exact: true }).click();
    await expect(page).toHaveURL(/\/assistant$/);
    await page.evaluate(() => window.dispatchEvent(new Event("e2e-release-personal-snapshot")));
    await expect.poll(() => page.evaluate(() => (window as Window & { e2ePersonalQuotaCalls?: number }).e2ePersonalQuotaCalls ?? 0)).toBeGreaterThan(0);
    await expect.poll(() => page.evaluate(() => (window as Window & { e2ePersonalSnapshotCalls?: number }).e2ePersonalSnapshotCalls)).toBe(1);
    await expect.poll(() => secondTab.evaluate(() => (window as Window & { e2ePersonalSnapshotCalls?: number }).e2ePersonalSnapshotCalls)).toBe(0);
    await expect(secondTab.locator(".personal-sync-status")).toContainText("保存后自动更新云端");
    await secondTab.close();
  });

  test("submits profile changes automatically and keeps the account page free of manual sync actions", async ({ page }) => {
    await page.goto("/");
    await page.getByLabel("用户名").fill("demo");
    await page.getByLabel("密码").fill("demo");
    await page.getByRole("button", { name: "登录" }).click();
    await page.goto("/personal");

    await expect(page.getByRole("region", { name: "个人资料" })).toBeVisible();
    await expect(page.getByText("保存后自动更新云端")).toBeVisible();
    await expect(page.getByRole("button", { name: /立即同步|立即更新/ })).toHaveCount(0);
    await page.getByLabel("名称").fill("自动保存验收用户");
    const saveResponse = page.waitForResponse((response) =>
      response.url().endsWith("/api/personal-data/profile/default") && response.request().method() === "PUT",
    );
    await page.getByRole("button", { name: "保存个人资料" }).click();
    expect((await saveResponse).status()).toBe(200);

    await expect(page.getByText("0 项待提交 · 0 条冲突草稿")).toBeVisible();
    const saved = await page.evaluate(async () => {
      const response = await fetch("/api/personal-data/profile/default", { credentials: "include" });
      if (!response.ok) throw new Error(`个人资料读取失败：${response.status}`);
      return response.json() as Promise<{ object_type: string; object_id: string; revision: number; body: Record<string, unknown> }>;
    });
    expect(saved).toMatchObject({
      object_type: "profile",
      object_id: "default",
      revision: 1,
      body: { name: "自动保存验收用户" },
    });
  });

  test("paginates confirmed memory records and keeps the personal-data page within one screen", async ({ page }) => {
    await page.goto("/");
    await page.getByLabel("用户名").fill("demo");
    await page.getByLabel("密码").fill("demo");
    await page.getByRole("button", { name: "登录" }).click();
    await expect(page.getByRole("banner").getByText("demo", { exact: true })).toBeVisible();
    await page.evaluate(() => {
      window.history.pushState({}, "", "/personal?section=memory");
      window.dispatchEvent(new PopStateEvent("popstate"));
    });
    await expect(page.getByRole("heading", { name: "记忆" })).toBeAttached();

    const statuses = await page.evaluate(async () => {
      const results: number[] = [];
      for (let index = 0; index < 12; index += 1) {
        const number = String(index + 1).padStart(2, "0");
        results.push(await fetch(`/api/personal-data/memory/m_${String(index + 1).padStart(8, "0")}`, {
        method: "PUT",
        credentials: "include",
        headers: { "Content-Type": "application/json", "Idempotency-Key": `memory-e2e-${number}` },
        body: JSON.stringify({ base_revision: 0, body: { content: `长记忆分页验收第 ${number} 项`, category: "本地验收" } }),
        }).then((response) => response.status));
      }
      return results;
    });
    expect(statuses.every((status) => status === 200), JSON.stringify(statuses)).toBe(true);
    await page.evaluate(() => window.dispatchEvent(new Event("online")));

    const pagination = page.getByRole("navigation", { name: "已保存记忆分页" });
    const visibleMemoryValues = () => page.locator(".personal-data-pager textarea").evaluateAll((fields) =>
      fields.filter((field) => !field.closest("[data-item-key]")?.hasAttribute("hidden"))
        .map((field) => (field as HTMLTextAreaElement).value),
    );
    const firstMemory = "长记忆分页验收第 01 项";
    const lastMemory = "长记忆分页验收第 12 项";
    await expect.poll(visibleMemoryValues).toContain(firstMemory);
    expect(await visibleMemoryValues()).not.toContain(lastMemory);
    await expectPersonalPageFitsViewport(page);

    for (let pageIndex = 0; pageIndex < 12 && !(await visibleMemoryValues()).includes(lastMemory); pageIndex += 1) {
      await pagination.getByRole("button", { name: "下一页" }).click();
    }
    expect(await visibleMemoryValues()).not.toContain(firstMemory);
    await expect.poll(visibleMemoryValues).toContain(lastMemory);
    await expectPersonalPageFitsViewport(page);
  });
});
