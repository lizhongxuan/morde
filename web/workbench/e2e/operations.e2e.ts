import { expect, test } from "@playwright/test";

test("mock file management creates, renames, and confirms deleting a directory", async ({ page }) => {
  const workspace = "/home/gem/workspace";
  const created = `${workspace}/operations-created`;
  const renamed = `${workspace}/operations-renamed`;

  await page.goto("/");
  await page.getByRole("button", { name: "文件", exact: true }).focus();
  await page.keyboard.press("Enter");
  await expect(page.getByRole("button", { name: "README.md" })).toBeVisible();

  const createRequestPromise = page.waitForRequest((request) =>
    new URL(request.url()).pathname === "/v1/shell/exec" && request.method() === "POST",
  );
  const newDirectory = page.getByLabel("新建目录");
  await newDirectory.focus();
  await page.keyboard.type("operations-created");
  const createButton = page.getByRole("button", { name: "创建", exact: true });
  await createButton.focus();
  await page.keyboard.press("Enter");
  const createRequest = await createRequestPromise;
  expect(createRequest.postDataJSON()).toMatchObject({
    command: `mkdir -- '${created}'`,
    exec_dir: workspace,
  });
  await expect(page.getByRole("button", { name: /operations-created/ })).toBeVisible();

  const createdFolder = page.getByRole("button", { name: /operations-created/ });
  await createdFolder.focus();
  await page.keyboard.press("Enter");
  const renameRequestPromise = page.waitForRequest((request) =>
    new URL(request.url()).pathname === "/v1/shell/exec" && request.method() === "POST",
  );
  const renameInput = page.getByLabel("重命名所选项");
  await renameInput.focus();
  await renameInput.press("Home");
  await renameInput.press("Shift+End");
  await page.keyboard.type("operations-renamed");
  const renameButton = page.getByRole("button", { name: "重命名", exact: true });
  await renameButton.focus();
  await page.keyboard.press("Enter");
  const renameRequest = await renameRequestPromise;
  expect(renameRequest.postDataJSON()).toMatchObject({
    command: `mv -n -- '${created}' '${renamed}' && test ! -e '${created}'`,
    exec_dir: workspace,
  });
  await expect(page.getByRole("button", { name: /operations-created/ })).toHaveCount(0);
  await expect(page.getByRole("button", { name: /operations-renamed/ })).toBeVisible();

  const renamedFolder = page.getByRole("button", { name: /operations-renamed/ });
  await renamedFolder.focus();
  await page.keyboard.press("Enter");
  let deletionConfirmation = "";
  page.once("dialog", async (dialog) => {
    deletionConfirmation = dialog.message();
    await dialog.accept();
  });
  const deleteRequestPromise = page.waitForRequest((request) =>
    new URL(request.url()).pathname === "/v1/shell/exec" && request.method() === "POST",
  );
  const deleteButton = page.getByRole("button", { name: "删除", exact: true });
  await deleteButton.focus();
  await page.keyboard.press("Enter");
  expect(deletionConfirmation).toBe("确定删除“operations-renamed”吗？此操作不可撤销。");
  const deleteRequest = await deleteRequestPromise;
  expect(deleteRequest.postDataJSON()).toMatchObject({
    command: `rm -r -- '${renamed}'`,
    exec_dir: workspace,
  });
  await expect(page.getByRole("button", { name: /operations-renamed/ })).toHaveCount(0);
  await expect(page.getByRole("button", { name: "README.md" })).toBeVisible();
});
