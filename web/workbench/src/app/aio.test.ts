import { afterEach, describe, expect, it, vi } from 'vitest';
import { aioRequest, AioError, isTransientNetworkError } from './aio';

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

describe('transient network error classification', () => {
  it('routes fetch failures, 5xx responses, and successful-response parse failures as transient', () => {
    expect(isTransientNetworkError(new TypeError('Failed to fetch'))).toBe(true);
    expect(isTransientNetworkError(new AioError('server unavailable', 503))).toBe(true);
    expect(isTransientNetworkError(new AioError('invalid JSON', 200, false, true))).toBe(true);
    expect(isTransientNetworkError(new SyntaxError('Unexpected token'))).toBe(true);
  });

  it('keeps 4xx access/conflict errors and protocol validation errors on their existing path', () => {
    expect(isTransientNetworkError(new AioError('forbidden', 403))).toBe(false);
    expect(isTransientNetworkError(new AioError('conflict', 409))).toBe(false);
    expect(isTransientNetworkError(new AioError('invalid file metadata'))).toBe(false);
    expect(isTransientNetworkError(new Error('manifest path is invalid'))).toBe(false);
  });
});
