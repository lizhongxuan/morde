const assert = require('node:assert/strict');
const path = require('node:path');
const { pathToFileURL } = require('node:url');
const { chromium } = require('../../../web/portal/node_modules/playwright');

(async () => {
  const browser = await chromium.launch({
    headless: true,
    ...(process.env.MORDE_CHROMIUM_PATH && { executablePath: process.env.MORDE_CHROMIUM_PATH }),
  });
  try {
    const page = await browser.newPage({ viewport: { width: 1200, height: 900 } });
    const errors = [];
    page.on('pageerror', (error) => errors.push(error.message));
    await page.goto(pathToFileURL(path.join(__dirname, '../Morde-PC输入框优化设计稿.html')).href);
    const field = page.locator('#message');
    const size = () => field.evaluate((element) => ({
      height: element.clientHeight,
      content: element.scrollHeight,
      top: element.scrollTop,
      overflow: getComputedStyle(element).overflowY,
      outline: getComputedStyle(element).outlineStyle,
    }));
    const outerBorder = await page.locator('#composer').evaluate((element) => getComputedStyle(element).borderColor);
    assert.equal((await size()).height, 48);
    await field.focus();
    assert.equal((await size()).outline, 'none');
    assert.equal(await page.locator('#composer').evaluate((element) => getComputedStyle(element).borderColor), outerBorder);
    assert.equal(await page.locator('.focus-status').isVisible(), true);

    await field.fill('一\n二\n三');
    assert.equal((await size()).height, 72);
    await page.locator('[data-lines="6"]').click();
    assert.equal((await size()).height, 144);
    await page.locator('[data-lines="8"]').click();
    const overflow = await size();
    assert.equal(overflow.height, 144);
    assert(overflow.content > overflow.height && overflow.overflow === 'auto' && overflow.top > 0);
    await field.press('Shift+Enter');
    assert((await size()).top > overflow.top);
    await field.fill('一');
    assert.equal((await size()).height, 48);
    await field.fill('中文测试'.repeat(160));
    assert((await size()).content > 144);
    assert.equal((await size()).height, 144);

    const feedback = page.locator('.preview-feedback');
    const previousFeedback = await feedback.innerText();
    assert.equal(await field.evaluate((element) => element.dispatchEvent(new KeyboardEvent('keydown', {
      key: 'Enter', bubbles: true, cancelable: true, isComposing: true,
    }))), true);
    assert.equal(await feedback.innerText(), previousFeedback);
    await field.press('Enter');
    assert.match(await feedback.innerText(), /没有发送/);
    assert.notEqual(await field.inputValue(), '');

    await field.fill('中'.repeat(85));
    const wideHeight = (await size()).height;
    await page.setViewportSize({ width: 390, height: 900 });
    await page.waitForFunction((height) => document.querySelector('#message').clientHeight > height, wideHeight);
    const narrowHeight = (await size()).height;
    assert(await page.evaluate(() => document.documentElement.scrollWidth <= document.documentElement.clientWidth));
    const footer = await page.locator('.composer-bar').boundingBox();
    const send = await page.locator('.send').boundingBox();
    assert(send.x + send.width <= footer.x + footer.width + 1);
    await page.setViewportSize({ width: 1200, height: 900 });
    await page.waitForFunction((height) => document.querySelector('#message').clientHeight < height, narrowHeight);
    assert.deepEqual(errors, []);
    console.log('Morde PC 输入框设计稿交互检查通过');
  } finally {
    await browser.close();
  }
})().catch((error) => {
  console.error(error);
  process.exitCode = 1;
});
