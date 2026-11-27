import { test } from 'node:test';
import assert from 'node:assert/strict';
import { MordeAPI, checkedId, credentialExpired, credentialExpiresSoon, gatewayURL, mordeUserDataDirectory, portalOrigin, tracedFetch, withTimeout } from '../out/morde-core.js';

const future = '2099-01-01T00:00:00Z';
const serviceInstanceID = '12345678-1234-4234-8234-123456789abc';
const capabilities = { service_instance_id: serviceInstanceID, api_version: '1', hub_version: null, event_version: null, minimum_client_version: null, supported_features: ['password_login', 'client_installations', 'ordered_client_login'], default_max_request_bytes: 1048576 };
const credentials = { origin: 'https://portal.example', serviceInstanceID, installationId: 'i1234567', clientCredential: 'client-secret', clientExpiresAt: future, syncCredential: 'sync-secret', syncExpiresAt: future };
const loginBody = { installation_id: 'i1234567', client_credential: 'client-secret', client_expires_at: future, sync_credential: 'sync-secret', sync_expires_at: future, web_session_credential: 'web-secret', web_session_expires_at: future, web_session_host_frame_source: 'https://*.hosts.example.test' };
const json = (value, status = 200) => new Response(JSON.stringify(value && typeof value === 'object' && 'web_session_credential' in value && !('web_session_host_frame_source' in value) ? { ...value, web_session_host_frame_source: 'https://*.hosts.example.test' } : value), { status, headers: { 'content-type': 'application/json' } });
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

test('withTimeout stops a hanging operation when its deadline expires', async () => {
  let stopped = false;
  await assert.rejects(withTimeout(new Promise(() => {}), 1, () => { stopped = true; }), /timed out/i);
  assert.equal(stopped, true);
  await assert.rejects(withTimeout(new Promise(() => {}), 1, () => { throw new Error('stop unavailable'); }), /timed out/i);
});

test('tracedFetch adds a valid W3C context and preserves caller request IDs', async () => {
  let request;
  const response = await tracedFetch(async (input, init) => {
    request = new Request(input, init);
    return new Response('{}', { headers: { 'X-Trace-Id': 'a'.repeat(32) } });
  }, 'https://portal.example/api/test', { headers: { 'X-Request-Id': 'caller-request-id' } });

  const traceparent = request.headers.get('traceparent');
  assert.match(traceparent, /^00-[0-9a-f]{32}-[0-9a-f]{16}-01$/);
  const [, traceID, spanID] = traceparent.split('-');
  assert.notEqual(traceID, '0'.repeat(32));
  assert.notEqual(spanID, '0'.repeat(16));
  assert.equal(request.headers.get('X-Request-Id'), 'caller-request-id');
  assert.equal(response.headers.get('X-Trace-Id'), 'a'.repeat(32));
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
    assert.equal(new Headers(calls[1].options.headers).get('Idempotency-Key'), 'retry-1');
    assert.deepEqual(JSON.parse(calls[1].options.body), { name: 'user', password: 'password', platform: 'desktop', display_name: 'morde-pc', client_instance_id: attempt().clientInstanceID, attempt_generation: 1 });
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

test('Morde API uses the injected network stack for remote calls', async () => {
  const calls = [];
  const api = new MordeAPI(() => credentials, () => '', async (url, options) => {
    calls.push({ url, options });
    return json({ default_host_id: 'h1234567' });
  });
  assert.deepEqual(await api.userPreferences({ credential: 'web-secret', expiresAt: future }), { defaultHostID: 'h1234567' });
  assert.equal(calls[0].url, 'https://portal.example/api/me/preferences');
});

test('project sync reads the account default computer through its short-lived Portal session', async () => {
  const originalFetch = globalThis.fetch;
  const calls = [];
  globalThis.fetch = async (url, options) => {
    calls.push({ url, options });
    return json({ default_host_id: 'h1234567' });
  };
  try {
    const session = { credential: 'web-secret', expiresAt: future };
    const result = await new MordeAPI(() => credentials).userPreferences(session);
    assert.deepEqual(result, { defaultHostID: 'h1234567' });
    assert.equal(calls[0].url, 'https://portal.example/api/me/preferences');
    assert.equal(new Headers(calls[0].options.headers).get('cookie'), 'ch_portal=web-secret');
    assert.equal(new Headers(calls[0].options.headers).has('authorization'), false);
    globalThis.fetch = async () => json({ default_host_id: '../other' });
    await assert.rejects(() => new MordeAPI(() => credentials).userPreferences(session), /Invalid host ID/);
  } finally { globalThis.fetch = originalFetch; }
});

test('Host embed tickets use the short-lived Portal session and reject targets outside the request', async () => {
  const originalFetch = globalThis.fetch;
  const calls = [];
  globalThis.fetch = async (url, options) => {
    calls.push({ url, options });
    return json({ exchange_url: 'https://h1234567.hosts.example.test/__ch/embed-auth', ticket: 'A'.repeat(43),
      embed_nonce: 'B'.repeat(32), targets: ['browser', 'terminal'], expires_at: future }, 201);
  };
  try {
    const api = new MordeAPI(() => credentials);
    const session = { credential: 'web-secret', expiresAt: future };
    const result = await api.issueHostEmbedTicket('h1234567', 'c1234567', 'browser', ['browser', 'terminal'], session);
    assert.deepEqual(result, { exchangeURL: 'https://h1234567.hosts.example.test/__ch/embed-auth', ticket: 'A'.repeat(43),
      embedNonce: 'B'.repeat(32), targets: ['browser', 'terminal'] });
    assert.equal(calls[0].url, 'https://portal.example/api/hosts/h1234567/embed-tickets');
    assert.equal(new Headers(calls[0].options.headers).get('cookie'), 'ch_portal=web-secret');
    assert.equal(new Headers(calls[0].options.headers).get('origin'), credentials.origin);
    assert.equal(new Headers(calls[0].options.headers).has('authorization'), false);
    assert.deepEqual(JSON.parse(calls[0].options.body), { conversation_id: 'c1234567', target: 'browser', targets: ['browser', 'terminal'] });
    await assert.rejects(() => api.issueHostEmbedTicket('h1234567', 'c1234567', 'browser', ['browser'], session), /Invalid Morde Host embed ticket response/);
    await assert.rejects(() => api.issueHostEmbedTicket('h1234567', '../other', 'browser', ['browser'], session), /Invalid Morde Host embed request/);
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
    assert.equal(new Headers(calls[0].options.headers).get('authorization'), 'Bearer client-secret');
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
    assert.equal(new Headers(calls[0].options.headers).get('authorization'), 'Bearer client-secret');
    assert.equal(new Headers(calls[0].options.headers).get('Idempotency-Key'), 'register-retry-1');
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
    return url.endsWith('client-logout') ? new Response(null, { status: 204 }) : json({ web_session_credential: 'new-web-secret', web_session_expires_at: future });
  };
  try {
    const api = new MordeAPI(() => credentials);
    assert.equal((await api.webSession()).credential, 'new-web-secret');
    await api.logout();
    assert.deepEqual(calls.map((call) => call.url), ['https://portal.example/api/auth/client-web-session', 'https://portal.example/api/auth/client-logout']);
  assert.ok(calls.every((call) => new Headers(call.options.headers).get('authorization') === 'Bearer client-secret'));
    assert.equal(calls.every((call) => !call.options.body), true);
  } finally { globalThis.fetch = originalFetch; }
});

test('project upload is gated by both advertised sync features', async () => {
  const originalFetch = globalThis.fetch;
  const calls = [];
  globalThis.fetch = async (url) => {
    calls.push(String(url));
    return json(String(url).endsWith('/api/capabilities')
      ? { ...capabilities, supported_features: [...capabilities.supported_features, 'workspace_sync'] }
      : {});
  };
  try {
    const api = new MordeAPI(() => credentials);
    await assert.rejects(() => api.requireProjectUploadCapability(), /尚未开放云端电脑项目上传/);
    globalThis.fetch = async (url) => {
      calls.push(String(url));
      return json({ ...capabilities, supported_features: [...capabilities.supported_features, 'workspace_sync', 'project_upload'] });
    };
    await api.requireProjectUploadCapability();
    assert.deepEqual(calls, [
      'https://portal.example/api/capabilities',
      'https://portal.example/api/capabilities',
    ]);
  } finally { globalThis.fetch = originalFetch; }
});

test('project folder registration uses a Portal cookie and sends metadata only', async () => {
  const originalFetch = globalThis.fetch;
  const calls = [];
  const folder = {
    folder_id: 'morde-0123456789abcdef0123456789abcdef', workspace_id: 'w1234567',
    folder_kind: 'project', project_name: 'demo', relative_path: 'project/demo',
    state: 'pending_node', sync_available: false,
  };
  globalThis.fetch = async (url, options) => {
    calls.push({ url: String(url), options });
    return json(String(url).endsWith('/sync-folders') && options.method === 'POST' ? folder : { items: [folder] }, options.method === 'POST' ? 201 : 200);
  };
  try {
    const api = new MordeAPI(() => credentials);
    const session = { credential: 'portal-cookie-secret', expiresAt: future };
    const listed = await api.hostProjectFolders('h1234567', session);
    const registered = await api.registerHostProjectFolder('h1234567', 'demo', session, 'registration-key');
    assert.equal(listed[0].folderID, folder.folder_id);
    assert.equal(registered.relativePath, 'project/demo');
    assert.equal(new Headers(calls[0].options.headers).get('cookie'), 'ch_portal=portal-cookie-secret');
    assert.equal(new Headers(calls[0].options.headers).get('authorization'), null);
    assert.equal(new Headers(calls[1].options.headers).get('origin'), credentials.origin);
    assert.equal(new Headers(calls[1].options.headers).get('Idempotency-Key'), 'registration-key');
    assert.deepEqual(JSON.parse(calls[1].options.body), { project_name: 'demo' });
    assert.equal(JSON.stringify(calls).includes('file'), false);
    await assert.rejects(() => api.registerHostProjectFolder('../bad', 'demo', session, 'key'), /Invalid host ID/);
    await assert.rejects(() => api.registerHostProjectFolder('h1234567', '../bad', session, 'key'), /Invalid Morde project folder registration request/);
    folder.relative_path = 'project/../outside';
    await assert.rejects(() => api.hostProjectFolders('h1234567', session), /Invalid Morde project folder path/);
    folder.relative_path = 'project/demo';
    folder.folder_id = 'invalid-folder-id';
    await assert.rejects(() => api.hostProjectFolders('h1234567', session), /Invalid Morde project folder response/);
  } finally { globalThis.fetch = originalFetch; }
});

test('API failures include the request route without exposing credentials', async () => {
  const originalFetch = globalThis.fetch;
  globalThis.fetch = async () => new Response('not found', { status: 404 });
  try {
    const api = new MordeAPI(() => credentials);
    await assert.rejects(() => api.workspaces(), (error) => {
      assert.equal(error.message, 'Morde returned HTTP 404 (GET /api/workspaces)');
      assert.equal(error.message.includes(credentials.clientCredential), false);
      return true;
    });
    await assert.rejects(() => api.hostProjectFolders('h1234567', { credential: 'portal-cookie-secret', expiresAt: future }), (error) => {
      assert.equal(error.message, 'Morde returned HTTP 404 (GET /api/hosts/h1234567/sync-folders)');
      assert.equal(error.message.includes(credentials.clientCredential), false);
      assert.equal(error.message.includes('portal-cookie-secret'), false);
      return true;
    });
  } finally { globalThis.fetch = originalFetch; }
});

test('API failures include only UUID request IDs for log correlation', async () => {
  const originalFetch = globalThis.fetch;
  const requestID = '12345678-1234-4234-8234-123456789abc';
  globalThis.fetch = async () => new Response('forbidden', { status: 403, headers: { 'x-request-id': requestID } });
  try {
    const api = new MordeAPI(() => credentials);
    await assert.rejects(() => api.workspaces(), (error) => {
      assert.equal(error.message, `Morde returned HTTP 403 (GET /api/workspaces); request ID ${requestID}`);
      assert.equal(error.message.includes(credentials.clientCredential), false);
      return true;
    });
    await assert.rejects(() => api.issueHostEmbedTicket('h1234567', 'c1234567', 'browser', ['browser'],
      { credential: 'portal-cookie-secret', expiresAt: future, hostFrameSource: 'https://*.hosts.example.test' }), (error) => {
      assert.equal(error.message, `Morde returned HTTP 403 (POST /api/hosts/h1234567/embed-tickets); request ID ${requestID}`);
      assert.equal(error.message.includes('portal-cookie-secret'), false);
      return true;
    });

    globalThis.fetch = async () => new Response('forbidden', { status: 403, headers: { 'x-request-id': 'gateway-auth-rejected' } });
    await assert.rejects(() => api.workspaces(), (error) => {
      assert.equal(error.message, 'Morde returned HTTP 403 (GET /api/workspaces)');
      assert.equal(error.message.includes('gateway-auth-rejected'), false);
      return true;
    });
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

test('login rejects unsafe Host frame sources from the service', async () => {
  const originalFetch = globalThis.fetch;
  try {
    for (const hostFrameSource of ['https://*.hosts.example; script-src *', 'https://*..hosts.example', 'https://*.hosts.example:99999']) {
      globalThis.fetch = async (url) => json(String(url).endsWith('/api/capabilities') ? capabilities : { ...loginBody, web_session_host_frame_source: hostFrameSource });
      await assert.rejects(() => new MordeAPI(() => null).login('https://portal.example', 'u', 'p', attempt()), /Invalid Morde Web session response/);
    }
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

test('client login preserves the server conflict reason for ordered recovery', async () => {
  const originalFetch = globalThis.fetch;
  globalThis.fetch = async (url) => String(url).endsWith('/api/capabilities') ? json(capabilities) :
    json({ error: { code: 'conflict', details: { reason: 'client_login_state_changed' } } }, 409);
  try {
    await assert.rejects(() => new MordeAPI(() => null).login('https://portal.example', 'user', 'password', attempt()),
      (error) => error.code === 'conflict' && error.reason === 'client_login_state_changed');
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

test('Codex claim preserves exact structured-question IDs and rejects malformed answer payloads', async () => {
  const originalFetch = globalThis.fetch;
  const command = {
    id: 'g1234567', session_id: 'l1234567', profile_key: 'profile-1', thread_id: 'native-thread', grant_version: 1,
    type: 'answer_question', input: {
      question_request_id: '100', question_version: 'a'.repeat(64), answers: { ' choice ': ['A'] },
    },
  };
  try {
    globalThis.fetch = async () => json({ items: [command] });
    const [parsed] = await new MordeAPI(() => credentials).codexClaim();
    assert.deepEqual(parsed, {
      id: 'g1234567', session_id: 'l1234567', profile_key: 'profile-1', thread_id: 'native-thread', grant_version: 1,
      type: 'answer_question', input: {
        question_request_id: '100', question_version: 'a'.repeat(64), answers: { ' choice ': ['A'] },
      },
    });

    for (const invalidInput of [
      { question_request_id: '100', answers: { choice: ['A'] } },
      { question_request_id: '100', question_version: 'v1', answers: { choice: ['A'] } },
      { question_request_id: '100', question_version: 'a'.repeat(64), answers: { '   ': ['A'] } },
      { question_request_id: '100', question_version: 'a'.repeat(64), answers: { choice: [] } },
      { question_request_id: '100', question_version: 'a'.repeat(64), answers: { choice: [''] } },
      { question_request_id: '100', question_version: 'a'.repeat(64), answers: { choice: ['   '] } },
    ]) {
      globalThis.fetch = async () => json({ items: [{ ...command, input: invalidInput }] });
      await assert.rejects(() => new MordeAPI(() => credentials).codexClaim(), /(Invalid|Incomplete) Morde Codex/);
    }
  } finally { globalThis.fetch = originalFetch; }
});
