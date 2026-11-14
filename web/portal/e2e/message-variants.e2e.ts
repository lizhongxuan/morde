import { expect, test } from '@playwright/test';

test.describe('immutable message versions', () => {
  test.skip(process.env.CH_REAL_E2E === '1', 'variant UI flow uses local MSW routes');

  test('edits a source message as a new version and switches without changing the draft', async ({ page }) => {
    await page.goto('/');
    await page.getByLabel('用户名').fill('demo');
    await page.getByLabel('密码').fill('demo');
    await page.getByRole('button', { name: '登录' }).click();
    await page.goto('/assistant');
    await page.getByRole('button', { name: '新建旁聊' }).click();
    await page.getByRole('combobox', { name: '选择目标电脑' }).selectOption('');

    const sourceText = `原始版本 ${Date.now()}`;
    const editedText = `编辑版本 ${Date.now()}`;
    const composer = page.getByRole('textbox', { name: '消息内容' });
    await composer.fill(sourceText);
    await page.getByRole('button', { name: '发送', exact: true }).click();
    const sourceMessage = page.locator('.chat-message-user p').filter({ hasText: sourceText });
    await expect(sourceMessage).toBeVisible();
    await composer.fill('保留在输入框里的草稿');

    const sourceRow = page.locator('.chat-message-row-user').filter({ has: sourceMessage });
    await sourceRow.getByRole('button', { name: '编辑并重发' }).click();
    const edit = page.getByRole('textbox', { name: '编辑消息并创建新版本' });
    await edit.fill(editedText);
    await page.getByRole('button', { name: '创建新版本' }).click();

    await expect(page.locator('.chat-message-user p').filter({ hasText: editedText })).toBeVisible();
    await expect(page.locator('.chat-message-user p').filter({ hasText: sourceText })).toHaveCount(0);
    await expect(composer).toHaveValue('保留在输入框里的草稿');
    await page.getByRole('combobox', { name: '选择消息版本' }).selectOption('1');
    await expect(page.locator('.chat-message-user p').filter({ hasText: sourceText })).toBeVisible();
    await expect(page.locator('.chat-message-user p').filter({ hasText: editedText })).toHaveCount(0);
    await page.getByRole('combobox', { name: '选择消息版本' }).selectOption('2');
    await expect(page.locator('.chat-message-user p').filter({ hasText: editedText })).toBeVisible();
    await expect(composer).toHaveValue('保留在输入框里的草稿');
  });
});
