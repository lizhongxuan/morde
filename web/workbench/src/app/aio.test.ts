import { afterEach, describe, expect, it, vi } from 'vitest';
import { aioRequest } from './aio';

afterEach(() => vi.unstubAllGlobals());

describe('aioRequest', () => {
  it('reports an invalid response with its HTTP status', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response('', { status: 502 })));

    await expect(aioRequest('/v1/file/list')).rejects.toThrow('AIO 返回了无效响应（HTTP 502）');
  });

  it.each([
    ['string success flag', { success: 'false', message: 'invalid success field' }, 'invalid success field'],
    ['missing success flag', { message: 'missing success field' }, 'missing success field'],
    ['null envelope', null, '请求失败（HTTP 200）'],
  ])('rejects an envelope with a %s', async (_name, envelope, message) => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response(JSON.stringify(envelope))));

    await expect(aioRequest('/v1/file/write')).rejects.toThrow(message);
  });
});
