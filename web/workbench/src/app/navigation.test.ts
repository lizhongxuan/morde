import { afterEach, describe, expect, it, vi } from 'vitest';
import { returnToPreviousPage } from './navigation';

describe('returnToPreviousPage', () => {
  afterEach(() => {
    Reflect.deleteProperty(window, 'morde');
    Reflect.deleteProperty(window, 'opener');
    vi.restoreAllMocks();
    window.history.replaceState({}, '', '/');
  });

  it('uses only the restricted native Portal return bridge when the desktop shell provides it', () => {
    const returnToPortalHosts = vi.fn(() => Promise.resolve({ returned: true }));
    Object.defineProperty(window, 'morde', { configurable: true, value: { returnToPortalHosts } });
    const back = vi.spyOn(window.history, 'back').mockImplementation(() => undefined);

    returnToPreviousPage();

    expect(returnToPortalHosts).toHaveBeenCalledOnce();
    expect(back).not.toHaveBeenCalled();
  });

  it('prompts in place when the native return request is rejected', async () => {
    const returnToPortalHosts = vi.fn(() => Promise.reject(new Error('request rejected')));
    Object.defineProperty(window, 'morde', { configurable: true, value: { returnToPortalHosts } });
    const back = vi.spyOn(window.history, 'back').mockImplementation(() => undefined);
    const alert = vi.spyOn(window, 'alert').mockImplementation(() => undefined);

    returnToPreviousPage();
    await Promise.resolve();

    expect(returnToPortalHosts).toHaveBeenCalledOnce();
    expect(back).not.toHaveBeenCalled();
    expect(alert).toHaveBeenCalledWith('无法返回云端电脑，请确认登录状态后重试。');
  });

  it('prompts in place in a browser without an authorized native return bridge', () => {
    const back = vi.spyOn(window.history, 'back').mockImplementation(() => undefined);
    const close = vi.spyOn(window, 'close').mockImplementation(() => undefined);
    const alert = vi.spyOn(window, 'alert').mockImplementation(() => undefined);
    Object.defineProperty(window, 'opener', { configurable: true, value: window });

    returnToPreviousPage();

    expect(back).not.toHaveBeenCalled();
    expect(close).not.toHaveBeenCalled();
    expect(alert).toHaveBeenCalledWith('当前浏览器无法安全返回 Morde 云端电脑列表，请回到 Morde 客户端操作。');
  });
});
