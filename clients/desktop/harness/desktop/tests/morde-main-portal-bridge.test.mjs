import assert from 'node:assert/strict';
import { test } from 'node:test';
import { createRequire } from 'node:module';
import { mkdirSync, mkdtempSync, renameSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

const require = createRequire(import.meta.url);
const future = '2099-01-01T00:00:00Z';
const serviceInstanceID = '12345678-1234-4234-8234-123456789abc';
const auth = {
  origin: 'https://portal.example', serviceInstanceID, accountName: 'local-test', installationId: 'i1234567',
  clientCredential: 'local-client-credential', clientExpiresAt: future,
};
const capabilities = {
  service_instance_id: serviceInstanceID, api_version: '1', hub_version: null, event_version: null,
  minimum_client_version: null, supported_features: ['password_login', 'client_installations', 'ordered_client_login', 'workspace_sync', 'project_upload'],
  default_max_request_bytes: 1048576,
};
const host = {
  id: 'h1234567', owner_id: 'u1234567', name: 'Local test computer', url: 'https://host.example/', state: 'running',
  workspaces: [{ state: 'active', workspace: { id: 'w1234567', name: 'Default Workspace', state: 'ready' } }],
};
const json = (value, status = 200) => new Response(JSON.stringify(value), { status, headers: { 'content-type': 'application/json' } });
const portalStateKeys = ['projectMappings', 'syncCheckpoint', 'syncRecovery', 'syncState'];
function assertPortalProjectState(state) {
  assert.deepEqual(Object.keys(state).sort(), portalStateKeys);
  for (const key of ['accountName', 'origin', 'installationId', 'expiresAt', 'mappings', 'workspaceSync', 'liveSyncs', 'handoff', 'profileError', 'storageReady', 'view']) {
    assert.equal(Object.hasOwn(state, key), false, `Portal state must not expose ${key}`);
  }
  const serialized = JSON.stringify(state);
  for (const privateValue of ['local-client-credential', 'portal.example', 'i1234567', 'credentials.bin']) {
    assert.equal(serialized.includes(privateValue), false, `Portal state must not expose ${privateValue}`);
  }
}

function mockFetch(calls) {
  globalThis.fetch = async (input, options = {}) => {
    const url = new URL(String(input));
    calls.push({ url: url.href, method: options.method ?? 'GET', body: options.body });
    if (url.pathname === '/api/capabilities') return json(capabilities);
    if (url.pathname === '/api/auth/client-web-session') return json({ web_session_credential: 'local-web-session', web_session_expires_at: future });
    if (url.pathname === '/api/auth/client-logout') return new Response(null, { status: 204 });
    if (url.pathname === '/api/me') return json({ id: 'u1234567' });
    if (url.pathname === '/api/hosts') return json({ items: [host] });
    if (url.pathname === '/api/hosts/h1234567') return json(host);
    if (url.pathname === '/api/hosts/h1234567/sync-folders') return json({ items: [] });
    return json({ error: { code: 'not_found', message: 'local mock route missing' } }, 404);
  };
}

function harness() {
  const root = mkdtempSync(join(tmpdir(), 'morde-p11-portal-'));
  const userData = join(root, 'Morde Client');
  mkdirSync(userData, { recursive: true });
  writeFileSync(join(userData, 'credentials.bin'), JSON.stringify({ version: 2, credentials: auth }));
  const handlers = new Map(), windows = [], views = [], partitions = [], appEvents = new Map();
  let nextContentsID = 1;

  class FakeContents {
    id = nextContentsID++;
    events = new Map();
    messages = [];
    url = '';
    closed = false;
    mainFrame = {};
    on(name, fn) { this.events.set(name, fn); }
    setWindowOpenHandler(fn) { this.open = fn; }
    async loadURL(url) { this.url = url; }
    getURL() { return this.url; }
    isDestroyed() { return this.closed; }
    focus() { this.focused = true; }
    close() { this.closed = true; this.events.get('destroyed')?.(); }
    send(...args) { this.messages.push(args); }
  }

  class FakeWindow {
    constructor(options) {
      this.options = options;
      this.events = new Map();
      this.webContents = options.webContents ?? new FakeContents();
      this.contentBounds = { x: 0, y: 0, width: 1120, height: 760 };
      this.contentView = {
        attached: [], addChildView: (view) => this.contentView.attached.push(view),
        removeChildView: (view) => { this.contentView.attached = this.contentView.attached.filter((item) => item !== view); },
      };
      this.minimized = false;
      windows.push(this);
    }
    on(name, fn) { this.events.set(name, fn); }
    once(name, fn) { this.events.set(name, fn); }
    async loadFile(path) { this.path = path; }
    async loadURL(url) { this.webContents.url = url; }
    show() { this.shown = true; }
    isMinimized() { return this.minimized; }
    restore() { this.minimized = false; this.restored = true; }
    getContentBounds() { return this.contentBounds; }
    focus() { this.focused = true; }
    isDestroyed() { return this.destroyed ?? false; }
    close() { this.destroyed = true; this.events.get('closed')?.(); }
  }

  class FakeView {
    constructor(options) { this.options = options; this.webContents = new FakeContents(); views.push(this); }
    setBounds(bounds) { this.bounds = bounds; }
  }

  const fakeSession = { fromPartition(name) {
    const partition = {
      name, cookies: { values: [], async set(value) { this.values.push(value); } },
      webRequest: {
        onCompleted(_filter, fn) { this.completed = fn; },
        onHeadersReceived(_filter, fn) { this.headersReceived = fn; },
      },
      events: new Map(), on(event, fn) { this.events.set(event, fn); },
      setPermissionRequestHandler(fn) { this.permissionRequest = fn; },
      async clearStorageData() { this.cleared = true; },
    };
    partitions.push(partition);
    return partition;
  } };
  const dialog = {
    showOpenDialog: async () => ({ canceled: true, filePaths: [] }),
    showMessageBox: async () => ({ response: 1 }),
  };
  const paths = { appData: root };
  const electron = {
    app: {
      setName() {}, getPath: (key) => paths[key] ?? root, setPath: (key, path) => { paths[key] = path; },
      getLocale: () => 'en', getVersion: () => '0.1.0', requestSingleInstanceLock: () => true,
      isPackaged: false, whenReady: () => Promise.resolve(), on: (name, fn) => appEvents.set(name, fn), quit() {},
    },
    BrowserWindow: FakeWindow, WebContentsView: FakeView, session: fakeSession,
    ipcMain: { handle: (channel, fn) => handlers.set(channel, fn) }, dialog,
    Menu: { buildFromTemplate: (template) => template, setApplicationMenu() {} },
    safeStorage: {
      isEncryptionAvailable: () => true, getSelectedStorageBackend: () => 'test',
      encryptString: (value) => Buffer.from(value), decryptString: (value) => value.toString(),
    },
    shell: { openExternal: async () => undefined },
    Tray: class { setToolTip() {} setContextMenu() {} on() {} },
  };
  const Module = require('node:module');
  const originalLoad = Module._load;
  const entry = require.resolve('../out/morde-main.js');
  Module._load = function (request, parent, isMain) {
    if (request === 'electron') return electron;
    return originalLoad.call(this, request, parent, isMain);
  };
  delete require.cache[entry];
  try { require(entry); }
  finally { Module._load = originalLoad; }

  return {
    root, userData, handlers, windows, views, partitions, appEvents, dialog,
    async ready() { await new Promise((resolve) => setImmediate(resolve)); await new Promise((resolve) => setImmediate(resolve)); },
    invoke(channel, sender, args = [], senderFrame = sender.mainFrame) {
      return handlers.get(channel)({ sender, senderFrame }, ...args);
    },
    cleanup() { delete require.cache[entry]; rmSync(root, { recursive: true, force: true }); },
  };
}

test('Portal bridge requires the current trusted top-level route and returns only empty-folder metadata', async () => {
  const originalFetch = globalThis.fetch;
  const calls = [];
  mockFetch(calls);
  const h = harness();
  const selectedRoot = mkdtempSync(join(process.cwd(), '.morde-p11-empty-project-'));
  const selectedDirectory = join(selectedRoot, 'empty-project');
  mkdirSync(selectedDirectory);
  try {
    await h.ready();
    const portal = h.views[0];
    assert.equal(portal.options.webPreferences.sandbox, true);
    assert.equal(portal.options.webPreferences.nodeIntegration, false);
    assert.equal(portal.options.webPreferences.additionalArguments.at(-1), '--morde-preload-mode=portal');
    assert.match(portal.options.webPreferences.preload, /morde-preload\.js$/);

    portal.webContents.url = 'https://portal.example/hosts#host-h1234567';
    const state = await h.invoke('morde:portal-state', portal.webContents);
    assertPortalProjectState(state);
    assert.deepEqual(state, { projectMappings: [], syncState: 'idle', syncCheckpoint: null, syncRecovery: null });
    const mainState = await h.invoke('morde:state', h.windows[0].webContents);
    assert.equal(mainState.authenticated, true);
    assert.equal(mainState.accountName, auth.accountName);
    assert.equal(mainState.origin, auth.origin);
    await assert.rejects(h.invoke('morde:portal-state', h.windows[0].webContents), /Invalid Portal window or route/);
    await assert.rejects(h.invoke('morde:portal-state', portal.webContents, [], {}), /Invalid Portal window or route/);
    portal.webContents.url = 'https://portal.example/assistant';
    await assert.rejects(h.invoke('morde:portal-state', portal.webContents), /Invalid Portal window or route/);
    portal.webContents.url = 'https://portal.example/workspaces?host=h1234567';

    const targets = await h.invoke('morde:portal-project-upload-targets', portal.webContents);
    assert.deepEqual(targets.map(({ hostId, workspaceId }) => ({ hostId, workspaceId })), [{ hostId: 'h1234567', workspaceId: 'w1234567' }]);
    assert.equal(calls.every((call) => new URL(call.url).origin === auth.origin), true, 'all network calls are replaced with this test’s local fetch stub');

    h.dialog.showOpenDialog = async () => ({ canceled: false, filePaths: [selectedDirectory] });
    portal.webContents.url = 'https://portal.example/hosts#host-h1234567';
    const selection = await h.invoke('morde:portal-project-select-directory', portal.webContents, ['h1234567', 'w1234567']);
    assert.deepEqual(selection, {
      selectionID: selection.selectionID, hostId: 'h1234567', workspaceId: 'w1234567',
      projectName: 'empty-project', directoryName: 'empty-project',
    });
    assert.equal(JSON.stringify(selection).includes(selectedDirectory), false);
    const pushedState = portal.webContents.messages.filter(([channel]) => channel === 'morde:portal-state').at(-1)?.[1];
    assertPortalProjectState(pushedState);
    assert.equal(pushedState.projectMappings.length, 1);
    assert.deepEqual(pushedState.projectMappings[0], {
      selectionID: selection.selectionID, hostId: 'h1234567', workspaceId: 'w1234567',
      projectName: 'empty-project', directoryName: 'empty-project',
    });
    assert.equal(JSON.stringify(pushedState).includes(selectedDirectory), false);
    await assert.rejects(
      h.invoke('morde:portal-project-upload', portal.webContents, ['00000000-0000-4000-8000-000000000000', 'empty-project']),
      /先选择本机项目文件夹/,
    );
    assert.equal(calls.some((call) => call.method === 'POST' && new URL(call.url).pathname.includes('/sync/')), false);
  } finally {
    globalThis.fetch = originalFetch;
    h.cleanup();
    rmSync(selectedRoot, { recursive: true, force: true });
  }
});

test('Portal project upload rejects a replaced empty directory before registering or uploading it', async () => {
  const originalFetch = globalThis.fetch;
  const calls = [];
  mockFetch(calls);
  const h = harness();
  const selectedRoot = mkdtempSync(join(process.cwd(), '.morde-p11-replaced-project-'));
  const selectedDirectory = join(selectedRoot, 'empty-project');
  const movedDirectory = join(selectedRoot, 'original-empty-project');
  mkdirSync(selectedDirectory);
  try {
    await h.ready();
    const portal = h.views[0];
    portal.webContents.url = 'https://portal.example/hosts#host-h1234567';
    h.dialog.showOpenDialog = async () => ({ canceled: false, filePaths: [selectedDirectory] });
    const selection = await h.invoke('morde:portal-project-select-directory', portal.webContents, ['h1234567', 'w1234567']);
    assert.equal(selection.projectName, 'empty-project');

    renameSync(selectedDirectory, movedDirectory);
    mkdirSync(selectedDirectory);
    h.dialog.showMessageBox = async () => ({ response: 0 });

    await assert.rejects(
      h.invoke('morde:portal-project-upload', portal.webContents, [selection.selectionID, 'empty-project']),
      /项目文件夹身份已变化，请重新选择/,
    );
    assert.equal(calls.some((call) => call.method === 'POST' && new URL(call.url).pathname.endsWith('/sync-folders')), false,
      'a changed local directory must be rejected before registering a remote folder');
    assert.equal(calls.some((call) => call.method === 'POST' && new URL(call.url).pathname.includes('/sync-sessions')), false,
      'a changed local directory must never start an upload session');
  } finally {
    globalThis.fetch = originalFetch;
    h.cleanup();
    rmSync(selectedRoot, { recursive: true, force: true });
  }
});

test('returnToPortalHosts is limited to the verified Host Workbench top frame', async () => {
  const originalFetch = globalThis.fetch;
  const calls = [];
  mockFetch(calls);
  const h = harness();
  try {
    await h.ready();
    const portal = h.views[0];
    const returnHandler = h.handlers.get('morde:return-to-portal-hosts');
    await assert.rejects(returnHandler({ sender: h.windows[0].webContents, senderFrame: h.windows[0].webContents.mainFrame }), /Invalid Host Workbench window/);
    await assert.rejects(returnHandler({ sender: portal.webContents, senderFrame: portal.webContents.mainFrame }), /Invalid Host Workbench window/);

    const popup = portal.webContents.open({ url: 'https://portal.example/host-entry/h1234567' });
    assert.equal(popup.action, 'allow');
    const hostContents = new portal.webContents.constructor();
    popup.createWindow({ webContents: hostContents, webPreferences: { additionalArguments: ['--existing-arg'] } });
    const hostWindow = h.windows.at(-1);
    assert.equal(hostWindow.options.webPreferences.sandbox, true);
    assert.equal(hostWindow.options.webPreferences.nodeIntegration, false);
    assert.match(hostWindow.options.webPreferences.preload, /morde-preload\.js$/);
    assert.deepEqual(hostWindow.options.webPreferences.additionalArguments, ['--existing-arg', '--morde-preload-mode=host-workbench']);

    const navigate = hostContents.events.get('will-navigate');
    let prevented = false;
    navigate({ preventDefault() { prevented = true; } }, 'https://host.example/__ch/auth?t=local-test-ticket');
    await new Promise((resolve) => setImmediate(resolve));
    await new Promise((resolve) => setImmediate(resolve));
    assert.equal(hostContents.url, 'https://host.example/__ch/auth?t=local-test-ticket');
    prevented = false;
    navigate({ preventDefault() { prevented = true; } }, 'https://host.example/__ch/wb/?tab=workspace');
    assert.equal(prevented, false, 'the gateway can open its standalone Workbench route');
    hostContents.url = 'https://host.example/__ch/wb/?tab=workspace';

    await assert.rejects(returnHandler({ sender: hostContents, senderFrame: {} }), /Invalid Host Workbench window/);
    hostContents.url = 'https://host.example/__ch/wb/?task_id=q1234567';
    await assert.rejects(returnHandler({ sender: hostContents, senderFrame: hostContents.mainFrame }), /not on a trusted page/);
    hostContents.url = 'https://evil.example/__ch/wb/';
    await assert.rejects(returnHandler({ sender: hostContents, senderFrame: hostContents.mainFrame }), /not on a trusted page/);

    hostContents.url = 'https://host.example/__ch/wb/';
    const mainWindow = h.windows[0];
    for (const [index, event] of ['resize', 'maximize', 'unmaximize', 'restore', 'enter-full-screen', 'leave-full-screen'].entries()) {
      const bounds = { x: 0, y: 0, width: 1440 + index * 10, height: 900 + index * 10 };
      mainWindow.contentBounds = bounds;
      mainWindow.events.get(event)();
      assert.deepEqual(portal.bounds, bounds, `WebContentsView must follow ${event}`);
    }
    assert.equal(mainWindow.options.frame, true, 'the desktop window uses native OS caption controls');

    const restoredBounds = { x: 0, y: 0, width: 1680, height: 1050 };
    mainWindow.contentBounds = restoredBounds;
    mainWindow.minimized = true;
    mainWindow.shown = false;
    assert.deepEqual(await returnHandler({ sender: hostContents, senderFrame: hostContents.mainFrame }), { returned: true });
    assert.equal(hostWindow.isDestroyed(), true);
    assert.deepEqual(portal.bounds, restoredBounds, 'returning to an already attached view refreshes its bounds');
    assert.equal(mainWindow.restored, true, 'returning from a Host Workbench restores a minimized desktop window');
    assert.equal(mainWindow.shown, true, 'returning from a Host Workbench shows a hidden desktop window');
    assert.equal(mainWindow.focused, true);
    assert.equal(mainWindow.contentView.attached.includes(portal), true);
    await assert.rejects(returnHandler({ sender: hostContents, senderFrame: hostContents.mainFrame }), /Invalid Host Workbench window/);
  } finally {
    globalThis.fetch = originalFetch;
    h.cleanup();
  }
});

test('task observation windows do not inherit a desktop preload or bridge mode', async () => {
  const originalFetch = globalThis.fetch;
  const calls = [];
  mockFetch(calls);
  const h = harness();
  try {
    await h.ready();
    const popup = h.views[0].webContents.open({ url: 'about:blank' });
    assert.equal(popup.action, 'allow');
    popup.createWindow({ webPreferences: { additionalArguments: ['--caller-arg'] } });
    const observationWindow = h.windows.at(-1);
    assert.equal(observationWindow.options.webPreferences.preload, undefined);
    assert.deepEqual(observationWindow.options.webPreferences.additionalArguments, []);
  } finally {
    globalThis.fetch = originalFetch;
    h.cleanup();
  }
});
