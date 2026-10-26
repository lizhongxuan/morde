import { expect, test } from "@playwright/test";
import { readFile } from "node:fs/promises";

test("mock workbench edits files, uploads, downloads, and keeps browser frame mounted", async ({
  page,
}) => {
  let vncLoads = 0;
  let hostReads = 0;
  page.on("request", (request) => {
    const path = new URL(request.url()).pathname;
    if (path === "/vnc/index.html") vncLoads += 1;
    if (path === "/__ch/api/host") hostReads += 1;
  });
  const fileListRequest = (request: { url(): string; method(): string }) =>
    new URL(request.url()).pathname === "/v1/file/list" && request.method() === "POST";
  const fileRequest = (pathname: string) => (request: { url(): string; method(): string }) =>
    new URL(request.url()).pathname === pathname && request.method() === "POST";
  await page.goto("/");
  await expect(page.getByText("本地测试主机")).toBeVisible();
  await expect(page.getByText("远程桌面已连接")).toBeVisible();
  const hostReadsAfterInitialLoad = hostReads;
  await expect(page.getByText(/^剩余 \d{2}:\d{2}:\d{2}$/)).toBeVisible();
  await expect(page.locator(".primary-tabs .tab-button")).toHaveCount(4);
  const vscodeTab = page.getByRole("button", { name: "VS Code", exact: true });
  await expect(vscodeTab).toBeDisabled();
  await expect(page.getByRole("button", { name: /复制 Agent 接入信息/ })).toHaveCount(0);

  const renewRequestPromise = page.waitForRequest(fileRequest("/__ch/api/renew"));
  await page.getByRole("button", { name: "续期", exact: true }).click();
  const renewRequest = await renewRequestPromise;
  expect(new URL(renewRequest.url()).pathname).toBe("/__ch/api/renew");
  expect(renewRequest.method()).toBe("POST");
  await expect(page.getByText("主机已续期", { exact: true })).toBeVisible();

  const browserFrame = page.getByTitle("远程桌面");
  await expect(page.getByText("远程桌面已连接")).toBeVisible();
  await expect(browserFrame).toBeAttached();
  expect(await browserFrame.getAttribute("src")).toBe("/vnc/index.html?autoconnect=true&reconnect=true&resize=scale&view_only=true");
  await browserFrame.evaluate((frame) => frame.contentDocument!.documentElement.classList.remove("noVNC_connected"));
  await expect(page.getByText("远程桌面已断开")).toBeVisible();
  await page.getByRole("button", { name: "重新连接", exact: true }).click();
  await expect(page.getByText("远程桌面已连接")).toBeVisible();
  await expect(page.getByText("本地测试主机")).toBeVisible();
  expect(hostReads).toBe(hostReadsAfterInitialLoad);
  await browserFrame.evaluate((frame) => { frame.dataset.preserved = "true"; });
  expect(vncLoads).toBe(2);

  await page.getByRole("button", { name: "文件", exact: true }).focus();
  const initialListRequestPromise = page.waitForRequest(fileListRequest);
  await page.keyboard.press("Enter");
  await expect(page.getByRole("button", { name: "README.md" })).toBeVisible();
  expect(await (await initialListRequestPromise).postDataJSON()).toEqual({
    path: "/home/gem/workspace",
    recursive: false,
    show_hidden: false,
    include_size: true,
  });

  const readRequestPromise = page.waitForRequest(fileRequest("/v1/file/read"));
  await page.getByRole("button", { name: "README.md" }).focus();
  await page.keyboard.press("Enter");
  expect(await (await readRequestPromise).postDataJSON()).toEqual({
    file: "/home/gem/workspace/README.md",
  });
  const editor = page.getByLabel("文件内容");
  await editor.fill("# Playwright 文件编辑");
  await page.getByRole("button", { name: "保存" }).focus();
  const writeRequestPromise = page.waitForRequest(fileRequest("/v1/file/write"));
  await page.keyboard.press("Enter");
  expect(await (await writeRequestPromise).postDataJSON()).toEqual({
    file: "/home/gem/workspace/README.md",
    content: "# Playwright 文件编辑",
  });
  await expect(page.getByText("文件已保存", { exact: true })).toBeVisible();

  const hiddenListRequestPromise = page.waitForRequest(fileListRequest);
  await page.getByLabel("显示隐藏文件").check();
  expect(await (await hiddenListRequestPromise).postDataJSON()).toEqual({
    path: "/home/gem/workspace",
    recursive: false,
    show_hidden: true,
    include_size: true,
  });

  await page.getByLabel("新建目录").fill("playwright-folder");
  await page.getByRole("button", { name: "创建", exact: true }).click();
  const folderRow = page.getByRole("row").filter({ hasText: "playwright-folder" });
  await expect(folderRow).toBeVisible();
  const childListRequestPromise = page.waitForRequest(fileListRequest);
  await folderRow.getByRole("button", { name: "打开" }).click();
  expect(await (await childListRequestPromise).postDataJSON()).toEqual({
    path: "/home/gem/workspace/playwright-folder",
    recursive: false,
    show_hidden: true,
    include_size: true,
  });
  const parentListRequestPromise = page.waitForRequest(fileListRequest);
  await page.getByRole("button", { name: "上级目录" }).click();
  expect(await (await parentListRequestPromise).postDataJSON()).toEqual({
    path: "/home/gem/workspace",
    recursive: false,
    show_hidden: true,
    include_size: true,
  });
  const folder = page.getByRole("button", { name: /playwright-folder/ });
  await folder.click();
  await page.getByLabel("重命名所选项").fill("playwright-renamed");
  await page.getByRole("button", { name: "重命名" }).click();
  await expect(page.getByRole("button", { name: /playwright-renamed/ })).toBeVisible();

  const uploadName = "playwright-upload.txt";
  await page.locator("#file-upload").setInputFiles({
    name: uploadName,
    mimeType: "text/plain",
    buffer: Buffer.from("upload verified"),
  });
  await expect(page.getByText(uploadName + " 上传完成")).toBeVisible();
  await expect(page.getByRole("button", { name: uploadName })).toBeVisible();

  const downloadButton = page
    .getByRole("row")
    .filter({ hasText: uploadName })
    .getByRole("button", { name: "下载" });
  const downloadResponsePromise = page.waitForResponse((response) =>
    new URL(response.url()).pathname === "/v1/file/download" &&
    response.request().method() === "GET",
  );
  const downloadPromise = page.waitForEvent("download");
  await downloadButton.click();
  const [downloadResponse, download] = await Promise.all([
    downloadResponsePromise,
    downloadPromise,
  ]);
  expect(downloadResponse.status()).toBe(200);
  expect(new URL(downloadResponse.url()).searchParams.get("path")).toBe(
    "/home/gem/workspace/playwright-upload.txt",
  );
  expect(download.suggestedFilename()).toBe(uploadName);
  expect(await readFile((await download.path())!, "utf8")).toBe("upload verified");

  await expect(browserFrame).toBeHidden();
  const terminalRequest = page.waitForRequest((request) =>
    new URL(request.url()).pathname === "/__ch/api/terminal-session" && request.method() === "PUT",
  );
  await page.getByRole("button", { name: "终端", exact: true }).click();
  await expect(page.getByText("请先接管电脑后使用终端。", { exact: true })).toBeVisible();
  await expect(browserFrame).toHaveAttribute("data-preserved", "true");
  expect(vncLoads).toBe(2);

  const takeoverRequest = page.waitForRequest((request) =>
    new URL(request.url()).pathname === "/__ch/api/control/actions" && request.method() === "POST",
  );
  await page.getByRole("button", { name: "接管电脑" }).click();
  await takeoverRequest;
  await page.getByRole("button", { name: "重试接管" }).click();
  await expect(vscodeTab).toBeEnabled({ timeout: 5000 });
  await terminalRequest;
  await expect(page.getByTitle("主机终端")).toBeVisible();
  const terminal = page.frameLocator('iframe[title="主机终端"]');
  const command = terminal.getByLabel("命令");
  await command.fill("echo local-terminal-ok");
  await command.press("Enter");
  await expect(terminal.getByRole("log")).toContainText("local-terminal-ok");
  const replaceRequestPromise = page.waitForRequest((request) =>
    new URL(request.url()).pathname === "/__ch/api/terminal-session" && request.method() === "POST",
  );
  page.once("dialog", (dialog) => {
    expect(dialog.message()).toContain("原终端中的命令");
    void dialog.accept();
  });
  await page.getByRole("button", { name: "新建终端" }).click();
  const replaceRequest = await replaceRequestPromise;
  expect(replaceRequest.postDataJSON()).toEqual({ confirm_new: true });
  await expect(page.getByTitle("主机终端")).toHaveAttribute("src", "/terminal?session_id=mock-terminal-2");
  const terminalURL = await page.getByTitle("主机终端").getAttribute("src");
  await page.getByRole("button", { name: "桌面", exact: true }).click();
  await expect(page.getByTitle("主机终端")).toHaveCount(0);
  await page.getByRole("button", { name: "终端", exact: true }).click();
  await expect(page.getByTitle("主机终端")).toBeVisible();
  expect(await page.getByTitle("主机终端").getAttribute("src")).toBe(terminalURL);
  await expect(browserFrame).toHaveAttribute("src", "/vnc/index.html?autoconnect=true&reconnect=true&resize=scale&view_only=false");
  await vscodeTab.click();
  const vscodeFrame = page.getByTitle("VSCode");
  await expect(vscodeFrame).toBeVisible();
  await expect(vscodeFrame).toHaveAttribute("src", "/code-server/");
  await expect.poll(() => vscodeFrame.evaluate((frame) => frame.contentDocument?.title)).toBe("本地 VSCode 模拟");

  await page.getByRole("button", { name: "交回 AI" }).click();
  await expect(vscodeTab).toBeDisabled({ timeout: 5000 });
  await expect(vscodeFrame).toHaveCount(0);
  await expect(page.getByTitle("主机终端")).toHaveCount(0);
  await expect(browserFrame).toHaveAttribute("src", "/vnc/index.html?autoconnect=true&reconnect=true&resize=scale&view_only=true");

  await page.getByRole("button", { name: "桌面", exact: true }).click();
  await expect(browserFrame).toBeVisible();
  await expect(page.getByText("远程桌面已连接")).toBeVisible();
  expect(await browserFrame.getAttribute("src")).toBe("/vnc/index.html?autoconnect=true&reconnect=true&resize=scale&view_only=true");
  expect(vncLoads).toBe(4);
});

test("MVP workbench hides third-party app installation and operation", async ({ page }) => {
  let appRequests = 0;
  page.on("request", (request) => {
    if (new URL(request.url()).pathname.startsWith("/__ch/api/applications")) appRequests += 1;
  });
  await page.goto("/");

  await expect(page.getByRole("button", { name: "桌面", exact: true })).toBeVisible();
  await expect(page.getByRole("button", { name: "终端", exact: true })).toBeVisible();
  await expect(page.getByRole("button", { name: "文件", exact: true })).toBeVisible();
  await expect(page.getByRole("button", { name: "VS Code", exact: true })).toBeDisabled();
  await expect(page.getByText(/Codex|微信|QQ|飞书/)).toHaveCount(0);
  expect(appRequests).toBe(0);
});

test("does not treat an HTTP 200 noVNC page as a connected desktop", async ({ page }) => {
  await page.clock.install();
  await page.goto("/");
  await expect(page.locator('.browser-connection[data-state="connected"]')).toBeVisible();

  const disconnectedLoad = page.waitForRequest((request) =>
    new URL(request.url()).pathname === "/vnc/index.html" &&
    new URL(request.url()).searchParams.get("morde_mock_state") === "connecting",
  );
  await page.getByTitle("远程桌面").evaluate((frame) => {
    frame.src = "/vnc/index.html?autoconnect=true&resize=scale&morde_mock_state=connecting";
  });
  await disconnectedLoad;
  await expect(page.locator('.browser-connection[data-state="connecting"]')).toBeVisible();

  await page.clock.fastForward(15_000);
  await expect(page.getByRole("alert")).toContainText("没有返回连接状态");
  await expect(page.getByRole("button", { name: "重新连接" })).toBeVisible();
  await page.getByRole("button", { name: "重新连接" }).click();
  await expect(page.getByText("远程桌面已连接")).toBeVisible();
});

test("hands desktop control between AI and user, waits for confirmation, and retries failures", async ({ page }) => {
  await page.goto("/");
  const controlStatus = page.locator(".control-state");
  const browserFrame = page.getByTitle("远程桌面");
  await expect(controlStatus).toHaveText("当前控制者：AI · 桌面只读");
  await expect.poll(() => browserFrame.evaluate((frame) => frame.contentDocument!.body.dataset.viewOnly)).toBe("true");

  const firstActionPromise = page.waitForRequest((request) =>
    new URL(request.url()).pathname === "/__ch/api/control/actions" && request.method() === "POST",
  );
  await page.getByRole("button", { name: "接管电脑" }).click();
  const firstAction = await firstActionPromise;
  const firstBody = firstAction.postDataJSON();
  expect(firstBody).toMatchObject({ action: "take_over", expected_control_version: 0 });
  await expect(page.getByRole("alert")).toHaveText("运行端暂不可用，请重试");

  const retryActionPromise = page.waitForRequest((request) =>
    new URL(request.url()).pathname === "/__ch/api/control/actions" && request.method() === "POST",
  );
  await page.getByRole("button", { name: "重试接管" }).click();
  const retryAction = await retryActionPromise;
  expect(retryAction.postDataJSON()).toEqual(firstBody);
  await expect(controlStatus).toHaveText("正在接管电脑，请稍候…");
  await expect.poll(() => browserFrame.evaluate((frame) => frame.contentDocument!.body.dataset.viewOnly)).toBe("true");
  await expect(controlStatus).toHaveText("当前控制者：你", { timeout: 5000 });
  await expect.poll(() => browserFrame.evaluate((frame) => frame.contentDocument!.body.dataset.viewOnly)).toBe("false");

  const releasePromise = page.waitForRequest((request) =>
    new URL(request.url()).pathname === "/__ch/api/control/actions" && request.method() === "POST",
  );
  await page.getByRole("button", { name: "交回 AI" }).click();
  const release = await releasePromise;
  expect(release.postDataJSON()).toMatchObject({ action: "release", expected_control_version: 2 });
  await expect(controlStatus).toHaveText("正在交回 AI，请稍候…");
  await expect.poll(() => browserFrame.evaluate((frame) => frame.contentDocument!.body.dataset.viewOnly)).toBe("false");
  await expect(controlStatus).toHaveText("当前控制者：AI · 桌面只读", { timeout: 5000 });
  await expect.poll(() => browserFrame.evaluate((frame) => frame.contentDocument!.body.dataset.viewOnly)).toBe("true");
});

test("mobile keyboard and shortcut controls require desktop takeover", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto("/");
  const keyboard = page.getByRole("button", { name: "键盘", exact: true });
  const shortcut = page.getByRole("button", { name: "Alt+Tab", exact: true });
  const browserFrame = page.getByTitle("远程桌面");
  await expect(keyboard).toBeVisible();
  await expect(keyboard).toBeDisabled();
  await expect(shortcut).toBeDisabled();
  await page.getByRole("button", { name: "接管电脑" }).click();
  await expect(page.getByRole("button", { name: "重试接管" })).toBeVisible();
  await page.getByRole("button", { name: "重试接管" }).click();
  await expect(page.locator(".control-state")).toHaveText("当前控制者：你", { timeout: 5000 });
  await expect(keyboard).toBeEnabled();
  await keyboard.click();
  await expect.poll(() => browserFrame.evaluate((frame) => frame.contentDocument!.body.dataset.keyboardOpens)).toBe("1");
  await shortcut.click();
  await expect.poll(() => browserFrame.evaluate((frame) => frame.contentDocument!.body.dataset.tabPresses)).toBe("1");
  await page.getByRole("button", { name: "知道了" }).click();
  await page.getByRole("button", { name: "交回 AI" }).click();
  await expect(page.locator(".control-state")).toHaveText("当前控制者：AI · 桌面只读", { timeout: 5000 });
  await expect(keyboard).toBeDisabled();
});

test("adapts the browser canvas to viewport changes and tab switches", async ({ page }) => {
  await page.addInitScript(() => {
    Object.defineProperty(document, "fullscreenElement", {
      configurable: true,
      get: () => document.querySelector<HTMLElement>("[data-test-fullscreen]") ?? null,
    });
    HTMLElement.prototype.requestFullscreen = function () {
      this.dataset.testFullscreen = "true";
      document.dispatchEvent(new Event("fullscreenchange"));
      return Promise.resolve();
    };
    document.exitFullscreen = async () => {
      document.fullscreenElement?.removeAttribute("data-test-fullscreen");
      document.dispatchEvent(new Event("fullscreenchange"));
    };
  });
  await page.setViewportSize({ width: 1440, height: 900 });
  await page.goto("/");

  const browserFrame = page.getByTitle("远程桌面");
  await expect(browserFrame).toHaveAttribute("src", "/vnc/index.html?autoconnect=true&reconnect=true&resize=scale&view_only=true");
  await expect(browserFrame).toBeVisible();
  await expect(page.getByText("远程桌面已连接")).toBeVisible();
  await browserFrame.evaluate((frame) => { frame.dataset.instance = "connected"; });

  await browserFrame.evaluate((frame) => {
    (frame.contentDocument!.getElementById("noVNC_setting_resize") as HTMLSelectElement).value = "off";
  });
  await page.getByRole("button", { name: "适应窗口" }).click();
  await expect.poll(() => browserFrame.evaluate((frame) => frame.contentDocument!.body.dataset.resize)).toBe("scale");

  await page.getByRole("button", { name: "找回窗口" }).click();
  await expect(page.getByRole("dialog", { name: "找回被遮挡的窗口" })).toContainText("Alt");
  await expect(page.getByRole("dialog", { name: "找回被遮挡的窗口" })).toContainText("Tab");
  await expect.poll(() => browserFrame.evaluate((frame) => frame.contentDocument!.body.dataset.tabPresses)).toBe("1");
  expect(await browserFrame.evaluate((frame) => frame.contentDocument!.getElementById("noVNC_toggle_alt_button")!.classList.contains("noVNC_selected"))).toBe(false);
  await page.getByRole("button", { name: "知道了" }).click();
  await browserFrame.evaluate((frame) => frame.contentDocument!.getElementById("noVNC_toggle_alt_button")!.classList.add("noVNC_selected"));
  await page.getByRole("button", { name: "找回窗口" }).click();
  await expect.poll(() => browserFrame.evaluate((frame) => frame.contentDocument!.body.dataset.tabPresses)).toBe("2");
  expect(await browserFrame.evaluate((frame) => frame.contentDocument!.getElementById("noVNC_toggle_alt_button")!.classList.contains("noVNC_selected"))).toBe(true);
  await page.getByRole("button", { name: "知道了" }).click();

  await page.getByRole("button", { name: "全屏", exact: true }).click();
  await expect(page.getByRole("button", { name: "退出全屏" })).toBeVisible();
  await page.getByRole("button", { name: "退出全屏" }).click();
  await expect(page.getByRole("button", { name: "全屏", exact: true })).toBeVisible();

  const wideSize = await browserFrame.evaluate((frame) => {
    const { width, height } = frame.getBoundingClientRect();
    return { width, height };
  });
  await page.setViewportSize({ width: 640, height: 720 });
  const compactSize = await browserFrame.evaluate((frame) => {
    const { width, height } = frame.getBoundingClientRect();
    return { width, height };
  });
  expect(compactSize.width).toBeLessThan(wideSize.width);
  expect(compactSize.height).toBeLessThan(wideSize.height);

  for (const nextTab of ["终端", "文件", "终端"]) {
    await page.getByRole("button", { name: nextTab, exact: true }).click();
    await expect(browserFrame).toBeHidden();
    const size = await browserFrame.evaluate((frame) => {
      const { width, height } = frame.getBoundingClientRect();
      return { width, height, visibility: getComputedStyle(frame.parentElement!).visibility };
    });
    expect(size.width).toBeGreaterThan(0);
    expect(size.height).toBeGreaterThan(0);
    expect(size.visibility).toBe("hidden");
    await expect(browserFrame).toHaveAttribute("data-instance", "connected");

    await page.getByRole("button", { name: "桌面", exact: true }).click();
    await expect(browserFrame).toBeVisible();
    await expect(browserFrame).toHaveAttribute("data-instance", "connected");
  }
});
