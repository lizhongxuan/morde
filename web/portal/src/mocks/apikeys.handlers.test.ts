import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it } from 'vitest';
import { setupServer } from 'msw/node';
import { handlers, resetMockState } from './handlers';

const server = setupServer(...handlers);
function createApiClient() {
  let cookie = '';
  return async (path: string, init?: RequestInit) => {
    const headers = new Headers(init?.headers);
    if (cookie) headers.set('cookie', cookie);
    const response = await fetch(new URL(path, globalThis.location.href), { ...init, headers });
    const setCookie = response.headers.get('set-cookie');
    if (setCookie) {
      const pair = setCookie.split(';', 1)[0] ?? '';
      cookie = pair.endsWith('=') ? '' : pair;
    }
    return response;
  };
}
const json = (method: string, body: unknown): RequestInit => ({ method, headers: { 'content-type': 'application/json' }, body: JSON.stringify(body) });
let api = createApiClient();

beforeAll(() => server.listen());
beforeEach(() => { resetMockState(); api = createApiClient(); });
afterEach(() => { server.resetHandlers(); });
afterAll(() => server.close());

describe('API Key mock contract', () => {
  it('requires explicit grants and preserves revoked records for the UI', async () => {
    const unauthorized = await api('/api/apikeys', json('POST', { name: 'no session', host_ids: [], scopes: ['host:create'] }));
    expect(unauthorized.status).toBe(401);

    await api('/api/auth/login', json('POST', { name: 'demo', password: 'demo' }));
    const implicit = await api('/api/apikeys', json('POST', { name: 'implicit' }));
    expect(implicit.status).toBe(400);

    const created = await api('/api/apikeys', json('POST', { name: 'host creator', host_ids: [], scopes: ['host:create'] }));
    expect(created.status).toBe(201);
    const body = await created.json();
    expect(body).toMatchObject({ host_ids: [], scopes: ['host:create'], revoked: false });
    expect(body).toHaveProperty('key', 'chk_mock-one-time-key');

    expect((await (await api('/api/apikeys')).json()).items).toContainEqual(expect.objectContaining({ id: body.id, host_ids: [], scopes: ['host:create'], revoked: false }));
    expect((await api(`/api/apikeys/${body.id}`, { method: 'DELETE' })).status).toBe(204);
    expect((await (await api('/api/apikeys')).json()).items).toContainEqual(expect.objectContaining({ id: body.id, revoked: true }));

    const locked = await api('/api/apikeys', json('POST', { name: 'no authority', host_ids: [], scopes: [] }));
    expect(locked.status).toBe(201);
    const lockedBody = await locked.json();
    expect(lockedBody).toMatchObject({ host_ids: [], scopes: [], revoked: false });
  });
});
