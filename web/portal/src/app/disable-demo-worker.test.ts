import { afterEach, describe, expect, it, vi } from 'vitest';
import { disableDemoWorker } from './disable-demo-worker';

afterEach(() => vi.unstubAllGlobals());

describe('disabling the demo service worker', () => {
  it('removes only the root demo worker and requests a reload when it controls this page', async () => {
    const workerURL = new URL('/mockServiceWorker.js', window.location.origin).href;
    const unregister = vi.fn().mockResolvedValue(true);
    const otherUnregister = vi.fn().mockResolvedValue(true);
    vi.stubGlobal('navigator', { serviceWorker: {
      controller: { scriptURL: workerURL },
      getRegistrations: async () => [
        { scope: new URL('/', window.location.origin).href, active: { scriptURL: workerURL }, unregister },
        { scope: new URL('/other/', window.location.origin).href, active: { scriptURL: workerURL }, unregister: otherUnregister },
      ],
    } });
    expect(await disableDemoWorker()).toBe(true);
    expect(unregister).toHaveBeenCalledOnce();
    expect(otherUnregister).not.toHaveBeenCalled();
  });

  it('does not remove an unrelated worker', async () => {
    const unregister = vi.fn().mockResolvedValue(true);
    vi.stubGlobal('navigator', { serviceWorker: {
      controller: { scriptURL: new URL('/other-worker.js', window.location.origin).href },
      getRegistrations: async () => [{ scope: new URL('/', window.location.origin).href,
        active: { scriptURL: new URL('/other-worker.js', window.location.origin).href }, unregister }],
    } });
    expect(await disableDemoWorker()).toBe(false);
    expect(unregister).not.toHaveBeenCalled();
  });
});
