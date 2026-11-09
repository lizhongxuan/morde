import { test } from 'node:test';
import assert from 'node:assert/strict';
import { MordeAPI, checkedId, credentialExpired, credentialExpiresSoon, gatewayURL, mordeUserDataDirectory, portalOrigin } from '../out/morde-core.js';

const future = '2099-01-01T00:00:00Z';
const serviceInstanceID = '12345678-1234-4234-8234-123456789abc';
const capabilities = { service_instance_id: serviceInstanceID, api_version: '1', hub_version: null, event_version: null, minimum_client_version: null, supported_features: ['password_login', 'client_installations', 'ordered_client_login'], default_max_request_bytes: 1048576 };
const credentials = { origin: 'https://portal.example', serviceInstanceID, installationId: 'i1234567', clientCredential: 'client-secret', clientExpiresAt: future, syncCredential: 'sync-secret', syncExpiresAt: future };
const loginBody = { installation_id: 'i1234567', client_credential: 'client-secret', client_expires_at: future, sync_credential: 'sync-secret', sync_expires_at: future, web_session_credential: 'web-secret', web_session_expires_at: future };
const json = (value, status = 200) => new Response(JSON.stringify(value), { status, headers: { 'content-type': 'application/json' } });
const attempt = (attemptGeneration = 1, idempotencyKey = `retry-${attemptGeneration}`) => ({ clientInstanceID: '123e4567-e89b-42d3-a456-426614174000', attemptGeneration: String(attemptGeneration), idempotencyKey });
const syncDeviceID = 'AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA';
const syncServerDeviceID = 'BBBBBBB-BBBBBBB-BBBBBBB-BBBBBBB-BBBBBBB-BBBBBBB-BBBBBBB-BBBBBBB';
const syncChallenge = {
  id: 'n1234567', device_id: syncDeviceID, nonce: Buffer.alloc(32, 1).toString('base64'),
  signing_payload: Buffer.from('proof payload').toString('base64'), expires_at: '2099-01-01T00:00:00Z',
};
const syncRegistration = {
  id: 'd1234567', workspace_id: 'w1234567', client_installation_id: 'i1234567', device_id: syncDeviceID,
  folder_id: 'morde-0123456789abcdef0123456789abcdef', server_device_id: syncServerDeviceID,
  relay_address: `relay://relay.example.test:22067/?id=${syncServerDeviceID}&token=abcdefghijklmnopabcdefghijklmnop`,
  state: 'pending_node', created_at: '2026-10-31T10:00:00Z',
};

test('expiry, origins, IDs and Host ticket URLs reject malformed values', () => {
  const now = Date.parse('2026-10-26T00:00:00Z');
  assert.equal(credentialExpiresSoon(new Date(now + 7 * 86400_000).toISOString(), now), true);
  assert.equal(credentialExpiresSoon(new Date(now + 7 * 86400_000 + 1).toISOString(), now), false);
  assert.equal(credentialExpired('invalid', now), true);
  assert.equal(portalOrigin('https://portal.example/'), 'https://portal.example');
  assert.equal(mordeUserDataDirectory('/app-data'), '/app-data/Morde Client');
  assert.equal(mordeUserDataDirectory('/app-data', '/tmp/morde-isolated-test'), '/tmp/morde-isolated-test');
  assert.throws(() => mordeUserDataDirectory('/app-data', 'relative-test-profile'), /absolute path/);
  assert.equal(portalOrigin('http://localhost:8080'), 'http://localhost:8080');
  assert.equal(portalOrigin('http://portal.localhost'), 'http://portal.localhost');
  for (const value of ['http://portal.example', 'https://user:pass@portal.example', 'https://portal.example/path', 'https://portal.example?x=1']) assert.throws(() => portalOrigin(value));
  assert.equal(checkedId('host', 'h1234567'), 'h1234567');
  assert.throws(() => checkedId('host', '../admin'));
  assert.equal(gatewayURL('https://h1234567.example/__ch/auth?t=one-time'), 'https://h1234567.example/__ch/auth?t=one-time');
  assert.equal(gatewayURL('http://h1234567.hosts.localhost/__ch/auth?t=one-time', 'http://localhost'), 'http://h1234567.hosts.localhost/__ch/auth?t=one-time');
  assert.equal(gatewayURL('http://h1234567.hosts.localhost/__ch/auth?t=one-time', 'http://portal.localhost'), 'http://h1234567.hosts.localhost/__ch/auth?t=one-time');
  assert.throws(() => gatewayURL('http://h1234567.hosts.localhost/__ch/auth?t=one-time', 'https://portal.example'));
  assert.throws(() => gatewayURL('http://evil.hosts.localhost/__ch/auth?t=one-time', 'http://localhost'));
  for (const value of ['https://evil.example/__ch/auth?ticket=wrong', 'http://h1234567.example/__ch/auth?t=one-time', 'https://h1234567.example/__ch/auth?t=one&t=two', 'https://h1234567.example/__ch/auth?t=one&next=%2F%5Cevil.example']) assert.throws(() => gatewayURL(value));
});

test('account login sends one password request and receives separate Client, Sync and Web credentials', async () => {
  const originalFetch = globalThis.fetch;
  const calls = [];
  globalThis.fetch = async (url, options) => {
    calls.push({ url, options });
    return json(String(url).endsWith('/api/capabilities') ? capabilities : loginBody);
  };
  try {
    const result = await new MordeAPI(() => null).login('https://portal.example', ' user ', 'password', attempt());
    assert.equal(calls.length, 2);
    assert.equal(calls[0].url, 'https://portal.example/api/capabilities');
    assert.equal(calls[0].options.method, undefined);
    assert.equal(calls[0].options.body, undefined);
    assert.equal(calls[1].url, 'https://portal.example/api/auth/client-login');
    assert.equal(calls[1].options.headers['Idempotency-Key'], 'retry-1');
    assert.deepEqual(JSON.parse(calls[1].options.body), { name: 'user', password: 'password', platform: 'desktop', display_name: 'Morde Desktop', client_instance_id: attempt().clientInstanceID, attempt_generation: 1 });
    assert.equal(result.credentials.clientCredential, 'client-secret');
    assert.equal(result.credentials.serviceInstanceID, serviceInstanceID);
    assert.equal(result.credentials.syncCredential, 'sync-secret');
    assert.equal(result.webSession.credential, 'web-secret');
    assert.equal('workspaceIds' in result.credentials, false);
    await new MordeAPI(() => null).login('https://portal.example', 'user', 'password', attempt(2), { installationID: 'i1234567', serviceInstanceID });
    assert.equal(JSON.parse(calls[3].options.body).renewal_installation_id, 'i1234567');
    await new MordeAPI(() => null).login('https://portal.example', 'user', 'password', attempt(3), { installationID: 'i1234567', serviceInstanceID: 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa' });
    assert.equal('renewal_installation_id' in JSON.parse(calls[5].options.body), false);
  } finally { globalThis.fetch = originalFetch; }
});

test('login keeps the full signed int64 attempt generation in the JSON request', async () => {
  const originalFetch = globalThis.fetch;
  const calls = [];
  globalThis.fetch = async (url, options) => {
    calls.push({ url, options });
    return json(String(url).endsWith('/api/capabilities') ? capabilities : loginBody);
  };
  try {
    const generation = '9007199254740993';
    await new MordeAPI(() => null).login('https://portal.example', 'user', 'password', attempt(generation));
    assert.match(calls[1].options.body, /"attempt_generation":9007199254740993/);
  } finally { globalThis.fetch = originalFetch; }
});

test('Workspace sync device challenge uses Client auth and strictly decodes its signing bytes', async () => {
  const originalFetch = globalThis.fetch;
  const calls = [];
  globalThis.fetch = async (url, options) => { calls.push({ url, options }); return json(syncChallenge, 201); };
  try {
    const certificateDER = Buffer.from('synthetic certificate DER');
    const result = await new MordeAPI(() => credentials).createWorkspaceSyncDeviceChallenge('w1234567', syncDeviceID, certificateDER);
    assert.equal(calls.length, 1);
    assert.equal(calls[0].url, 'https://portal.example/api/workspaces/w1234567/sync-device-challenges');
    assert.equal(calls[0].options.method, 'POST');
    assert.equal(calls[0].options.headers.authorization, 'Bearer client-secret');
    assert.deepEqual(JSON.parse(calls[0].options.body), { device_id: syncDeviceID, certificate_der: certificateDER.toString('base64') });
    assert.deepEqual([...result.nonce], [...Buffer.alloc(32, 1)]);
    assert.deepEqual([...result.signingPayload], [...Buffer.from('proof payload')]);
    assert.equal(result.id, 'n1234567');
    assert.equal(result.deviceID, syncDeviceID);
  } finally { globalThis.fetch = originalFetch; }
});

test('Workspace sync device registration sends the proof and idempotency key without exposing relay token in errors', async () => {
  const originalFetch = globalThis.fetch;
  const calls = [];
  globalThis.fetch = async (url, options) => { calls.push({ url, options }); return json(syncRegistration); };
  try {
    const signature = Buffer.from('synthetic signature');
    const result = await new MordeAPI(() => credentials).registerWorkspaceSyncDevice('w1234567', 'n1234567', syncDeviceID, signature, 'register-retry-1');
    assert.equal(calls.length, 1);
    assert.equal(calls[0].url, 'https://portal.example/api/workspaces/w1234567/sync-devices');
    assert.equal(calls[0].options.method, 'POST');
    assert.equal(calls[0].options.headers.authorization, 'Bearer client-secret');
    assert.equal(calls[0].options.headers['Idempotency-Key'], 'register-retry-1');
    assert.deepEqual(JSON.parse(calls[0].options.body), { challenge_id: 'n1234567', signature: signature.toString('base64') });
    assert.equal(result.folderID, syncRegistration.folder_id);
    assert.equal(result.serverDeviceID, syncServerDeviceID);
    assert.equal(result.relayAddress, syncRegistration.relay_address);
    assert.equal(result.state, 'pending_node');

    globalThis.fetch = async () => json({ ...syncRegistration, relay_address: 'relay://relay.example.test:22067/?id=bad&token=abcdefghijklmnopabcdefghijklmnop' });
    await assert.rejects(() => new MordeAPI(() => credentials).registerWorkspaceSyncDevice('w1234567', 'n1234567', syncDeviceID, signature, 'register-retry-2'), (error) => {
      assert.match(error.message, /Invalid Morde Workspace sync device registration response/);
      assert.equal(error.message.includes('abcdefghijklmnopabcdefghijklmnop'), false);
      return true;
    });
  } finally { globalThis.fetch = originalFetch; }
});

test('Workspace sync device methods reject invalid inputs, malformed responses and absent Client auth before use', async () => {
  const originalFetch = globalThis.fetch;
  let calls = 0;
  globalThis.fetch = async () => { calls++; return json({ ...syncChallenge, signing_payload: 'not base64!' }); };
  try {
    const api = new MordeAPI(() => credentials);
    await assert.rejects(() => api.createWorkspaceSyncDeviceChallenge('../admin', syncDeviceID, Buffer.from('cert')), /Invalid workspace ID/);
    await assert.rejects(() => api.registerWorkspaceSyncDevice('w1234567', '../challenge', syncDeviceID, Buffer.from('signature'), 'register-1'), /Invalid Workspace sync device registration request/);
    await assert.rejects(() => api.registerWorkspaceSyncDevice('../admin', 'n1234567', syncDeviceID, Buffer.from('signature'), 'register-1'), /Invalid workspace ID/);
    await assert.rejects(() => api.createWorkspaceSyncDeviceChallenge('w1234567', syncDeviceID, Buffer.from('cert')), /Invalid Morde Workspace sync device challenge response/);
    assert.equal(calls, 1);

    const validInput = ['w1234567', 'n1234567', syncDeviceID, Buffer.from('signature'), 'register-2'];
    await assert.rejects(() => new MordeAPI(() => null).registerWorkspaceSyncDevice(...validInput), /Log in to Morde again/);
    await assert.rejects(() => new MordeAPI(() => ({ ...credentials, clientExpiresAt: 'invalid' })).registerWorkspaceSyncDevice(...validInput), /Log in to Morde again/);
    assert.equal(calls, 1);
  } finally { globalThis.fetch = originalFetch; }
});

test('web renewal and logout only use the fixed Client bearer routes', async () => {
  const originalFetch = globalThis.fetch;
  const calls = [];
  globalThis.fetch = async (url, options) => {
    calls.push({ url, options });
    return url.endsWith('client-logout') ? new Response(null, { status: 204 }) : new Response(JSON.stringify({ web_session_credential: 'new-web-secret', web_session_expires_at: future }), { headers: { 'content-type': 'application/json' } });
  };
  try {
    const api = new MordeAPI(() => credentials);
    assert.equal((await api.webSession()).credential, 'new-web-secret');
    await api.logout();
    assert.deepEqual(calls.map((call) => call.url), ['https://portal.example/api/auth/client-web-session', 'https://portal.example/api/auth/client-logout']);
    assert.ok(calls.every((call) => call.options.headers.authorization === 'Bearer client-secret'));
    assert.equal(calls.every((call) => !call.options.body), true);
  } finally { globalThis.fetch = originalFetch; }
});

test('invalid login responses and expired Client credentials fail closed', async () => {
  const originalFetch = globalThis.fetch;
  let calls = 0;
  globalThis.fetch = async (url) => { calls += 1; return json(String(url).endsWith('/api/capabilities') ? capabilities : { installation_id: 'i1234567', client_credential: 'secret', client_expires_at: future }); };
  try {
    await assert.rejects(() => new MordeAPI(() => null).login('https://portal.example', 'u', 'p', attempt()), /Invalid Morde client login response/);
    await assert.rejects(() => new MordeAPI(() => ({ ...credentials, clientExpiresAt: 'invalid' })).workspaces(), /Log in to Morde again/);
    assert.equal(calls, 2);
  } finally { globalThis.fetch = originalFetch; }
});

test('client login explains account policy errors and offline failure', async () => {
  const originalFetch = globalThis.fetch;
  try {
    for (const [code, status, message] of [
      ['unauthenticated', 401, /用户名或密码错误/],
      ['account_pending', 403, /等待管理员审核/],
      ['account_rejected', 403, /审核未通过/],
      ['account_disabled', 403, /已被禁用/],
      ['rate_limited', 429, /过于频繁/],
      ['forbidden', 403, /修改密码/],
    ]) {
      globalThis.fetch = async (url) => json(String(url).endsWith('/api/capabilities') ? capabilities : { error: { code } }, String(url).endsWith('/api/capabilities') ? 200 : status);
      await assert.rejects(() => new MordeAPI(() => null).login('https://portal.example', 'user', 'password', attempt()), message);
    }
    globalThis.fetch = async () => { throw new TypeError('connect failed'); };
    await assert.rejects(() => new MordeAPI(() => null).login('https://portal.example', 'user', 'password', attempt()), /检查网络和服务地址/);
  } finally { globalThis.fetch = originalFetch; }
});

test('login refuses unsupported services and minimum client versions before sending a password', async () => {
  const originalFetch = globalThis.fetch;
  const calls = [];
  try {
    globalThis.fetch = async (url, options) => { calls.push({ url, options }); return json({}, 404); };
    await assert.rejects(() => new MordeAPI(() => null).login('https://portal.example', 'user', 'password', attempt()), /未发送账号信息/);
    assert.equal(calls.length, 1);
    assert.equal(calls[0].url, 'https://portal.example/api/capabilities');

    calls.length = 0;
    globalThis.fetch = async (url, options) => {
      calls.push({ url, options });
      return json({ ...capabilities, supported_features: capabilities.supported_features.filter((feature) => feature !== 'ordered_client_login') });
    };
    await assert.rejects(() => new MordeAPI(() => null).login('https://portal.example', 'user', 'password', attempt()), /不兼容/);
    assert.equal(calls.length, 1);

    calls.length = 0;
    globalThis.fetch = async (url, options) => {
      calls.push({ url, options });
      return json(String(url).endsWith('/api/capabilities') ? { ...capabilities, minimum_client_version: '0.2.0' } : loginBody);
    };
    await assert.rejects(() => new MordeAPI(() => null, () => '0.1.0').login('https://portal.example', 'user', 'password', attempt()), /版本过低/);
    assert.equal(calls.length, 1);
    assert.equal(calls[0].url, 'https://portal.example/api/capabilities');

    calls.length = 0;
    globalThis.fetch = async (url, options) => {
      calls.push({ url, options });
      return json(String(url).endsWith('/api/capabilities') ? { ...capabilities, minimum_client_version: '0.1.0-rc.2' } : loginBody);
    };
    await new MordeAPI(() => null, () => '0.1.0').login('https://portal.example', 'user', 'password', attempt());
    assert.equal(calls.length, 2);
    assert.equal(calls[1].url, 'https://portal.example/api/auth/client-login');
  } finally { globalThis.fetch = originalFetch; }
});
