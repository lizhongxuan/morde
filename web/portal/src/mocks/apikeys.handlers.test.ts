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
  it('creates an account-permission key without grants and preserves revoked records', async () => {
    const unauthorized = await api('/api/apikeys', json('POST', { name: 'no session' }));
    expect(unauthorized.status).toBe(401);

    await api('/api/auth/login', json('POST', { name: 'demo', password: 'demo' }));
    expect((await api('/api/apikeys', json('POST', { name: 'old grants', host_ids: [], scopes: [] }))).status).toBe(400);
    expect((await api('/api/apikeys', json('POST', { name: 'invalid expiry', ttl_seconds: 0 }))).status).toBe(400);

    const created = await api('/api/apikeys', json('POST', { name: 'account key' }));
    expect(created.status).toBe(201);
    const body = await created.json();
    expect(body).toEqual({ id: expect.any(String), key: 'chk_mock-one-time-key', expires_at: expect.any(String) });

    expect((await (await api('/api/apikeys')).json()).items).toContainEqual(expect.objectContaining({ id: body.id, name: 'account key', revoked: false }));
    resetMockState({ restoreAssistantState: true });
    api = createApiClient();
    await api('/api/auth/login', json('POST', { name: 'demo', password: 'demo' }));
    expect((await (await api('/api/apikeys')).json()).items).toContainEqual(expect.objectContaining({ id: body.id, revoked: false }));
    expect((await api(`/api/apikeys/${body.id}`, { method: 'DELETE' })).status).toBe(204);
    expect((await (await api('/api/apikeys')).json()).items).toContainEqual(expect.objectContaining({ id: body.id, revoked: true }));
  });
});
