import assert from 'node:assert/strict';
import { test } from 'node:test';
import { createHash } from 'node:crypto';
import { createRequire } from 'node:module';
import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, statSync, writeFileSync } from 'node:fs';
import { homedir, tmpdir } from 'node:os';
import { join } from 'node:path';

const require = createRequire(import.meta.url);
const future = '2099-01-01T00:00:00Z';
const serviceInstanceID = '12345678-1234-4234-8234-123456789abc';
const capabilitiesResponse = { service_instance_id: serviceInstanceID, api_version: '1', hub_version: null, event_version: null, minimum_client_version: null, supported_features: ['password_login', 'client_installations', 'ordered_client_login'], default_max_request_bytes: 1048576 };
const capabilitiesWithWorkspaceSync = { ...capabilitiesResponse, supported_features: [...capabilitiesResponse.supported_features, 'workspace_sync'] };
const auth = { origin: 'https://portal.example', serviceInstanceID, accountName: 'user', installationId: 'i1234567', clientCredential: 'client-secret', clientExpiresAt: future, syncCredential: 'sync-secret', syncExpiresAt: future };
const loginResponse = { installation_id: auth.installationId, client_credential: auth.clientCredential, client_expires_at: future, sync_credential: auth.syncCredential, sync_expires_at: future, web_session_credential: 'web-secret', web_session_expires_at: future };
const json = (value, status = 200) => new Response(JSON.stringify(value), { status, headers: { 'content-type': 'application/json' } });
const loginFetch = (url, fallback = loginResponse) => json(String(url).endsWith('/api/capabilities') ? capabilitiesResponse : fallback);
const profileFile = (h, value, name) => join(h.userData, `${name}-${createHash('sha256').update(`${value.origin}\0${value.serviceInstanceID}\0${value.accountName}\0${value.installationId}`).digest('hex')}.json`);
const liveSyncProfileFile = (h, value) => join(h.userData, `live-sync-${createHash('sha256').update(`${value.origin}\0${value.serviceInstanceID}\0${value.accountName}\0${value.installationId}`).digest('hex')}.bin`);
const workspaceSyncProfileFile = (h, value) => join(h.userData, `workspace-sync-${createHash('sha256').update(`${value.origin}\0${value.serviceInstanceID}\0${value.accountName}\0${value.installationId}`).digest('hex')}.bin`);
function savedWorkspaceSyncProfile(value, enabled) {
  const deviceID = 'ABCDEFG-ABCDEFG-ABCDEFG-ABCDEFG-ABCDEFG-ABCDEFG-ABCDEFG-ABCDEFG';
  return { version: 1, serviceInstanceID: value.serviceInstanceID, installationId: value.installationId, deviceID,
    bindings: { w1234567: { bindingID: 'd1234567', folderID: `morde-${'a'.repeat(32)}`, deviceID, serverDeviceID: deviceID,
      relayAddress: `relay://127.0.0.1:22067/?id=${deviceID}&token=${'b'.repeat(32)}`, state: 'active', enabled } }, pending: {} };
}

function harness(initialAuth = null, existingRoot = null) {
  const root = existingRoot ?? mkdtempSync(join(tmpdir(), 'morde-desktop-auth-'));
  const userData = join(root, 'Morde Client');
  const fs = require('node:fs');
  fs.mkdirSync(userData, { recursive: true });
  if (initialAuth) writeFileSync(join(userData, 'credentials.bin'), JSON.stringify({ version: 2, credentials: initialAuth }));
  const handlers = new Map(), windows = [], views = [], partitions = [], appEvents = new Map(), shellURLs = [];
  class FakeContents {
    events = new Map();
    messages = [];
    url = '';
    closed = false;
    on(name, fn) { this.events.set(name, fn); }
    setWindowOpenHandler(fn) { this.open = fn; }
    async loadURL(url) { this.url = url; }
    getURL() { return this.url; }
    isDestroyed() { return this.closed; }
    focus() { this.focused = true; }
    close() { this.closed = true; }
    send(...args) { this.messages.push(args); }
  }
  class FakeWindow {
    constructor(options) {
      this.options = options;
      this.events = new Map();
      this.webContents = options.webContents ?? new FakeContents();
      this.contentView = { attached: [], getBounds: () => ({ x: 0, y: 0, width: 0, height: 0 }), addChildView: (view) => this.contentView.attached.push(view), removeChildView: (view) => { this.contentView.attached = this.contentView.attached.filter((item) => item !== view); } };
      windows.push(this);
    }
    on(name, fn) { this.events.set(name, fn); }
    once(name, fn) { this.events.set(name, fn); }
    async loadFile(path) { this.path = path; }
    async loadURL(url) { this.webContents.url = url; }
    show() {}
    getContentBounds() { return { x: 0, y: 0, width: 1120, height: 760 }; }
    focus() { this.focused = true; }
    isDestroyed() { return this.destroyed || false; }
    close() { this.destroyed = true; this.events.get('closed')?.(); }
  }
  class FakeView {
    constructor(options) { this.options = options; this.webContents = new FakeContents(); views.push(this); }
    setBounds(bounds) { this.bounds = bounds; }
  }
  const fakeSession = { fromPartition(name) {
    const partition = {
      name, cookies: { values: [], async set(value) { this.values.push(value); } },
      webRequest: { onCompleted(_filter, fn) { this.completed = fn; } },
      events: new Map(), on(event, fn) { this.events.set(event, fn); },
      setPermissionRequestHandler(fn) { this.permissionRequest = fn; },
      async clearStorageData() { this.cleared = true; },
    };
    partitions.push(partition);
    return partition;
  } };
  const paths = { appData: root };
  const dialog = { showOpenDialog: async () => ({ canceled: true, filePaths: [] }), showMessageBox: async () => ({ response: 0 }) };
  const electron = {
    app: { setName() {}, getPath: (key) => paths[key] ?? root, setPath: (key, path) => { paths[key] = path; }, getLocale: () => 'en', getVersion: () => '0.1.0', requestSingleInstanceLock: () => true, isPackaged: false, whenReady: () => Promise.resolve(), on: (name, fn) => appEvents.set(name, fn), quit() {} },
    BrowserWindow: FakeWindow, WebContentsView: FakeView, session: fakeSession,
    ipcMain: { handle: (channel, fn) => handlers.set(channel, fn) },
    dialog,
    Menu: { buildFromTemplate: (template) => template, setApplicationMenu() {} },
    safeStorage: { isEncryptionAvailable: () => true, getSelectedStorageBackend: () => 'gnome_libsecret', encryptString: (value) => Buffer.from(value), decryptString: (value) => value.toString() },
    shell: { openExternal: async (url) => { shellURLs.push(url); } },
    Tray: class { setToolTip() {} setContextMenu() {} on() {} },
  };
  const Module = require('node:module');
  const originalLoad = Module._load;
  const entry = require.resolve('../out/morde-main.js');
  Module._load = function (request, parent, isMain) { if (request === 'electron') return electron; return originalLoad.call(this, request, parent, isMain); };
  delete require.cache[entry];
  try { require(entry); } finally { Module._load = originalLoad; }
  return {
    root, userData, handlers, windows, views, partitions, appEvents, shellURLs, dialog,
    async ready() { await new Promise((resolve) => setImmediate(resolve)); await new Promise((resolve) => setImmediate(resolve)); },
    invoke(channel, ...args) { return handlers.get(channel)({ sender: windows.at(-1).webContents }, ...args); },
    cleanup(removeRoot = true) { delete require.cache[entry]; if (removeRoot) rmSync(root, { recursive: true, force: true }); },
  };
}

test('one desktop login stores Client and Sync only, injects an HttpOnly Web cookie in an isolated view', async () => {
  const originalFetch = globalThis.fetch;
  const calls = [];
  globalThis.fetch = async (url, options) => { calls.push({ url, options }); return loginFetch(url); };
  const h = harness();
  try {
    await h.ready();
    const state = await h.invoke('morde:login', 'https://portal.example', 'user', 'password');
    assert.equal(state.authenticated, true);
    assert.equal(calls.length, 3);
    assert.equal(calls[0].url, 'https://portal.example/api/capabilities');
    assert.equal(calls[1].url, 'https://portal.example/api/auth/client-login');
    assert.equal(calls[2].url, 'https://portal.example/api/capabilities');
    const body = JSON.parse(calls[1].options.body);
    const persistedAttempt = JSON.parse(readFileSync(join(h.userData, 'client-login-attempts.json'), 'utf8'))[createHash('sha256').update(`${auth.origin}\0${auth.accountName}`).digest('hex')];
    assert.deepEqual(persistedAttempt, { clientInstanceID: body.client_instance_id, attemptGeneration: 1, retiredClientInstanceIDs: [], installationID: auth.installationId, allowLegacyRenewal: false });
    assert.equal(body.attempt_generation, 1);
    assert.match(calls[1].options.headers['Idempotency-Key'], /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i);
    assert.equal(h.partitions.length, 1);
    assert.match(h.partitions[0].name, /^morde-user-/);
    assert.deepEqual(h.partitions[0].cookies.values.map((item) => ({ name: item.name, httpOnly: item.httpOnly, secure: item.secure, sameSite: item.sameSite })), [{ name: 'ch_portal', httpOnly: true, secure: true, sameSite: 'strict' }]);
    assert.equal(h.views[0].options.webPreferences.preload, undefined);
    assert.equal(h.views[0].options.webPreferences.sandbox, true);
    assert.equal(h.windows[0].contentView.attached[0], h.views[0]);
    assert.deepEqual(h.views[0].bounds, { x: 0, y: 64, width: 1120, height: 696 });
    const persisted = JSON.parse(readFileSync(join(h.userData, 'credentials.bin'), 'utf8'));
    assert.equal(persisted.version, 2);
    assert.equal(JSON.stringify(persisted).includes('password'), false);
    assert.equal(JSON.stringify(persisted).includes('web-secret'), false);
    assert.equal(h.handlers.has('morde:pair'), false);
    assert.equal(h.handlers.has('morde:submit'), false);
    await assert.rejects(h.handlers.get('morde:state')({ sender: h.views[0].webContents }), /Invalid desktop window/);
  } finally { globalThis.fetch = originalFetch; h.cleanup(); }
});

test('renderer state exposes only a local directory label and a resumable Workspace checkpoint summary', async () => {
  const originalFetch = globalThis.fetch;
  globalThis.fetch = async (url) => loginFetch(url);
  const h = harness(auth);
  const privateDirectory = join(h.root, 'private-workspace-name');
  writeFileSync(profileFile(h, auth, 'mappings'), JSON.stringify([{ workspaceId: 'w1234567', directory: privateDirectory, dev: '4', ino: '9' }]));
  writeFileSync(profileFile(h, auth, 'sync-checkpoint'), JSON.stringify({ serviceInstanceID: auth.serviceInstanceID, installationId: auth.installationId,
    workspaceId: 'w1234567', directory: privateDirectory, dev: '4', ino: '9', mode: 'upload', scope: ['src'], idempotencyKey: 'private-key' }));
  try {
    await h.ready();
    const state = await h.invoke('morde:state');
    assert.deepEqual(state.mappings, [{ workspaceId: 'w1234567', directoryName: 'private-workspace-name' }]);
    assert.equal('directory' in state.mappings[0], false);
    assert.equal('dev' in state.mappings[0], false);
    assert.equal('ino' in state.mappings[0], false);
    assert.deepEqual(state.syncRecovery, { state: 'pending', workspaceId: 'w1234567', mode: 'upload' });
    const serialized = JSON.stringify(state);
    for (const secret of [h.root, privateDirectory, auth.clientCredential, auth.syncCredential]) assert.equal(serialized.includes(secret), false);
  } finally { globalThis.fetch = originalFetch; h.cleanup(); }
});

test('choosing a folder returns only a safe basename to the renderer', async () => {
  const originalFetch = globalThis.fetch;
  globalThis.fetch = async (url) => json(String(url).endsWith('/api/workspaces/w1234567') ? { state: 'ready' } : { web_session_credential: 'renewed-web', web_session_expires_at: future });
  const h = harness(auth);
  const chosenRoot = mkdtempSync(join(homedir(), 'morde-desktop-choose-'));
  const chosenDirectory = join(chosenRoot, 'chosen-folder');
  mkdirSync(chosenDirectory);
  h.dialog.showOpenDialog = async () => ({ canceled: false, filePaths: [chosenDirectory] });
  try {
    await h.ready();
    const chosen = await h.invoke('morde:choose-directory', 'w1234567');
    assert.deepEqual(chosen, { workspaceId: 'w1234567', directoryName: 'chosen-folder' });
    const state = await h.invoke('morde:state');
    assert.deepEqual(state.mappings, [chosen]);
    assert.equal(JSON.stringify(state).includes(chosenDirectory), false);
  } finally { globalThis.fetch = originalFetch; h.cleanup(); rmSync(chosenRoot, { recursive: true, force: true }); }
});

test('login attempt generation and client instance survive a restart while each retry gets a new key', async () => {
  const originalFetch = globalThis.fetch;
  const root = mkdtempSync(join(tmpdir(), 'morde-desktop-login-retry-'));
  const requests = [];
  let loginCount = 0;
  globalThis.fetch = async (url, options) => {
    requests.push({ url, options });
    if (String(url).endsWith('/api/capabilities')) return loginFetch(url);
    loginCount++;
    return loginCount === 1 ? json({ error: { code: 'unauthenticated' } }, 401) : loginFetch(url);
  };
  let first, second;
  try {
    first = harness(null, root);
    await first.ready();
    await assert.rejects(first.invoke('morde:login', auth.origin, auth.accountName, 'password'), /用户名或密码错误/);
    const firstRequest = requests.find((request) => String(request.url).endsWith('/api/auth/client-login'));
    const firstBody = JSON.parse(firstRequest.options.body);
    const firstKey = firstRequest.options.headers['Idempotency-Key'];
    first.cleanup(false);

    second = harness(null, root);
    await second.ready();
    await second.invoke('morde:login', auth.origin, auth.accountName, 'password');
    const loginRequests = requests.filter((request) => String(request.url).endsWith('/api/auth/client-login'));
    const secondRequest = loginRequests[1];
    const secondBody = JSON.parse(secondRequest.options.body);
    const state = JSON.parse(readFileSync(join(second.userData, 'client-login-attempts.json'), 'utf8'))[createHash('sha256').update(`${auth.origin}\0${auth.accountName}`).digest('hex')];
    assert.equal(secondBody.client_instance_id, firstBody.client_instance_id);
    assert.equal(firstBody.attempt_generation, 1);
    assert.equal(secondBody.attempt_generation, 2);
    assert.notEqual(secondRequest.options.headers['Idempotency-Key'], firstKey);
    assert.deepEqual(state, { clientInstanceID: firstBody.client_instance_id, attemptGeneration: 2, retiredClientInstanceIDs: [], installationID: auth.installationId, allowLegacyRenewal: false });
  } finally {
    first?.cleanup(false);
    second?.cleanup();
    globalThis.fetch = originalFetch;
  }
});

test('renewal requires a matching client fence and the persisted int64 generation stays exact', async () => {
  const originalFetch = globalThis.fetch;
  const requests = [];
  const instanceID = '123e4567-e89b-42d3-a456-426614174000';
  const profileID = createHash('sha256').update(`${auth.origin}\0${auth.accountName}`).digest('hex');
  globalThis.fetch = async (url, options) => {
    requests.push({ url, options });
    return loginFetch(url);
  };
  const root = mkdtempSync(join(tmpdir(), 'morde-desktop-login-int64-'));
  const expiredAuth = { ...auth, clientExpiresAt: '2000-01-01T00:00:00Z' };
  let h;
  try {
    h = harness(expiredAuth, root);
    writeFileSync(join(h.userData, 'installation.json'), JSON.stringify({ origin: auth.origin, serviceInstanceID: auth.serviceInstanceID, name: auth.accountName, id: auth.installationId }));
    writeFileSync(join(h.userData, 'client-login-attempts.json'), `{"${profileID}":{"clientInstanceID":"${instanceID}","attemptGeneration":9007199254740992,"retiredClientInstanceIDs":[],"installationID":"${auth.installationId}","allowLegacyRenewal":false}}`);
    await h.ready();
    await h.invoke('morde:login', auth.origin, auth.accountName, 'password');
    const loginRequest = requests.find((request) => String(request.url).endsWith('/api/auth/client-login'));
    assert.match(loginRequest.options.body, /"attempt_generation":9007199254740993/);
    const body = JSON.parse(loginRequest.options.body);
    assert.equal(body.client_instance_id, instanceID);
    assert.equal(body.renewal_installation_id, auth.installationId);
    assert.match(readFileSync(join(h.userData, 'client-login-attempts.json'), 'utf8'), /"attemptGeneration":9007199254740993/);
  } finally {
    h?.cleanup(false);
    rmSync(root, { recursive: true, force: true });
    globalThis.fetch = originalFetch;
  }
});

test('legacy installation metadata without a service identity is not reused', async () => {
  const originalFetch = globalThis.fetch;
  const requests = [];
  globalThis.fetch = async (url, options) => {
    requests.push({ url, options });
    return loginFetch(url);
  };
  const h = harness({ ...auth, clientExpiresAt: '2000-01-01T00:00:00Z' });
  try {
  writeFileSync(join(h.userData, 'installation.json'), JSON.stringify({ origin: auth.origin, name: auth.accountName, id: auth.installationId }));
    await h.ready();
    await h.invoke('morde:login', auth.origin, auth.accountName, 'password');
    const loginRequest = requests.find((request) => String(request.url).endsWith('/api/auth/client-login'));
    const body = JSON.parse(loginRequest.options.body);
    assert.equal('renewal_installation_id' in body, false);
    assert.equal(body.attempt_generation, 1);
  } finally { globalThis.fetch = originalFetch; h.cleanup(); }
});

test('a generation-zero post-logout fence never renews the revoked installation', async () => {
  const originalFetch = globalThis.fetch;
  const requests = [];
  const instanceID = '123e4567-e89b-42d3-a456-426614174000';
  const retiredID = '123e4567-e89b-42d3-a456-426614174001';
  const profileID = createHash('sha256').update(`${auth.origin}\0${auth.accountName}`).digest('hex');
  globalThis.fetch = async (url, options) => {
    requests.push({ url, options });
    return loginFetch(url);
  };
  const h = harness({ ...auth, clientExpiresAt: '2000-01-01T00:00:00Z' });
  try {
    writeFileSync(join(h.userData, 'installation.json'), JSON.stringify({ origin: auth.origin, serviceInstanceID: auth.serviceInstanceID, name: auth.accountName, id: auth.installationId }));
    writeFileSync(join(h.userData, 'client-login-attempts.json'), `{"${profileID}":{"clientInstanceID":"${instanceID}","attemptGeneration":0,"retiredClientInstanceIDs":["${retiredID}"],"installationID":null,"allowLegacyRenewal":false}}`);
    await h.ready();
    await h.invoke('morde:login', auth.origin, auth.accountName, 'password');
    const loginRequest = requests.find((request) => String(request.url).endsWith('/api/auth/client-login'));
    const body = JSON.parse(loginRequest.options.body);
    assert.equal(body.client_instance_id, instanceID);
    assert.equal(body.attempt_generation, 1);
    assert.equal('renewal_installation_id' in body, false);
  } finally { globalThis.fetch = originalFetch; h.cleanup(); }
});

test('a late login response cannot replace credentials from the newest generation', async () => {
  const originalFetch = globalThis.fetch;
  const pending = [];
  const requests = [];
  globalThis.fetch = async (url, options) => {
    if (String(url).endsWith('/api/capabilities')) return loginFetch(url);
    requests.push({ url, options });
    return new Promise((resolve) => pending.push(resolve));
  };
  const h = harness();
  try {
    await h.ready();
    const old = h.invoke('morde:login', auth.origin, auth.accountName, 'password');
    const latest = h.invoke('morde:login', auth.origin, auth.accountName, 'password');
    await new Promise((resolve) => setImmediate(resolve));
    assert.equal(requests.length, 2);
    const oldBody = JSON.parse(requests[0].options.body);
    const latestBody = JSON.parse(requests[1].options.body);
    assert.equal(oldBody.client_instance_id, latestBody.client_instance_id);
    assert.equal(oldBody.attempt_generation, 1);
    assert.equal(latestBody.attempt_generation, 2);
    assert.notEqual(requests[0].options.headers['Idempotency-Key'], requests[1].options.headers['Idempotency-Key']);

    pending[1](json({ ...loginResponse, client_credential: 'newest-client' }));
    await latest;
    pending[0](json({ ...loginResponse, client_credential: 'late-client' }));
    await assert.rejects(old, /较新的登录尝试已开始/);
    const stored = JSON.parse(readFileSync(join(h.userData, 'credentials.bin'), 'utf8'));
    assert.equal(stored.credentials.clientCredential, 'newest-client');
  } finally { globalThis.fetch = originalFetch; h.cleanup(); }
});

test('a late login failure is discarded after the newest generation succeeds', async () => {
  const originalFetch = globalThis.fetch;
  const pending = [];
  globalThis.fetch = async (url) => String(url).endsWith('/api/capabilities')
    ? loginFetch(url)
    : new Promise((resolve) => pending.push(resolve));
  const h = harness();
  try {
    await h.ready();
    const old = h.invoke('morde:login', auth.origin, auth.accountName, 'password');
    const latest = h.invoke('morde:login', auth.origin, auth.accountName, 'password');
    await new Promise((resolve) => setImmediate(resolve));
    pending[1](json(loginResponse));
    await latest;
    pending[0](json({ error: { code: 'unauthenticated' } }, 401));
    await assert.rejects(old, /较新的登录尝试已开始，已丢弃此响应/);
    const stored = JSON.parse(readFileSync(join(h.userData, 'credentials.bin'), 'utf8'));
    assert.equal(stored.credentials.clientCredential, auth.clientCredential);
  } finally { globalThis.fetch = originalFetch; h.cleanup(); }
});

test('logout still revokes and clears authorization when the client fence cannot be rotated', async () => {
  const originalFetch = globalThis.fetch;
  const requests = [];
  globalThis.fetch = async (url, options) => {
    requests.push({ url, options });
    return String(url).endsWith('/api/auth/client-logout') ? new Response(null, { status: 204 }) : loginFetch(url);
  };
  const fs = require('node:fs');
  const originalRename = fs.renameSync;
  const root = mkdtempSync(join(tmpdir(), 'morde-desktop-logout-fence-error-'));
  let h, restarted;
  try {
    h = harness(null, root);
    await h.ready();
    await h.invoke('morde:login', auth.origin, auth.accountName, 'password');
    fs.renameSync = function (source, destination) {
      if (String(destination).endsWith('client-login-attempts.json')) throw new Error('simulated fence write failure');
      return originalRename.call(this, source, destination);
    };
    const warning = await h.invoke('morde:logout');
    fs.renameSync = originalRename;
    assert.match(warning, /登录设备状态无法保存，后续登录已禁止/);
    assert.equal(requests.some((request) => String(request.url).endsWith('/api/auth/client-logout')), true);
    assert.equal((await h.invoke('morde:state')).authenticated, false);
    assert.equal(existsSync(join(h.userData, 'credentials.bin')), false);
    assert.equal(existsSync(join(h.userData, 'installation.json')), false);
    const requestCount = requests.length;
    await assert.rejects(h.invoke('morde:login', auth.origin, auth.accountName, 'password'), /无法安全读取或写入/);
    assert.equal(requests.length, requestCount);

    h.cleanup(false);
    restarted = harness(null, root);
    await restarted.ready();
    await assert.rejects(restarted.invoke('morde:login', auth.origin, auth.accountName, 'password'), /无法安全读取或写入/);
  } finally {
    fs.renameSync = originalRename;
    h?.cleanup(false);
    restarted?.cleanup(false);
    rmSync(root, { recursive: true, force: true });
    globalThis.fetch = originalFetch;
  }
});

test('successful sign-out retires the client instance and the next login starts a new generation', async () => {
  const originalFetch = globalThis.fetch;
  const requests = [];
  globalThis.fetch = async (url, options) => {
    requests.push({ url, options });
    if (String(url).endsWith('/api/auth/client-logout')) return new Response(null, { status: 204 });
    return loginFetch(url);
  };
  const h = harness();
  try {
    await h.ready();
    await h.invoke('morde:login', auth.origin, auth.accountName, 'password');
    const first = requests.find((request) => String(request.url).endsWith('/api/auth/client-login'));
    const firstBody = JSON.parse(first.options.body);
    await h.invoke('morde:logout');
    assert.equal(existsSync(join(h.userData, 'pending-revocations.json')), false);
    const fence = JSON.parse(readFileSync(join(h.userData, 'client-login-attempts.json'), 'utf8'))[createHash('sha256').update(`${auth.origin}\0${auth.accountName}`).digest('hex')];
    assert.equal(fence.retiredClientInstanceIDs.includes(firstBody.client_instance_id), true);
    assert.notEqual(fence.clientInstanceID, firstBody.client_instance_id);
    assert.equal(fence.installationID, null);
    assert.equal(fence.allowLegacyRenewal, false);
    await h.invoke('morde:login', auth.origin, auth.accountName, 'password');
    const loginRequests = requests.filter((request) => String(request.url).endsWith('/api/auth/client-login'));
    const secondBody = JSON.parse(loginRequests[1].options.body);
    assert.notEqual(secondBody.client_instance_id, firstBody.client_instance_id);
    assert.equal(secondBody.attempt_generation, 1);
    assert.equal('renewal_installation_id' in secondBody, false);
    assert.equal(loginRequests[0].options.headers['Idempotency-Key'] === loginRequests[1].options.headers['Idempotency-Key'], false);
  } finally { globalThis.fetch = originalFetch; h.cleanup(); }
});

test('closing a window after login does not touch its destroyed Web view', async () => {
  const originalFetch = globalThis.fetch;
  globalThis.fetch = async (url) => loginFetch(url);
  const h = harness();
  try {
    await h.ready();
    await h.invoke('morde:login', auth.origin, 'user', 'password');
    h.windows[0].contentView.removeChildView = () => { throw new Error('Object has been destroyed'); };
    h.views[0].webContents.closed = true;
    h.views[0].webContents.close = () => { throw new Error('Object has been destroyed'); };
    assert.doesNotThrow(() => h.windows[0].close());
  } finally { globalThis.fetch = originalFetch; h.cleanup(); }
});

test('Host popups are limited to the same-origin entry and Admin opens an independent browser', async () => {
  const originalFetch = globalThis.fetch;
  globalThis.fetch = async (url) => loginFetch(url);
  const h = harness();
  try {
    await h.ready();
    await h.invoke('morde:login', auth.origin, 'user', 'password');
    const open = h.views[0].webContents.open;
    assert.equal(open({ url: 'https://evil.example/' }).action, 'deny');
    assert.equal(open({ url: 'https://portal.example/client-authorize' }).action, 'deny');
    assert.equal(open({ url: 'https://portal.example/host-entry/h1234567?target=desktop' }).action, 'allow');
    assert.equal(open({ url: 'https://portal.example/host-entry/h1234567?target=invalid' }).action, 'deny');
    assert.equal(open({ url: 'https://portal.example/admin/' }).action, 'deny');
    let blocked = false;
    h.views[0].webContents.events.get('will-navigate')({ preventDefault() { blocked = true; } }, 'https://portal.example/admin/');
    assert.equal(blocked, true);
    assert.deepEqual(h.shellURLs, ['https://portal.example/admin/', 'https://portal.example/admin/']);
  } finally { globalThis.fetch = originalFetch; h.cleanup(); }
});

test('trusted Web downloads show a save dialog and clipboard access stays scoped', async () => {
  const originalFetch = globalThis.fetch;
  globalThis.fetch = async (url) => loginFetch(url);
  const h = harness();
  try {
    await h.ready();
    await h.invoke('morde:login', auth.origin, 'user', 'password');
    const partition = h.partitions[0];
    const download = partition.events.get('will-download');
    let saveOptions, cancelled = false;
    download({}, { setSaveDialogOptions: (options) => { saveOptions = options; } }, h.views[0].webContents);
    assert.deepEqual(saveOptions, { title: '保存 Morde 文件' });
    const foreign = { getURL: () => 'https://evil.example/' };
    download({}, { cancel: () => { cancelled = true; } }, foreign);
    assert.equal(cancelled, true);
    let allowed;
    partition.permissionRequest(h.views[0].webContents, 'clipboard-sanitized-write', (value) => { allowed = value; });
    assert.equal(allowed, true);
    partition.permissionRequest(foreign, 'clipboard-sanitized-write', (value) => { allowed = value; });
    assert.equal(allowed, false);
    partition.permissionRequest(h.views[0].webContents, 'clipboard-read', (value) => { allowed = value; });
    assert.equal(allowed, false);
  } finally { globalThis.fetch = originalFetch; h.cleanup(); }
});

test('public registration returns to the local login when its SPA link changes route', async () => {
  const h = harness();
  try {
    await h.ready();
    await h.invoke('morde:public-page', 'register', 'https://portal.example');
    const register = h.windows.at(-1);
    assert.equal(register.webContents.url, 'https://portal.example/register');
    register.webContents.events.get('did-navigate-in-page')({}, 'https://portal.example/login');
    assert.equal(register.isDestroyed(), true);
    assert.equal(h.windows[0].isDestroyed(), false);
  } finally { h.cleanup(); }
});

test('remote Web and public page reject cross-origin HTTP redirects', async () => {
  const originalFetch = globalThis.fetch;
  globalThis.fetch = async (url) => loginFetch(url);
  const h = harness();
  try {
    await h.ready();
    await h.invoke('morde:login', auth.origin, 'user', 'password');
    let blocked = false;
    h.views[0].webContents.events.get('will-redirect')({ preventDefault() { blocked = true; } }, 'https://evil.example/login');
    assert.equal(blocked, true);
    await h.invoke('morde:public-page', 'download');
    blocked = false;
    h.windows.at(-1).webContents.events.get('will-redirect')({ preventDefault() { blocked = true; } }, 'https://evil.example/download');
    assert.equal(blocked, true);
  } finally { globalThis.fetch = originalFetch; h.cleanup(); }
});

test('Host window checks a one-time ticket against the Host origin before navigation', async () => {
  const originalFetch = globalThis.fetch;
  globalThis.fetch = async (url) => loginFetch(url, url.endsWith('/api/hosts/h1234567') ? { url: 'https://host.example/' } : loginResponse);
  const h = harness();
  try {
    await h.ready();
    await h.invoke('morde:login', auth.origin, 'user', 'password');
    const popup = h.views[0].webContents.open({ url: 'https://portal.example/host-entry/h1234567' });
    const guest = new h.views[0].webContents.constructor();
    const contents = popup.createWindow({ webContents: guest, webPreferences: { sandbox: true } });
    const win = h.windows.at(-1);
    assert.equal(contents, win.webContents);
    assert.equal(contents, guest);
    assert.equal(win.options.webPreferences.sandbox, true);
    assert.equal(win.options.webPreferences.preload, undefined);
    let blocked = false;
    win.webContents.events.get('will-navigate')({ preventDefault() { blocked = true; } }, 'https://evil.example/__ch/auth?t=secret');
    assert.equal(blocked, true);
    blocked = false;
    win.webContents.events.get('will-navigate')({ preventDefault() { blocked = true; } }, 'https://host.example/__ch/auth?t=secret');
    assert.equal(blocked, true);
    await new Promise((resolve) => setImmediate(resolve));
    assert.equal(win.webContents.url, 'https://host.example/__ch/auth?t=secret');
    blocked = false;
    win.webContents.events.get('will-navigate')({ preventDefault() { blocked = true; } }, 'https://host.example/__ch/wb/');
    assert.equal(blocked, false);
    const download = h.partitions[0].events.get('will-download');
    let saveOptions, cancelled = false;
    download({}, { setSaveDialogOptions: (options) => { saveOptions = options; } }, win.webContents);
    assert.deepEqual(saveOptions, { title: '保存 Morde 文件' });
    win.close();
    download({}, { cancel: () => { cancelled = true; } }, win.webContents);
    assert.equal(cancelled, true);
  } finally { globalThis.fetch = originalFetch; h.cleanup(); }
});

test('shared Host windows recheck the current shared Host origin before navigation', async () => {
  const originalFetch = globalThis.fetch;
  const calls = [];
  globalThis.fetch = async (url, options) => {
    calls.push(String(url));
    if (String(url).endsWith('/api/shared-hosts')) return json({ items: [{ id: 'h1234567', url: 'https://shared-host.example/' }] });
    return loginFetch(url);
  };
  const h = harness();
  try {
    await h.ready();
    await h.invoke('morde:login', auth.origin, 'user', 'password');
    const popup = h.views[0].webContents.open({ url: 'https://portal.example/host-entry/h1234567?target=desktop' });
    const guest = new h.views[0].webContents.constructor();
    popup.createWindow({ webContents: guest });
    const win = h.windows.at(-1);
    let blocked = false;
    win.webContents.events.get('will-navigate')({ preventDefault() { blocked = true; } }, 'https://shared-host.example/__ch/auth?t=secret');
    await new Promise((resolve) => setImmediate(resolve));

    assert.equal(blocked, true);
    assert.equal(win.webContents.url, 'https://shared-host.example/__ch/auth?t=secret');
    assert.equal(calls.some((url) => url.endsWith('/api/shared-hosts')), true);
    assert.equal(calls.some((url) => url.endsWith('/api/hosts/h1234567')), false);
  } finally { globalThis.fetch = originalFetch; h.cleanup(); }
});

test('revoking a session closes its open Host windows', async () => {
  const originalFetch = globalThis.fetch;
  globalThis.fetch = async (url) => String(url).endsWith('/api/auth/client-logout') ? new Response(null, { status: 204 }) : loginFetch(url);
  const h = harness();
  try {
    await h.ready();
    await h.invoke('morde:login', auth.origin, 'user', 'password');
    const popup = h.views[0].webContents.open({ url: 'https://portal.example/host-entry/h1234567' });
    popup.createWindow({});
    const hostWindow = h.windows.at(-1);
    assert.equal(hostWindow.isDestroyed(), false);

    await h.handlers.get('morde:logout')({ sender: h.windows[0].webContents });

    assert.equal(hostWindow.isDestroyed(), true);
  } finally { globalThis.fetch = originalFetch; h.cleanup(); }
});

test('Host entry failure returns to the usable main Web window', async () => {
  const originalFetch = globalThis.fetch;
  globalThis.fetch = async (url) => loginFetch(url);
  const h = harness();
  try {
    await h.ready();
    await h.invoke('morde:login', auth.origin, 'user', 'password');
    const popup = h.views[0].webContents.open({ url: 'https://portal.example/host-entry/h1234567' });
    popup.createWindow({});
    const win = h.windows.at(-1);
    h.windows[0].focused = false;
    win.webContents.events.get('did-navigate-in-page')({}, 'https://portal.example/hosts');
    assert.equal(win.isDestroyed(), true);
    assert.equal(h.windows[0].focused, true);
    assert.equal(h.windows[0].contentView.attached[0], h.views[0]);
  } finally { globalThis.fetch = originalFetch; h.cleanup(); }
});

test('restart renews the Web session, while logout preserves local folder mapping and recovery point', async () => {
  const originalFetch = globalThis.fetch;
  const calls = [];
  globalThis.fetch = async (url, options) => {
    calls.push({ url, options });
    return url.endsWith('client-logout') ? new Response(null, { status: 204 }) : new Response(JSON.stringify({ web_session_credential: 'renewed-web', web_session_expires_at: future }), { headers: { 'content-type': 'application/json' } });
  };
  const h = harness(auth);
  const mappingsFile = profileFile(h, auth, 'mappings');
  const checkpointFile = profileFile(h, auth, 'sync-checkpoint');
  const legacyMappings = join(h.userData, 'mappings.json');
  const legacyCheckpoint = join(h.userData, 'sync-checkpoint.json');
  writeFileSync(join(h.userData, 'installation.json'), JSON.stringify({ origin: auth.origin, serviceInstanceID: auth.serviceInstanceID, name: auth.accountName, id: auth.installationId }));
  writeFileSync(mappingsFile, JSON.stringify([{ workspaceId: 'w1234567', directory: '/tmp/example', dev: '1', ino: '2' }]));
  writeFileSync(checkpointFile, JSON.stringify({ serviceInstanceID: auth.serviceInstanceID, installationId: auth.installationId, workspaceId: 'w1234567', directory: '/tmp/example', dev: '1', ino: '2', mode: 'upload', idempotencyKey: 'same-key' }));
  writeFileSync(legacyMappings, JSON.stringify([{ workspaceId: 'w7654321', directory: '/tmp/legacy', dev: '1', ino: '2' }]));
  writeFileSync(legacyCheckpoint, JSON.stringify({ installationId: auth.installationId, workspaceId: 'w7654321', directory: '/tmp/legacy', dev: '1', ino: '2', mode: 'upload', idempotencyKey: 'legacy-key' }));
  try {
    await h.ready();
    assert.deepEqual((await h.invoke('morde:state')).mappings[0], { workspaceId: 'w1234567', directoryName: 'example' });
    assert.equal(existsSync(mappingsFile), true);
    assert.equal(existsSync(checkpointFile), true);
    assert.deepEqual(JSON.parse(readFileSync(legacyMappings, 'utf8'))[0], { workspaceId: 'w7654321', directory: '/tmp/legacy', dev: '1', ino: '2' });
    assert.equal(JSON.parse(readFileSync(legacyCheckpoint, 'utf8')).idempotencyKey, 'legacy-key');
    assert.equal(h.partitions[0].cookies.values[0].value, 'renewed-web');
    assert.equal(calls[0].url, 'https://portal.example/api/auth/client-web-session');
    await h.invoke('morde:logout');
    assert.equal(existsSync(join(h.userData, 'credentials.bin')), false);
    assert.equal(existsSync(mappingsFile), true);
    assert.equal(existsSync(checkpointFile), true);
    assert.equal(existsSync(legacyMappings), true);
    assert.equal(existsSync(legacyCheckpoint), true);
    assert.equal(h.partitions[0].cleared, true);
    assert.equal(h.views[0].webContents.closed, true);
    assert.equal(calls.at(-1).url, 'https://portal.example/api/auth/client-logout');
  } finally { globalThis.fetch = originalFetch; h.cleanup(); }
});

test('offline logout clears session runtime, records a non-secret revoke identity and preserves local data', async () => {
  const originalFetch = globalThis.fetch;
  const clientInstanceID = '123e4567-e89b-42d3-a456-426614174000';
  globalThis.fetch = async (url) => {
    if (String(url).endsWith('/api/auth/client-web-session')) return json({ web_session_credential: 'renewed-web', web_session_expires_at: future });
    if (String(url).endsWith('/api/auth/client-logout')) {
      assert.equal(existsSync(join(h.userData, 'credentials.bin')), false);
      assert.equal(existsSync(join(h.userData, 'installation.json')), false);
      throw new TypeError('network offline');
    }
    return loginFetch(url);
  };
  const h = harness(auth);
  const mappingsFile = profileFile(h, auth, 'mappings');
  const checkpointFile = profileFile(h, auth, 'sync-checkpoint');
  const workspaceDir = join(h.root, 'workspace-data');
  const workspaceFile = join(workspaceDir, 'notes.txt');
  writeFileSync(join(h.userData, 'installation.json'), JSON.stringify({ origin: auth.origin, serviceInstanceID: auth.serviceInstanceID, name: auth.accountName, id: auth.installationId }));
  writeFileSync(join(h.userData, 'client-login-attempts.json'), JSON.stringify({
    [createHash('sha256').update(`${auth.origin}\0${auth.accountName}`).digest('hex')]: {
      clientInstanceID, attemptGeneration: 1, retiredClientInstanceIDs: [], installationID: auth.installationId, allowLegacyRenewal: false,
    },
  }));
  require('node:fs').mkdirSync(workspaceDir, { recursive: true });
  writeFileSync(workspaceFile, 'keep this workspace data');
  writeFileSync(mappingsFile, JSON.stringify([{ workspaceId: 'w1234567', directory: workspaceDir, dev: '1', ino: '2' }]));
  writeFileSync(checkpointFile, JSON.stringify({ serviceInstanceID: auth.serviceInstanceID, installationId: auth.installationId, workspaceId: 'w1234567', directory: workspaceDir, dev: '1', ino: '2', mode: 'upload', idempotencyKey: 'offline-revoke-checkpoint' }));
  try {
    await h.ready();
    assert.equal((await h.invoke('morde:state')).syncCheckpoint.mode, 'upload');
    const warning = await h.invoke('morde:logout');
    assert.match(warning, /服务器撤销尚未确认/);
    assert.match(warning, /撤销标识已保存在本机/);
    const revoked = JSON.parse(readFileSync(join(h.userData, 'pending-revocations.json'), 'utf8'))[0];
    assert.deepEqual({ origin: revoked.origin, serviceInstanceID: revoked.serviceInstanceID, installationID: revoked.installationID, clientInstanceID: revoked.clientInstanceID }, {
      origin: auth.origin, serviceInstanceID: auth.serviceInstanceID, installationID: auth.installationId, clientInstanceID,
    });
    assert.equal(JSON.stringify(revoked).includes(auth.clientCredential), false);
    assert.equal((await h.invoke('morde:state')).authenticated, false);
    assert.deepEqual((await h.invoke('morde:state')).mappings, []);
    assert.equal((await h.invoke('morde:state')).syncCheckpoint, null);
    assert.equal(existsSync(join(h.userData, 'credentials.bin')), false);
    assert.equal(existsSync(mappingsFile), true);
    assert.equal(existsSync(checkpointFile), true);
    assert.equal(readFileSync(workspaceFile, 'utf8'), 'keep this workspace data');
  } finally { globalThis.fetch = originalFetch; h.cleanup(); }
});

test('scheduled Web session refresh keeps the desktop running during a network outage', async () => {
  const originalFetch = globalThis.fetch;
  const unhandled = [];
  const onUnhandled = (error) => unhandled.push(error);
  process.on('unhandledRejection', onUnhandled);
  let requests = 0;
  globalThis.fetch = async () => {
    requests++;
    if (requests > 1) throw new TypeError('network offline');
    return new Response(JSON.stringify({ web_session_credential: 'first-web', web_session_expires_at: new Date(Date.now() + 61_000).toISOString() }), { headers: { 'content-type': 'application/json' } });
  };
  const h = harness(auth);
  try {
    await h.ready();
    await new Promise((resolve) => setTimeout(resolve, 1600));
    assert.equal(requests, 2);
    assert.deepEqual(unhandled, []);
    assert.equal((await h.invoke('morde:state')).authenticated, true);
    assert.equal(h.partitions[0].cookies.values[0].value, 'first-web');
  } finally {
    h.windows[0].close();
    process.off('unhandledRejection', onUnhandled);
    globalThis.fetch = originalFetch;
    h.cleanup();
  }
});

test('switching accounts or services cannot reuse another profile’s local Workspace mapping', async () => {
  const originalFetch = globalThis.fetch;
  globalThis.fetch = async (url) => url.endsWith('client-logout')
    ? new Response(null, { status: 204 })
    : loginFetch(url, url.endsWith('client-web-session')
      ? { web_session_credential: 'renewed-web', web_session_expires_at: future }
      : loginResponse);
  const h = harness(auth);
  const mappingsFile = profileFile(h, auth, 'mappings');
  const checkpointFile = profileFile(h, auth, 'sync-checkpoint');
  writeFileSync(mappingsFile, JSON.stringify([{ workspaceId: 'w1234567', directory: '/tmp/owner-one', dev: '1', ino: '2' }]));
  writeFileSync(checkpointFile, JSON.stringify({ serviceInstanceID: auth.serviceInstanceID, installationId: auth.installationId, workspaceId: 'w1234567', directory: '/tmp/owner-one', dev: '1', ino: '2', mode: 'upload', idempotencyKey: 'same-key' }));
  try {
    await h.ready();
    assert.equal((await h.invoke('morde:state')).mappings[0].directoryName, 'owner-one');
    await h.invoke('morde:logout');
    assert.deepEqual((await h.invoke('morde:state')).mappings, []);
    await h.invoke('morde:login', 'https://another.example', 'other', 'password');
    assert.deepEqual((await h.invoke('morde:state')).mappings, []);
    assert.equal((await h.invoke('morde:state')).syncCheckpoint, null);
    assert.equal(existsSync(mappingsFile), true);
    assert.equal(existsSync(checkpointFile), true);
    await h.invoke('morde:logout');
    await h.invoke('morde:login', auth.origin, auth.accountName, 'password');
    assert.equal((await h.invoke('morde:state')).mappings[0].directoryName, 'owner-one');
    assert.equal((await h.invoke('morde:state')).syncCheckpoint.workspaceId, 'w1234567');
  } finally { globalThis.fetch = originalFetch; h.cleanup(); }
});

test('service instance and installation changes isolate mappings and Workspace checkpoints', async () => {
  const originalFetch = globalThis.fetch;
  const serviceB = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa';
  let activeService = serviceInstanceID;
  let activeInstallation = auth.installationId;
  globalThis.fetch = async (url) => {
    const requestURL = String(url);
    if (requestURL.endsWith('/api/capabilities')) return json({ ...capabilitiesResponse, service_instance_id: activeService });
    if (requestURL.endsWith('/api/auth/client-logout')) return new Response(null, { status: 204 });
    if (requestURL.endsWith('/api/auth/client-login')) return json({ ...loginResponse, installation_id: activeInstallation });
    return loginFetch(url, requestURL.endsWith('/api/auth/client-web-session')
      ? { web_session_credential: 'renewed-web', web_session_expires_at: future } : loginResponse);
  };
  const h = harness();
  const identityA = { ...auth };
  const mappingA = profileFile(h, identityA, 'mappings');
  const checkpointA = profileFile(h, identityA, 'sync-checkpoint');
  try {
    await h.ready();
    await h.invoke('morde:login', auth.origin, auth.accountName, 'password');
    writeFileSync(mappingA, JSON.stringify([{ workspaceId: 'w1234567', directory: '/tmp/service-a-install-1', dev: '1', ino: '2' }]));
    writeFileSync(checkpointA, JSON.stringify({ serviceInstanceID, installationId: auth.installationId, workspaceId: 'w1234567', directory: '/tmp/service-a-install-1', dev: '1', ino: '2', mode: 'upload', idempotencyKey: 'service-a-install-1' }));
    await h.invoke('morde:logout');
    assert.equal(existsSync(checkpointA), true);

    activeService = serviceB;
    await h.invoke('morde:login', auth.origin, auth.accountName, 'password');
    assert.deepEqual((await h.invoke('morde:state')).mappings, []);
    assert.equal((await h.invoke('morde:state')).syncCheckpoint, null);
    const serviceBFiles = profileFile(h, { ...auth, serviceInstanceID: serviceB }, 'mappings');
    assert.notEqual(serviceBFiles, mappingA);
    assert.equal(existsSync(serviceBFiles), false);
    assert.equal(existsSync(mappingA), true);
    await h.invoke('morde:logout');
    assert.equal(existsSync(checkpointA), true);

    activeService = serviceInstanceID;
    activeInstallation = 'i2222222';
    await h.invoke('morde:login', auth.origin, auth.accountName, 'password');
    assert.deepEqual((await h.invoke('morde:state')).mappings, []);
    assert.equal((await h.invoke('morde:state')).syncCheckpoint, null);
    const install2Files = profileFile(h, { ...auth, installationId: activeInstallation }, 'mappings');
    assert.notEqual(install2Files, mappingA);
    assert.equal(existsSync(install2Files), false);
    await h.invoke('morde:logout');
    assert.equal(existsSync(checkpointA), true);

    activeInstallation = auth.installationId;
    await h.invoke('morde:login', auth.origin, auth.accountName, 'password');
    assert.equal(existsSync(checkpointA), true);
    const restored = await h.invoke('morde:state');
    assert.equal(restored.profileError, null);
    assert.equal(restored.mappings[0].directoryName, 'service-a-install-1');
    assert.equal(restored.syncCheckpoint.workspaceId, 'w1234567');
    assert.equal(JSON.parse(readFileSync(checkpointA, 'utf8')).idempotencyKey, 'service-a-install-1');
    assert.equal(existsSync(mappingA), true);
    assert.equal(existsSync(checkpointA), true);
  } finally { globalThis.fetch = originalFetch; h.cleanup(); }
});

test('damaged sync recovery point preserves the file and blocks new local operations', async () => {
  const originalFetch = globalThis.fetch;
  globalThis.fetch = async () => new Response(JSON.stringify({ web_session_credential: 'renewed-web', web_session_expires_at: future }), { headers: { 'content-type': 'application/json' } });
  const h = harness(auth);
  const checkpointFile = profileFile(h, auth, 'sync-checkpoint');
  writeFileSync(profileFile(h, auth, 'mappings'), JSON.stringify([{ workspaceId: 'w1234567', directory: '/tmp/example', dev: '1', ino: '2' }]));
  writeFileSync(checkpointFile, '{damaged');
  try {
    await h.ready();
    const state = await h.invoke('morde:state');
    assert.equal(state.authenticated, true);
    assert.match(state.profileError, /同步恢复点无法读取/);
    assert.equal(state.profileError.includes(h.userData), false);
    assert.equal(state.syncRecovery.state, 'unknown');
    await assert.rejects(h.invoke('morde:choose-directory', 'w1234567'), /同步已暂停/);
    await assert.rejects(h.invoke('morde:sync', 'w1234567', 'upload'), /同步已暂停/);
    await assert.rejects(h.invoke('morde:sync-discard'), /同步已暂停/);
    assert.equal(readFileSync(checkpointFile, 'utf8'), '{damaged');
  } finally { globalThis.fetch = originalFetch; h.cleanup(); }
});

test('invalid local Workspace mapping is preserved instead of being replaced on folder selection', async () => {
  const originalFetch = globalThis.fetch;
  globalThis.fetch = async () => new Response(JSON.stringify({ web_session_credential: 'renewed-web', web_session_expires_at: future }), { headers: { 'content-type': 'application/json' } });
  const h = harness(auth);
  const mappingsFile = profileFile(h, auth, 'mappings');
  const damaged = JSON.stringify([{ workspaceId: 'w1234567', dev: '1', ino: '2' }]);
  writeFileSync(mappingsFile, damaged);
  try {
    await h.ready();
    assert.match((await h.invoke('morde:state')).profileError, /同步配置无法读取/);
    await assert.rejects(h.invoke('morde:choose-directory', 'w1234567'), /同步已暂停/);
    assert.equal(readFileSync(mappingsFile, 'utf8'), damaged);
  } finally { globalThis.fetch = originalFetch; h.cleanup(); }
});

test('duplicate Workspace mappings are preserved and pause local sync instead of selecting an arbitrary folder', async () => {
  const originalFetch = globalThis.fetch;
  globalThis.fetch = async () => new Response(JSON.stringify({ web_session_credential: 'renewed-web', web_session_expires_at: future }), { headers: { 'content-type': 'application/json' } });
  const h = harness(auth);
  const mappingsFile = profileFile(h, auth, 'mappings');
  const damaged = JSON.stringify([
    { workspaceId: 'w1234567', directory: '/tmp/first', dev: '1', ino: '2' },
    { workspaceId: 'w1234567', directory: '/tmp/second', dev: '3', ino: '4' },
  ]);
  writeFileSync(mappingsFile, damaged);
  try {
    await h.ready();
    const state = await h.invoke('morde:state');
    assert.match(state.profileError, /同步配置无法读取/);
    assert.deepEqual(state.mappings, []);
    await assert.rejects(h.invoke('morde:choose-directory', 'w1234567'), /同步已暂停/);
    assert.equal(readFileSync(mappingsFile, 'utf8'), damaged);
  } finally { globalThis.fetch = originalFetch; h.cleanup(); }
});

test('closing and reopening the macOS window restores the authenticated Web view', async () => {
  const originalFetch = globalThis.fetch;
  globalThis.fetch = async () => new Response(JSON.stringify({ web_session_credential: 'renewed-web', web_session_expires_at: future }), { headers: { 'content-type': 'application/json' } });
  const h = harness(auth);
  try {
    await h.ready();
    h.windows[0].close();
    h.appEvents.get('activate')();
    await new Promise((resolve) => setImmediate(resolve));
    assert.equal(h.windows.length, 2);
    assert.equal(h.views.length, 2);
    assert.equal(h.windows[1].contentView.attached[0], h.views[1]);
    assert.equal((await h.invoke('morde:state')).authenticated, true);
  } finally { globalThis.fetch = originalFetch; h.cleanup(); }
});

test('completed server session with a file conflict keeps the desktop recovery point', async () => {
  const originalFetch = globalThis.fetch;
  const Sync = require('../out/morde-sync.js').MordeSync;
  const originalRun = Sync.prototype.run;
  globalThis.fetch = async (url) => new Response(JSON.stringify(String(url).endsWith('/api/capabilities')
    ? capabilitiesWithWorkspaceSync
    : String(url).endsWith('/api/workspaces/w1234567') ? { state: 'ready' }
      : { web_session_credential: 'renewed-web', web_session_expires_at: future }), { headers: { 'content-type': 'application/json' } });
  Sync.prototype.run = async (_auth, mapping) => ({ session: { id: 'p1234567', workspace_id: mapping.workspaceId, state: 'completed', target_revision: 1 }, conflicts: [{ path: 'shared.txt' }] });
  const h = harness(auth);
  const checkpointFile = profileFile(h, auth, 'sync-checkpoint');
  writeFileSync(profileFile(h, auth, 'mappings'), JSON.stringify([{ workspaceId: 'w1234567', directory: '/tmp/example', dev: '1', ino: '2' }]));
  try {
    await h.ready();
    const result = await h.invoke('morde:sync', 'w1234567', 'live_sync');
    assert.equal(result.conflicts[0].path, 'shared.txt');
    assert.equal((await h.invoke('morde:state')).syncCheckpoint.mode, 'live_sync');
    assert.equal(existsSync(checkpointFile), true);
  } finally { Sync.prototype.run = originalRun; globalThis.fetch = originalFetch; h.cleanup(); }
});

test('sync IPC scopes progress to its Workspace and strips paths, credentials and result extras', async () => {
  const originalFetch = globalThis.fetch;
  const Sync = require('../out/morde-sync.js').MordeSync;
  const originalRun = Sync.prototype.run;
  globalThis.fetch = async (url) => json(String(url).endsWith('/api/workspaces/w1234567')
    ? { state: 'ready' } : { web_session_credential: 'renewed-web', web_session_expires_at: future });
  Sync.prototype.run = async function (_auth, mapping) {
    this.update('running', { stage: 'transferring', confirmed_bytes: 8, planned_bytes: 16, session_id: 'p1234567', current_path: '/private/workspace/secret.txt', bearer_token: 'sync-secret', file_content: 'private contents' });
    return { session: { id: 'p1234567', workspace_id: mapping.workspaceId, state: 'conflict', bearer_token: 'sync-secret', file_content: 'private contents' },
      conflicts: [{ path: 'shared.txt', reason: 'content_changed', absolute_path: '/private/workspace/shared.txt', file_content: 'private contents' }] };
  };
  const h = harness(auth);
  writeFileSync(profileFile(h, auth, 'mappings'), JSON.stringify([{ workspaceId: 'w1234567', directory: '/private/workspace', dev: '1', ino: '2' }]));
  try {
    await h.ready();
    const result = await h.invoke('morde:sync', 'w1234567', 'bidirectional', ['src']);
    assert.deepEqual(result, { session: { id: 'p1234567', workspace_id: 'w1234567', state: 'conflict' }, conflicts: [{ path: 'shared.txt', reason: 'content_changed' }] });
    const event = h.windows[0].webContents.messages.find(([channel, payload]) => channel === 'morde:sync-state' && payload.progress)?.[1];
    assert.equal(event.workspaceId, 'w1234567');
    assert.equal(event.mode, 'bidirectional');
    assert.deepEqual(event.progress, { stage: 'transferring', session_id: 'p1234567', confirmed_bytes: 8, planned_bytes: 16 });
    assert.equal((await h.invoke('morde:state')).syncRecovery.state, 'pending');
    const serialized = JSON.stringify({ result, event });
    for (const secret of ['/private/workspace', 'sync-secret', 'private contents', 'absolute_path']) assert.equal(serialized.includes(secret), false);
  } finally { Sync.prototype.run = originalRun; globalThis.fetch = originalFetch; h.cleanup(); }
});

test('live sync rejects a closed capability before starting the sync process', async () => {
  const originalFetch = globalThis.fetch;
  const Sync = require('../out/morde-sync.js').MordeSync;
  const originalRun = Sync.prototype.run;
  let runCalls = 0;
  Sync.prototype.run = async () => { runCalls++; return {}; };
  const workRoot = mkdtempSync(join(homedir(), 'morde-live-sync-denied-'));
  const workspaceDir = join(workRoot, 'workspace');
  mkdirSync(workspaceDir);
  const info = statSync(workspaceDir, { bigint: true });
  const h = harness(auth);
  globalThis.fetch = async (url) => String(url).endsWith('/api/capabilities')
    ? json(capabilitiesResponse)
    : json({ state: 'ready' });
  writeFileSync(profileFile(h, auth, 'mappings'), JSON.stringify([{ workspaceId: 'w1234567', directory: workspaceDir, dev: info.dev.toString(), ino: info.ino.toString() }]));
  try {
    await h.ready();
    await assert.rejects(h.invoke('morde:live-sync-start', 'w1234567', [], 30, false), /Workspace sync is not enabled/);
    assert.equal(runCalls, 0);
    const state = await h.invoke('morde:state');
    assert.equal(state.liveSyncCapability, 'disabled');
    assert.equal(state.liveSyncs.w1234567.state, 'unavailable');
    assert.equal(existsSync(liveSyncProfileFile(h, auth)), false);
  } finally {
    Sync.prototype.run = originalRun;
    globalThis.fetch = originalFetch;
    h.cleanup();
    rmSync(workRoot, { recursive: true, force: true });
  }
});

test('live sync rejects a changed local directory identity before capability checks or process startup', async () => {
  const originalFetch = globalThis.fetch;
  const Sync = require('../out/morde-sync.js').MordeSync;
  const originalRun = Sync.prototype.run;
  let runCalls = 0;
  const fetchURLs = [];
  Sync.prototype.run = async () => { runCalls++; return {}; };
  const workRoot = mkdtempSync(join(homedir(), 'morde-live-sync-identity-'));
  const workspaceDir = join(workRoot, 'workspace');
  mkdirSync(workspaceDir);
  const info = statSync(workspaceDir, { bigint: true });
  const h = harness(auth);
  globalThis.fetch = async (url) => { fetchURLs.push(String(url)); return json(capabilitiesWithWorkspaceSync); };
  writeFileSync(profileFile(h, auth, 'mappings'), JSON.stringify([{ workspaceId: 'w1234567', directory: workspaceDir,
    dev: info.dev.toString(), ino: (info.ino + 1n).toString() }]));
  try {
    await h.ready();
    await assert.rejects(h.invoke('morde:live-sync-start', 'w1234567', [], 30, false), /Folder identity changed/);
    assert.equal(runCalls, 0);
    assert.equal(fetchURLs.some((url) => url.endsWith('/api/capabilities')), false, 'changed local identity must stop before requesting sync capability');
  } finally {
    Sync.prototype.run = originalRun;
    globalThis.fetch = originalFetch;
    h.cleanup();
    rmSync(workRoot, { recursive: true, force: true });
  }
});

test('live sync stores isolated Workspace state, redacts local paths, and marks confirmed stop uncertain', async () => {
  const originalFetch = globalThis.fetch;
  const Sync = require('../out/morde-sync.js').MordeSync;
  const originalRun = Sync.prototype.run;
  let nextPid = 90000000, runCalls = 0;
  Sync.prototype.run = async function (_auth, mapping, mode, scope, _checkpoint, _stillAuthorized, _confirmMapping, rescanSeconds) {
    runCalls++;
    assert.equal(mode, 'live_sync');
    assert.equal(rescanSeconds, 30);
    this.update('running', { state: 'running', stage: 'watching', session_id: mapping.workspaceId === 'w1234567' ? 'p1234567' : 'p7654321', confirmed_revision: 7,
      current_path: `${mapping.directory}/private.txt`, bearer_token: 'sync-secret', file_content: 'private content' });
    this.started(nextPid++);
    return new Promise(() => {});
  };
  const workRoot = mkdtempSync(join(homedir(), 'morde-live-sync-state-'));
  const firstDir = join(workRoot, 'first');
  const secondDir = join(workRoot, 'second');
  const overlappingDir = join(firstDir, 'nested');
  mkdirSync(firstDir);
  mkdirSync(secondDir);
  mkdirSync(overlappingDir);
  const firstInfo = statSync(firstDir, { bigint: true });
  const secondInfo = statSync(secondDir, { bigint: true });
  const h = harness(auth);
  globalThis.fetch = async (url) => String(url).endsWith('/api/capabilities')
    ? json(capabilitiesWithWorkspaceSync)
    : String(url).includes('/api/workspaces/') ? json({ state: 'ready' }) : json({});
  writeFileSync(profileFile(h, auth, 'mappings'), JSON.stringify([
    { workspaceId: 'w1234567', directory: firstDir, dev: firstInfo.dev.toString(), ino: firstInfo.ino.toString() },
    { workspaceId: 'w7654321', directory: secondDir, dev: secondInfo.dev.toString(), ino: secondInfo.ino.toString() },
  ]));
  try {
    await h.ready();
    const first = await h.invoke('morde:live-sync-start', 'w1234567', ['src'], 30, false);
    const second = await h.invoke('morde:live-sync-start', 'w7654321', ['docs'], 30, false);
    assert.equal(first.liveSyncs.w1234567.state, 'running');
    assert.equal(first.liveSyncs.w1234567.sessionId, 'p1234567');
    assert.equal(first.liveSyncs.w1234567.lastConfirmedRevision, 7);
    assert.equal(second.liveSyncs.w7654321.state, 'running');
    assert.deepEqual(second.liveSyncs.w7654321.scope, ['docs']);
    h.dialog.showOpenDialog = async () => ({ canceled: false, filePaths: [overlappingDir] });
    await assert.rejects(h.invoke('morde:choose-directory', 'w1111111'), /overlap|Another Workspace/i);
    assert.equal(runCalls, 2);
    const serialized = JSON.stringify(second.liveSyncs);
    for (const secret of [firstDir, secondDir, 'sync-secret', 'private content', 'current_path']) assert.equal(serialized.includes(secret), false);
    assert.equal(existsSync(liveSyncProfileFile(h, auth)), true);
    await h.invoke('morde:live-sync-stop', 'w1234567');
    const stopped = await h.invoke('morde:state');
    assert.equal(stopped.liveSyncs.w1234567.state, 'persisted_uncertain');
    assert.equal(stopped.liveSyncs.w1234567.recoveryRequired, true);
    assert.equal(stopped.liveSyncs.w7654321.state, 'running');
    await assert.rejects(h.invoke('morde:live-sync-start', 'w1234567', ['src'], 30, false), /Confirm recovery/);
    assert.equal(runCalls, 2);

    const retainedRoot = h.root;
    h.cleanup(false);
    const restarted = harness(auth, retainedRoot);
    try {
      await restarted.ready();
      const recovered = await restarted.invoke('morde:state');
      assert.equal(recovered.liveSyncs.w1234567.state, 'persisted_uncertain');
      assert.equal(recovered.liveSyncs.w1234567.recoveryRequired, true);
      assert.equal(recovered.liveSyncs.w7654321.state, 'persisted_uncertain');
      await assert.rejects(restarted.invoke('morde:live-sync-start', 'w1234567', ['src'], 30, false), /Confirm recovery/);
      assert.equal(runCalls, 2, 'restart must not spawn or replay live sync before explicit recovery confirmation');
    } finally { restarted.cleanup(); }
  } finally {
    Sync.prototype.run = originalRun;
    globalThis.fetch = originalFetch;
    h.cleanup();
    rmSync(workRoot, { recursive: true, force: true });
  }
});

test('live sync preserves conflicts and refuses automatic restart', async () => {
  const originalFetch = globalThis.fetch;
  const Sync = require('../out/morde-sync.js').MordeSync;
  const originalRun = Sync.prototype.run;
  let runCalls = 0;
  Sync.prototype.run = async function (_auth, mapping) {
    runCalls++;
    this.started(90123456);
    return { session: { id: 'p1234567', workspace_id: mapping.workspaceId, state: 'conflict', target_revision: 8 },
      conflicts: [{ path: 'src/shared.txt', reason: 'content_changed', absolute_path: `${mapping.directory}/src/shared.txt`, file_content: 'private content' }] };
  };
  const workRoot = mkdtempSync(join(homedir(), 'morde-live-sync-conflict-'));
  const workspaceDir = join(workRoot, 'workspace');
  mkdirSync(workspaceDir);
  const info = statSync(workspaceDir, { bigint: true });
  const h = harness(auth);
  globalThis.fetch = async (url) => String(url).endsWith('/api/capabilities')
    ? json(capabilitiesWithWorkspaceSync)
    : String(url).includes('/api/workspaces/') ? json({ state: 'ready' }) : json({});
  writeFileSync(profileFile(h, auth, 'mappings'), JSON.stringify([{ workspaceId: 'w1234567', directory: workspaceDir, dev: info.dev.toString(), ino: info.ino.toString() }]));
  try {
    await h.ready();
    await h.invoke('morde:live-sync-start', 'w1234567', ['src'], 30, false);
    let state;
    for (let attempt = 0; attempt < 20; attempt++) {
      state = await h.invoke('morde:state');
      if (state.liveSyncs.w1234567.state === 'conflict') break;
      await new Promise((resolve) => setImmediate(resolve));
    }
    assert.equal(state.liveSyncs.w1234567.state, 'conflict');
    assert.equal(state.liveSyncs.w1234567.recoveryRequired, true);
    assert.deepEqual(state.liveSyncs.w1234567.conflicts, [{ path: 'src/shared.txt', reason: 'content_changed' }]);
    assert.equal(JSON.stringify(state.liveSyncs).includes(workspaceDir), false);
    await assert.rejects(h.invoke('morde:live-sync-start', 'w1234567', ['src'], 30, true), /conflicts/);
    assert.equal(runCalls, 1);
  } finally {
    Sync.prototype.run = originalRun;
    globalThis.fetch = originalFetch;
    h.cleanup();
    rmSync(workRoot, { recursive: true, force: true });
  }
});

test('web-view connection failures never forward raw errors on the sync-state IPC channel', async () => {
  const originalFetch = globalThis.fetch;
  const h = harness(auth);
  try {
    await h.ready();
    h.windows.at(-1).close();
    globalThis.fetch = async () => { throw new Error('/private/workspace sync-secret private file contents'); };
    h.appEvents.get('second-instance')();
    await new Promise((resolve) => setImmediate(resolve));
    await new Promise((resolve) => setImmediate(resolve));
    const event = h.windows.at(-1).webContents.messages.find(([channel, payload]) => channel === 'morde:sync-state' && payload.state === 'offline')?.[1];
    assert.equal(event.message, 'Morde 云端页面暂时无法连接；本机同步状态未改变。');
    const serialized = JSON.stringify(event);
    for (const secret of ['/private/workspace', 'sync-secret', 'private file contents']) assert.equal(serialized.includes(secret), false);
  } finally { globalThis.fetch = originalFetch; h.cleanup(); }
});

test('failed checkpoint persistence is reported to the renderer without leaking the local data path', async () => {
  const originalFetch = globalThis.fetch;
  const fs = require('node:fs');
  const originalRename = fs.renameSync;
  globalThis.fetch = async (url) => json(String(url).endsWith('/api/workspaces/w1234567')
    ? { state: 'ready' } : { web_session_credential: 'renewed-web', web_session_expires_at: future });
  const h = harness(auth);
  writeFileSync(profileFile(h, auth, 'mappings'), JSON.stringify([{ workspaceId: 'w1234567', directory: '/private/workspace', dev: '1', ino: '2' }]));
  fs.renameSync = function (source, destination) {
    if (String(destination).includes('sync-checkpoint-')) throw new Error(`simulated local path failure: ${destination}`);
    return originalRename.call(this, source, destination);
  };
  try {
    await h.ready();
    await assert.rejects(h.invoke('morde:sync', 'w1234567', 'upload'), (error) => !error.message.includes(h.userData) && /sync was not started/.test(error.message));
    const state = await h.invoke('morde:state');
    assert.equal(state.syncCheckpoint, null);
    assert.equal(state.syncRecovery.state, 'failed');
    assert.match(state.syncRecovery.message, /同步未启动/);
    assert.equal(JSON.stringify(state).includes(h.userData), false);
  } finally { fs.renameSync = originalRename; globalThis.fetch = originalFetch; h.cleanup(); }
});

test('session checkpoint write failure marks recovery unknown and requests shutdown', async () => {
  const originalFetch = globalThis.fetch;
  const fs = require('node:fs');
  const originalRename = fs.renameSync;
  const Sync = require('../out/morde-sync.js').MordeSync;
  const originalRun = Sync.prototype.run;
  let checkpointRenames = 0;
  globalThis.fetch = async (url) => json(String(url).endsWith('/api/workspaces/w1234567')
    ? { state: 'ready' } : { web_session_credential: 'renewed-web', web_session_expires_at: future });
  Sync.prototype.run = async function () {
    this.update('running', { stage: 'transferring', session_id: 'p1234567', confirmed_bytes: 8, planned_bytes: 16 });
    throw new Error('simulated operation interrupted');
  };
  const h = harness(auth);
  const checkpointFile = profileFile(h, auth, 'sync-checkpoint');
  writeFileSync(profileFile(h, auth, 'mappings'), JSON.stringify([{ workspaceId: 'w1234567', directory: '/private/workspace', dev: '1', ino: '2' }]));
  fs.renameSync = function (source, destination) {
    if (String(destination).includes('sync-checkpoint-') && ++checkpointRenames === 2) throw new Error(`simulated local path failure: ${destination}`);
    return originalRename.call(this, source, destination);
  };
  try {
    await h.ready();
    await assert.rejects(h.invoke('morde:sync', 'w1234567', 'upload'), /simulated operation interrupted/);
    const state = await h.invoke('morde:state');
    assert.equal(state.syncRecovery.state, 'unknown');
    assert.equal(state.syncRecovery.workspaceId, 'w1234567');
    assert.match(state.syncRecovery.message, /保存状态未知/);
    const event = h.windows[0].webContents.messages.find(([, payload]) => payload?.state === 'shutdown_unknown')?.[1];
    assert.equal(event.workspaceId, 'w1234567');
    assert.equal(event.recovery.state, 'unknown');
    assert.equal(JSON.parse(readFileSync(checkpointFile, 'utf8')).sessionId, undefined);
    assert.equal(JSON.stringify({ state, event }).includes(h.userData), false);
  } finally { fs.renameSync = originalRename; Sync.prototype.run = originalRun; globalThis.fetch = originalFetch; h.cleanup(); }
});

test('failed handoff persistence exposes an unknown recovery state and keeps the checkpoint', async () => {
  const originalFetch = globalThis.fetch;
  const fs = require('node:fs');
  const originalRename = fs.renameSync;
  const Sync = require('../out/morde-sync.js').MordeSync;
  const originalRun = Sync.prototype.run;
  let handoffWrites = 0;
  globalThis.fetch = async (url) => json(String(url).endsWith('/api/workspaces/w1234567')
    ? { state: 'ready' } : { web_session_credential: 'renewed-web', web_session_expires_at: future });
  Sync.prototype.run = async () => ({ revision: 31 });
  const h = harness(auth);
  writeFileSync(profileFile(h, auth, 'mappings'), JSON.stringify([{ workspaceId: 'w1234567', directory: '/private/workspace', dev: '1', ino: '2' }]));
  fs.renameSync = function (source, destination) {
    if (String(destination).includes('handoff-') && ++handoffWrites === 2) throw new Error(`simulated local path failure: ${destination}`);
    return originalRename.call(this, source, destination);
  };
  try {
    await h.ready();
    await assert.rejects(h.invoke('morde:sync', 'w1234567', 'handoff_checkpoint', ['src'], true, 'desktop'), /local handoff record was saved/);
    const state = await h.invoke('morde:state');
    assert.equal(state.syncRecovery.state, 'unknown');
    assert.equal(state.syncRecovery.workspaceId, 'w1234567');
    assert.match(state.syncRecovery.message, /保存状态未知/);
    assert.equal(state.syncCheckpoint.mode, 'handoff_checkpoint');
    assert.equal(JSON.stringify(state).includes(h.userData), false);
  } finally { fs.renameSync = originalRename; Sync.prototype.run = originalRun; globalThis.fetch = originalFetch; h.cleanup(); }
});

test('an unshaped sync result retains its recovery point and is not returned as success', async () => {
  const originalFetch = globalThis.fetch;
  const Sync = require('../out/morde-sync.js').MordeSync;
  const originalRun = Sync.prototype.run;
  globalThis.fetch = async (url) => new Response(JSON.stringify(url.endsWith('/api/workspaces/w1234567')
    ? { state: 'ready' }
    : { web_session_credential: 'renewed-web', web_session_expires_at: future }), { headers: { 'content-type': 'application/json' } });
  Sync.prototype.run = async () => ({});
  const h = harness(auth);
  const checkpointFile = profileFile(h, auth, 'sync-checkpoint');
  writeFileSync(profileFile(h, auth, 'mappings'), JSON.stringify([{ workspaceId: 'w1234567', directory: '/tmp/example', dev: '1', ino: '2' }]));
  try {
    await h.ready();
    await assert.rejects(h.invoke('morde:sync', 'w1234567', 'upload'), /result is unknown; the recovery point was retained/);
    assert.equal((await h.invoke('morde:state')).syncCheckpoint.workspaceId, 'w1234567');
    assert.equal(existsSync(checkpointFile), true);
  } finally { Sync.prototype.run = originalRun; globalThis.fetch = originalFetch; h.cleanup(); }
});

test('invalid sync scope is rejected before creating a recovery point', async () => {
  const Sync = require('../out/morde-sync.js').MordeSync;
  const originalRun = Sync.prototype.run;
  Sync.prototype.run = async () => { throw new Error('sync child should not start'); };
  const h = harness(auth);
  const checkpointFile = profileFile(h, auth, 'sync-checkpoint');
  writeFileSync(profileFile(h, auth, 'mappings'), JSON.stringify([{ workspaceId: 'w1234567', directory: '/tmp/example', dev: '1', ino: '2' }]));
  try {
    await h.ready();
    await assert.rejects(h.invoke('morde:sync', 'w1234567', 'upload', ['../outside']), /Invalid sync scope/);
    assert.equal((await h.invoke('morde:state')).syncCheckpoint, null);
    assert.equal(existsSync(checkpointFile), false);
  } finally { Sync.prototype.run = originalRun; h.cleanup(); }
});

test('a scoped sync recovery point survives restart and only resumes with its original scope', async () => {
  const originalFetch = globalThis.fetch;
  const Sync = require('../out/morde-sync.js').MordeSync;
  const originalRun = Sync.prototype.run;
  const calls = [];
  let runCount = 0;
  globalThis.fetch = async (url) => json(String(url).endsWith('/api/workspaces/w1234567')
    ? { state: 'ready' } : { web_session_credential: 'renewed-web', web_session_expires_at: future });
  Sync.prototype.run = async (_auth, mapping, mode, scope, checkpoint) => {
    calls.push({ workspaceId: mapping.workspaceId, mode, scope: [...scope], idempotencyKey: checkpoint.idempotencyKey });
    runCount++;
    return runCount === 1 ? {} : { session: { id: 'p1234567', workspace_id: mapping.workspaceId, state: 'completed' }, conflicts: [] };
  };
  let h = harness(auth);
  const checkpointFile = profileFile(h, auth, 'sync-checkpoint');
  writeFileSync(profileFile(h, auth, 'mappings'), JSON.stringify([{ workspaceId: 'w1234567', directory: '/tmp/example', dev: '1', ino: '2' }]));
  try {
    await h.ready();
    await assert.rejects(h.invoke('morde:sync', 'w1234567', 'upload', ['src']), /result is unknown; the recovery point was retained/);
    assert.deepEqual((await h.invoke('morde:state')).syncCheckpoint.scope, ['src']);
    const root = h.root;
    h.cleanup(false);
    h = harness(auth, root);
    await h.ready();
    assert.deepEqual((await h.invoke('morde:state')).syncCheckpoint.scope, ['src']);
    await assert.rejects(h.invoke('morde:sync', 'w1234567', 'upload', []), /original scope/);
    assert.equal(calls.length, 1);
    const result = await h.invoke('morde:sync', 'w1234567', 'upload', ['src']);
    assert.equal(result.session.state, 'completed');
    assert.deepEqual(calls.map((call) => call.scope), [['src'], ['src']]);
    assert.equal(calls[0].idempotencyKey, calls[1].idempotencyKey);
    assert.equal((await h.invoke('morde:state')).syncCheckpoint, null);
    assert.equal(existsSync(checkpointFile), false);
  } finally { Sync.prototype.run = originalRun; globalThis.fetch = originalFetch; h.cleanup(); }
});

test('legacy sync checkpoints without a scope load with an empty scope', async () => {
  const h = harness(auth);
  const checkpointFile = profileFile(h, auth, 'sync-checkpoint');
  const legacy = { serviceInstanceID: auth.serviceInstanceID, installationId: auth.installationId, workspaceId: 'w1234567', directory: '/tmp/example', dev: '1', ino: '2', mode: 'upload', idempotencyKey: 'legacy-checkpoint' };
  writeFileSync(checkpointFile, JSON.stringify(legacy));
  try {
    await h.ready();
    const state = await h.invoke('morde:state');
    assert.equal(state.profileError, null);
    assert.deepEqual(state.syncCheckpoint.scope, []);
    assert.deepEqual(JSON.parse(readFileSync(checkpointFile, 'utf8')), legacy);
  } finally { h.cleanup(); }
});

test('legacy handoff checkpoints without a target recover toward the cloud Workspace', async () => {
  const originalFetch = globalThis.fetch;
  const Sync = require('../out/morde-sync.js').MordeSync;
  const originalRun = Sync.prototype.run;
  let target;
  globalThis.fetch = async (url) => json(String(url).endsWith('/api/workspaces/w1234567')
    ? { state: 'ready' } : { web_session_credential: 'renewed-web', web_session_expires_at: future });
  Sync.prototype.run = async (_auth, _mapping, _mode, _scope, checkpoint) => { target = checkpoint.handoffTarget; return { revision: 14 }; };
  const h = harness(auth);
  writeFileSync(profileFile(h, auth, 'mappings'), JSON.stringify([{ workspaceId: 'w1234567', directory: '/tmp/example', dev: '1', ino: '2' }]));
  writeFileSync(profileFile(h, auth, 'sync-checkpoint'), JSON.stringify({
    serviceInstanceID: auth.serviceInstanceID, installationId: auth.installationId, workspaceId: 'w1234567',
    directory: '/tmp/example', dev: '1', ino: '2', mode: 'handoff_checkpoint', idempotencyKey: 'legacy-handoff',
  }));
  try {
    await h.ready();
    assert.equal((await h.invoke('morde:state')).syncCheckpoint.handoffTarget, 'cloud_workspace');
    const result = await h.invoke('morde:sync', 'w1234567', 'handoff_checkpoint', [], true);
    assert.equal(result.revision, 14);
    assert.equal(target, 'cloud_workspace');
    assert.equal((await h.invoke('morde:state')).handoff.target, 'cloud_workspace');
  } finally { Sync.prototype.run = originalRun; globalThis.fetch = originalFetch; h.cleanup(); }
});

test('handoff checkpoint requires explicit writer quiescence and a verified result', async () => {
  const originalFetch = globalThis.fetch;
  const Sync = require('../out/morde-sync.js').MordeSync;
  const originalRun = Sync.prototype.run;
  const calls = [];
  globalThis.fetch = async (url) => json(String(url).endsWith('/api/workspaces/w1234567')
    ? { state: 'ready' } : { web_session_credential: 'renewed-web', web_session_expires_at: future });
  Sync.prototype.run = async (_auth, mapping, mode, scope, checkpoint) => { calls.push({ workspaceId: mapping.workspaceId, mode, scope, handoffTarget: checkpoint.handoffTarget }); return { revision: 12 }; };
  const h = harness(auth);
  const checkpointFile = profileFile(h, auth, 'sync-checkpoint');
  const handoffFile = profileFile(h, auth, 'handoff');
  writeFileSync(profileFile(h, auth, 'mappings'), JSON.stringify([{ workspaceId: 'w1234567', directory: '/tmp/example', dev: '1', ino: '2' }]));
  try {
    await h.ready();
    await assert.rejects(h.invoke('morde:sync', 'w1234567', 'handoff_checkpoint', [], false), /Confirm that Workspace writers are paused/);
    assert.equal(existsSync(checkpointFile), false);
    const result = await h.invoke('morde:sync', 'w1234567', 'handoff_checkpoint', ['src'], true, 'desktop');
    assert.equal(result.revision, 12);
    assert.deepEqual(calls, [{ workspaceId: 'w1234567', mode: 'handoff_checkpoint', scope: ['src'], handoffTarget: 'desktop' }]);
    assert.equal((await h.invoke('morde:state')).syncCheckpoint, null);
    assert.deepEqual((await h.invoke('morde:state')).handoff, {
      workspaceId: 'w1234567', scope: ['src'], source: 'cloud_workspace', target: 'desktop', phase: 'verified', revision: 12,
    });
    assert.equal('id' in (await h.invoke('morde:state')).handoff, false);
    assert.equal(JSON.parse(readFileSync(handoffFile, 'utf8')).phase, 'verified');
  } finally { Sync.prototype.run = originalRun; globalThis.fetch = originalFetch; h.cleanup(); }
});

test('unknown handoff survives restart and only resumes with its original scope and target', async () => {
  const originalFetch = globalThis.fetch;
  const Sync = require('../out/morde-sync.js').MordeSync;
  const originalRun = Sync.prototype.run;
  const root = mkdtempSync(join(tmpdir(), 'morde-desktop-handoff-recovery-'));
  const calls = [];
  let first, second;
  globalThis.fetch = async (url) => json(String(url).endsWith('/api/workspaces/w1234567')
    ? { state: 'ready' } : { web_session_credential: 'renewed-web', web_session_expires_at: future });
  Sync.prototype.run = async (_auth, mapping, mode, scope, checkpoint) => {
    calls.push({ workspaceId: mapping.workspaceId, mode, scope: [...scope], handoffTarget: checkpoint.handoffTarget, idempotencyKey: checkpoint.idempotencyKey });
    if (calls.length === 1) throw new Error('connection lost');
    return { revision: 13 };
  };
  try {
    first = harness(auth, root);
    writeFileSync(profileFile(first, auth, 'mappings'), JSON.stringify([{ workspaceId: 'w1234567', directory: '/tmp/example', dev: '1', ino: '2' }]));
    await first.ready();
    await assert.rejects(first.invoke('morde:sync', 'w1234567', 'handoff_checkpoint', ['src'], true, 'desktop'), /connection lost/);
    const firstState = await first.invoke('morde:state');
    assert.equal(firstState.syncCheckpoint.handoffTarget, 'desktop');
    assert.equal(firstState.handoff.phase, 'needs_review');
    const key = JSON.parse(readFileSync(profileFile(first, auth, 'sync-checkpoint'), 'utf8')).idempotencyKey;
    first.cleanup(false);
    first = null;

    second = harness(auth, root);
    await second.ready();
    const restored = await second.invoke('morde:state');
    assert.equal(restored.handoff.phase, 'needs_review');
    assert.equal(restored.syncCheckpoint.handoffTarget, 'desktop');
    await assert.rejects(second.invoke('morde:sync', 'w1234567', 'handoff_checkpoint', [], true, 'desktop'), /original scope/);
    await assert.rejects(second.invoke('morde:sync', 'w1234567', 'handoff_checkpoint', ['src'], true, 'cloud_workspace'), /earlier sync needs recovery/);
    const result = await second.invoke('morde:sync', 'w1234567', 'handoff_checkpoint', ['src'], true, 'desktop');
    assert.equal(result.revision, 13);
    assert.equal(calls[1].idempotencyKey, key);
    assert.deepEqual((await second.invoke('morde:state')).handoff, {
      workspaceId: 'w1234567', scope: ['src'], source: 'cloud_workspace', target: 'desktop', phase: 'verified', revision: 13,
    });
    assert.equal('id' in (await second.invoke('morde:state')).handoff, false);
    assert.equal((await second.invoke('morde:state')).syncCheckpoint, null);
  } finally {
    first?.cleanup(false);
    second?.cleanup();
    Sync.prototype.run = originalRun;
    globalThis.fetch = originalFetch;
    rmSync(root, { recursive: true, force: true });
  }
});

test('manual sync and handoff are blocked while the Workspace fixed sync binding is enabled', async () => {
  const Sync = require('../out/morde-sync.js').MordeSync;
  const originalRun = Sync.prototype.run;
  let started = false;
  Sync.prototype.run = async () => { started = true; return { revision: 1 }; };
  const h = harness(auth);
  writeFileSync(profileFile(h, auth, 'mappings'), JSON.stringify([{ workspaceId: 'w1234567', directory: '/tmp/example', dev: '1', ino: '2' }]));
  writeFileSync(workspaceSyncProfileFile(h, auth), Buffer.from(JSON.stringify(savedWorkspaceSyncProfile(auth, true))));
  try {
    await h.ready();
    await assert.rejects(h.invoke('morde:sync', 'w1234567', 'upload'), /Pause Workspace sync/);
    await assert.rejects(h.invoke('morde:sync', 'w1234567', 'handoff_checkpoint', [], true), /Pause Workspace sync/);
    assert.equal(started, false);
    assert.equal((await h.invoke('morde:state')).syncCheckpoint, null);
  } finally { Sync.prototype.run = originalRun; h.cleanup(); }
});

test('Workspace sync status IPC rechecks the active local binding and returns only aggregate node state', async () => {
  const Node = require('../out/morde-node.js').MordeSyncthingNode;
  const original = Object.fromEntries(['isRunning', 'start', 'identity', 'configureWorkspaces', 'workspaceStatus']
    .map((name) => [name, Node.prototype[name]]));
  const originalFetch = globalThis.fetch;
  const workRoot = mkdtempSync(join(homedir(), 'morde-workspace-status-'));
  const workspaceDir = join(workRoot, 'project');
  mkdirSync(workspaceDir);
  const info = statSync(workspaceDir, { bigint: true });
  const h = harness(auth);
  let configured = false;
  const requestedFolders = [];
  Node.prototype.isRunning = () => true;
  Node.prototype.start = async () => 'ABCDEFG-ABCDEFG-ABCDEFG-ABCDEFG-ABCDEFG-ABCDEFG-ABCDEFG-ABCDEFG';
  Node.prototype.identity = async () => ({ deviceID: 'ABCDEFG-ABCDEFG-ABCDEFG-ABCDEFG-ABCDEFG-ABCDEFG-ABCDEFG-ABCDEFG' });
  Node.prototype.configureWorkspaces = async () => { configured = true; };
  Node.prototype.workspaceStatus = async (folderID) => {
    requestedFolders.push(folderID);
    return { state: 'syncing', needBytes: 512, needFiles: 1, pullErrors: 0 };
  };
  globalThis.fetch = async (url) => json(String(url).endsWith('/api/capabilities')
    ? { ...capabilitiesResponse, supported_features: [...capabilitiesResponse.supported_features, 'workspace_sync'] }
    : {});
  writeFileSync(profileFile(h, auth, 'mappings'), JSON.stringify([{ workspaceId: 'w1234567', directory: workspaceDir, dev: info.dev.toString(), ino: info.ino.toString() }]));
  writeFileSync(workspaceSyncProfileFile(h, auth), Buffer.from(JSON.stringify(savedWorkspaceSyncProfile(auth, true))));
  try {
    await h.ready();
    for (let attempt = 0; attempt < 20 && !configured; attempt++) await new Promise((resolve) => setTimeout(resolve, 5));
    assert.equal(configured, true);
    const status = await h.invoke('morde:workspace-sync-status', 'w1234567');
    const { observedAt, ...aggregate } = status;
    assert.deepEqual(aggregate, { state: 'syncing', needBytes: 512, needFiles: 1, pullErrors: 0 });
    assert.match(observedAt, /^\d{4}-\d{2}-\d{2}T/);
    assert.deepEqual(requestedFolders, [`morde-${'a'.repeat(32)}`]);
    await assert.rejects(h.invoke('morde:workspace-sync-status', 'w7654321'), /active Workspace sync binding/);
  } finally {
    for (const [name, method] of Object.entries(original)) Node.prototype[name] = method;
    globalThis.fetch = originalFetch;
    h.cleanup();
    rmSync(workRoot, { recursive: true, force: true });
  }
});
