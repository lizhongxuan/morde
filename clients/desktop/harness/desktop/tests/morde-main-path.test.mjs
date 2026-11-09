import { test } from 'node:test';
import assert from 'node:assert/strict';
import { mkdirSync, mkdtempSync, realpathSync, rmSync, symlinkSync, writeFileSync } from 'node:fs';
import { homedir, tmpdir } from 'node:os';
import { join, parse } from 'node:path';
import { mordeUserDataDirectory } from '../out/morde-core.js';
import { createRequire } from 'node:module';

test('system directory protection handles Windows paths and filesystem roots', async () => {
  const root = mkdtempSync(join(tmpdir(), 'morde-system-path-'));
  const require = createRequire(import.meta.url);
  const Module = require('node:module');
  const originalLoad = Module._load;
  Module._load = function (request, parent, isMain) {
    if (request === 'electron') return {
      app: {
        setName() {},
        getPath: () => root,
        setPath() {},
        getLocale: () => 'en',
        requestSingleInstanceLock: () => false,
        quit() {},
      },
    };
    return originalLoad.call(this, request, parent, isMain);
  };

  try {
    const { isSystemDirectory } = await import('../out/morde-main.js');
    assert.equal(isSystemDirectory(String.raw`C:\Windows`, String.raw`C:\Windows`), true);
    assert.equal(isSystemDirectory(String.raw`c:\windows\Temp`, String.raw`C:\Windows`), true);
    assert.equal(isSystemDirectory(String.raw`C:\WindowsApps`, String.raw`C:\Windows`), false);
    assert.equal(isSystemDirectory(String.raw`D:\Temp`, String.raw`C:\Windows`), false);
    assert.equal(isSystemDirectory('/tmp', '/'), false);
  } finally {
    Module._load = originalLoad;
    rmSync(root, { recursive: true, force: true });
  }
});

test('authenticated WebView restores Assistant and workspace folder selection enforces local path boundaries', async () => {
  const root = mkdtempSync(join(homedir(), 'morde-assistant-entry-'));
  const protectedTemp = mkdtempSync(join(tmpdir(), 'morde-protected-system-dir-'));
  const require = createRequire(import.meta.url);
  const Module = require('node:module');
  const originalLoad = Module._load;
  const entry = require.resolve('../out/morde-main.js');
  const syncModule = require('../out/morde-sync.js');
  const handlers = new Map(), windows = [], views = [], appEvents = new Map(), paths = { appData: join(root, 'app-data') };
  const workspaceRequests = [], preservedCalls = [];
  let workspaceOffline = false, blockedPreservedMethod = '', releasePreserved;
  let childOwnsFolder = false, stopSyncResult = true, fakeSyncState = 'idle', startSyncForExit = false, quitCount = 0, shutdownWarnings = 0;
  const workspaceRoot = join(root, 'workspaces');
  const workspaceA = join(workspaceRoot, 'project-a'), workspaceB = join(workspaceRoot, 'project-b');
  const workspaceAlias = join(workspaceRoot, 'project-a-alias');
  mkdirSync(workspaceA, { recursive: true });
  mkdirSync(workspaceB, { recursive: true });
  symlinkSync(workspaceA, workspaceAlias, process.platform === 'win32' ? 'junction' : 'dir');
  let selectedDirectory = '';
  let syncDirectory = null;
  const expiresAt = '2099-01-01T00:00:00Z';

  class FakeContents {
    events = new Map();
    loaded = [];
    url = '';
    on(name, fn) { this.events.set(name, fn); }
    setWindowOpenHandler(fn) { this.open = fn; }
    async loadURL(url) { this.loaded.push(url); this.url = url; }
    async loadFile() {}
    getURL() { return this.url; }
    isDestroyed() { return false; }
    focus() {}
    close() {}
    send() {}
  }
  class FakeWindow {
    constructor(options) {
      this.webContents = new FakeContents();
      this.contentView = { addChildView() {}, removeChildView() {} };
      windows.push(this);
    }
    on(name, fn) { this.events ??= new Map(); this.events.set(name, fn); }
    once(name, fn) { this.on(name, fn); }
    async loadFile() {}
    getContentBounds() { return { x: 0, y: 0, width: 1120, height: 760 }; }
    isDestroyed() { return false; }
    show() {}
    focus() {}
    close() {}
  }
  class FakeView {
    constructor() { this.webContents = new FakeContents(); views.push(this); }
    setBounds() {}
  }
  class FakeMordeAPI {
    async login(origin, name) {
      return {
        credentials: { origin, serviceInstanceID: '12345678-1234-4234-8234-123456789abc', accountName: name,
          installationId: 'i1234567', clientCredential: 'client-secret', clientExpiresAt: expiresAt,
          syncCredential: 'sync-secret', syncExpiresAt: expiresAt },
        webSession: { credential: 'web-secret', expiresAt },
      };
    }
    async webSession() { return { credential: 'renewed-web-secret', expiresAt }; }
    async requireWorkspaceSyncCapability() {}
    async workspace(id) { workspaceRequests.push(id); if (workspaceOffline) throw new Error('Workspace API offline'); return { state: 'ready' }; }
  }
  class FakeSync {
    constructor(_binary, _update, started) { this.started = started; }
    snapshot() { return fakeSyncState; }
    checkpointMayOwnDirectory(checkpoint) { return Boolean(checkpoint?.childPid) && childOwnsFolder; }
    discardCheckpoint() { if (childOwnsFolder) return false; fakeSyncState = 'idle'; return true; }
    stop() { if (stopSyncResult) fakeSyncState = 'idle'; else fakeSyncState = 'shutdown_unknown'; return Promise.resolve(stopSyncResult); }
    async run(_auth, mapping, _mode, _scope, _checkpoint, _stillAuthorized, confirmMapping) {
      await confirmMapping?.();
      syncDirectory = mapping.directory;
      if (startSyncForExit) { fakeSyncState = 'running'; this.started(424242); return { session: { id: 'p1234567', workspace_id: mapping.workspaceId, state: 'running' }, conflicts: [] }; }
      return { session: { id: 'p1234567', workspace_id: mapping.workspaceId, state: 'completed' }, conflicts: [] };
    }
    async listPreserved(mapping) { preservedCalls.push(['list', mapping.workspaceId]); await this.waitFor('list'); return [{ id: 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', workspace_id: mapping.workspaceId, path: 'notes.txt', created_at: '2026-10-31T10:00:00Z', size: 19 }]; }
    async restorePreserved(mapping, id) { preservedCalls.push(['restore', mapping.workspaceId, id]); await this.waitFor('restore'); }
    async deletePreserved(mapping, id) { preservedCalls.push(['delete', mapping.workspaceId, id]); await this.waitFor('delete'); }
    async waitFor(method) { if (blockedPreservedMethod !== method) return; await new Promise((resolve) => { releasePreserved = resolve; }); }
  }
  const electron = {
    app: {
      setName() {}, getPath: (key) => paths[key] ?? root, setPath: (key, value) => { paths[key] = value; }, getLocale: () => 'en',
      getVersion: () => '0.1.0', requestSingleInstanceLock: () => true, isPackaged: false, whenReady: () => Promise.resolve(),
      on: (name, fn) => appEvents.set(name, fn), quit() { quitCount++; },
    },
    BrowserWindow: FakeWindow, WebContentsView: FakeView,
    session: { fromPartition: () => ({
      cookies: { async set() {} }, webRequest: { onCompleted() {} }, on() {}, setPermissionRequestHandler() {},
      async clearStorageData() {},
    }) },
    ipcMain: { handle: (channel, fn) => handlers.set(channel, fn) },
    dialog: {
      showMessageBox: async () => { shutdownWarnings++; return { response: 0 }; },
      showOpenDialog: async () => selectedDirectory ? { canceled: false, filePaths: [selectedDirectory] } : { canceled: true, filePaths: [] },
    },
    Menu: { buildFromTemplate: (items) => items, setApplicationMenu() {} },
    safeStorage: { isEncryptionAvailable: () => true, getSelectedStorageBackend: () => 'gnome_libsecret', encryptString: (value) => Buffer.from(value), decryptString: (value) => value.toString() },
    shell: { openExternal: async () => {} },
    Tray: class { setToolTip() {} setContextMenu() {} on() {} },
  };
  const core = {
    MordeAPI: FakeMordeAPI,
    mordeUserDataDirectory,
    checkedId: (_kind, value) => {
      if (typeof value !== 'string' || !/^w[a-z0-9]{7}$/.test(value)) throw new Error('Invalid Workspace ID');
      return value;
    },
    portalOrigin: (value) => {
      const url = new URL(value);
      if (url.protocol !== 'https:' || url.pathname !== '/' || url.search || url.hash) throw new Error('Invalid service origin');
      return url.origin;
    },
    credentialExpired: () => false,
    credentialExpiresSoon: () => false,
  };
  Module._load = function (request, parent, isMain) {
    if (request === 'electron') return electron;
    if (request === './morde-core') return core;
    if (request === './morde-sync') return { ...syncModule, MordeSync: FakeSync };
    return originalLoad.call(this, request, parent, isMain);
  };

  try {
    delete require.cache[entry];
    require(entry);
    await new Promise((resolve) => setImmediate(resolve));
    await new Promise((resolve) => setImmediate(resolve));
    const mainWindow = windows[0];
    await handlers.get('morde:login')({ sender: mainWindow.webContents }, 'https://portal.example', 'user', 'password');
    const contents = views[0].webContents;
    const assistantURL = 'https://portal.example/assistant';
    assert.deepEqual(contents.loaded, [assistantURL]);
    assert.equal((await handlers.get('morde:state')({ sender: mainWindow.webContents })).accountName, 'user');
    await handlers.get('morde:show-web')({ sender: mainWindow.webContents }, 'account');
    assert.deepEqual(contents.loaded, [assistantURL, 'https://portal.example/settings']);
    await handlers.get('morde:show-web')({ sender: mainWindow.webContents }, 'assistant');
    assert.deepEqual(contents.loaded, [assistantURL, 'https://portal.example/settings', assistantURL]);
    await assert.rejects(() => handlers.get('morde:show-web')({ sender: mainWindow.webContents }, 'https://evil.example/'), /Invalid Morde page/);

    let prevented = false;
    const navigate = contents.events.get('will-navigate');
    navigate({ preventDefault() { prevented = true; } }, 'https://evil.example/assistant');
    assert.equal(prevented, true);
    assert.deepEqual(contents.loaded, [assistantURL, 'https://portal.example/settings', assistantURL]);

    prevented = false;
    navigate({ preventDefault() { prevented = true; } }, 'https://portal.example/login');
    assert.equal(prevented, true);
    for (let i = 0; i < 4; i++) await new Promise((resolve) => setImmediate(resolve));
    assert.deepEqual(contents.loaded, [assistantURL, 'https://portal.example/settings', assistantURL, assistantURL]);

    const invoke = handlers.get('morde:choose-directory');
    selectedDirectory = workspaceA;
    const mappingA = await invoke({ sender: mainWindow.webContents }, 'w1234567');
    assert.deepEqual(mappingA, { workspaceId: 'w1234567', directoryName: 'project-a' });

    const firstRecoveryRoot = join(workspaceRoot, '.morde-preserved');
    const mappingInsideOwnArchive = join(firstRecoveryRoot, 'nested-workspace');
    mkdirSync(mappingInsideOwnArchive, { recursive: true });
    selectedDirectory = mappingInsideOwnArchive;
    await assert.rejects(() => invoke({ sender: mainWindow.webContents }, 'w1234567'), /safety-copy|overlap/i);

    selectedDirectory = workspaceB;
    const mappingB = await invoke({ sender: mainWindow.webContents }, 'w7654321');
    assert.deepEqual(mappingB, { workspaceId: 'w7654321', directoryName: 'project-b' });

    selectedDirectory = workspaceAlias;
    await assert.rejects(() => invoke({ sender: mainWindow.webContents }, 'w7654321'), /overlap/);

    selectedDirectory = join(paths.userData, 'nested');
    mkdirSync(selectedDirectory, { recursive: true });
    await assert.rejects(() => invoke({ sender: mainWindow.webContents }, 'w7654321'), /overlap/);
    selectedDirectory = paths.appData;
    await assert.rejects(() => invoke({ sender: mainWindow.webContents }, 'w7654321'), /overlap/);

    selectedDirectory = parse(root).root;
    await assert.rejects(() => invoke({ sender: mainWindow.webContents }, 'w7654321'), /filesystem root/);
    selectedDirectory = process.platform === 'win32' ? process.env.SystemRoot : process.platform === 'darwin' ? protectedTemp : '/etc';
    await assert.rejects(() => invoke({ sender: mainWindow.webContents }, 'w7654321'), /system directories|EACCES|EPERM|permission|access denied|EROFS/i);

    selectedDirectory = join(workspaceRoot, 'project-b-alias');
    symlinkSync(workspaceB, selectedDirectory, process.platform === 'win32' ? 'junction' : 'dir');
    const canonicalB = await invoke({ sender: mainWindow.webContents }, 'w7654321');
    assert.deepEqual(canonicalB, { workspaceId: 'w7654321', directoryName: 'project-b' });
    await handlers.get('morde:sync')({ sender: mainWindow.webContents }, 'w1234567', 'upload');
    assert.equal(syncDirectory, realpathSync(workspaceA));

    workspaceOffline = true;
    const workspaceRequestsBeforeLocalUse = workspaceRequests.length;
    const localCopies = await handlers.get('morde:preserved-list')({ sender: mainWindow.webContents }, 'w1234567');
    assert.equal(localCopies[0].path, 'notes.txt');
    assert.equal(workspaceRequests.length, workspaceRequestsBeforeLocalUse, 'local safety-copy listing must not call the online Workspace API');

    for (const [method, channel] of [['restore', 'morde:preserved-restore'], ['delete', 'morde:preserved-delete']]) {
      blockedPreservedMethod = method;
      releasePreserved = undefined;
      let operationError;
      const localOperation = handlers.get(channel)({ sender: mainWindow.webContents }, 'w1234567', 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa').catch((error) => { operationError = error; throw error; });
      localOperation.catch(() => {});
      for (let attempt = 0; attempt < 100 && !releasePreserved && !operationError; attempt++) await new Promise((resolve) => setTimeout(resolve, 5));
      assert.equal(typeof releasePreserved, 'function', `${method} operation should be in flight${operationError ? `, failed first: ${operationError.message}` : ''}`);
      await assert.rejects(() => handlers.get('morde:logout')({ sender: mainWindow.webContents }), /local Workspace operation|local safety-copy operation/);
      await assert.rejects(() => handlers.get('morde:login')({ sender: mainWindow.webContents }, 'https://other.example', 'other', 'password'), /local Workspace operations|local safety-copy operation/);
      const release = releasePreserved;
      releasePreserved = undefined;
      blockedPreservedMethod = '';
      release();
      await localOperation;
    }

    workspaceOffline = false;
    const reservedRoot = join(workspaceRoot, '.morde-preserved');
    const reservedMapping = join(reservedRoot, 'nested-workspace');
    mkdirSync(reservedMapping, { recursive: true });
    selectedDirectory = reservedMapping;
    await assert.rejects(() => invoke({ sender: mainWindow.webContents }, 'w5555555'), /safety-copy|overlap/i);

    const symlinkParent = join(workspaceRoot, 'symlink-parent'), symlinkWorkspace = join(symlinkParent, 'project');
    mkdirSync(symlinkWorkspace, { recursive: true });
    symlinkSync(reservedRoot, join(symlinkParent, '.morde-preserved'), process.platform === 'win32' ? 'junction' : 'dir');
    selectedDirectory = symlinkWorkspace;
    await assert.rejects(() => invoke({ sender: mainWindow.webContents }, 'w6666666'), /safety-copy.*real directory|symlink/i);

    const fileParent = join(workspaceRoot, 'file-parent'), fileWorkspace = join(fileParent, 'project');
    mkdirSync(fileWorkspace, { recursive: true });
    writeFileSync(join(fileParent, '.morde-preserved'), 'not a directory');
    selectedDirectory = fileWorkspace;
    await assert.rejects(() => invoke({ sender: mainWindow.webContents }, 'w7777777'), /safety-copy.*real directory/i);

    rmSync(workspaceB, { recursive: true, force: true });
    const otherWorkspaceInsideArchive = join(reservedRoot, 'other-workspace');
    mkdirSync(otherWorkspaceInsideArchive, { recursive: true });
    symlinkSync(otherWorkspaceInsideArchive, workspaceB, process.platform === 'win32' ? 'junction' : 'dir');
    const callsBeforeUnsafeRestore = preservedCalls.length;
    await assert.rejects(() => handlers.get('morde:preserved-restore')({ sender: mainWindow.webContents }, 'w1234567', 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'), /safety-copy|overlap/i);
    assert.equal(preservedCalls.length, callsBeforeUnsafeRestore, 'restore must stop before calling the helper when another mapping enters the archive root');

    rmSync(workspaceB, { recursive: true, force: true });
    mkdirSync(workspaceB, { recursive: true });
    selectedDirectory = workspaceB;
    await invoke({ sender: mainWindow.webContents }, 'w7654321');
    startSyncForExit = true;
    childOwnsFolder = true;
    stopSyncResult = false;
    await handlers.get('morde:sync')({ sender: mainWindow.webContents }, 'w1234567', 'live_sync');
    let quitPrevented = false;
    appEvents.get('before-quit')({ preventDefault() { quitPrevented = true; } });
    await new Promise((resolve) => setImmediate(resolve));
    assert.equal(quitPrevented, true);
    assert.equal(quitCount, 0, 'the application must stay open while a sync child may still own the folder');
    assert.equal(shutdownWarnings, 1);

    childOwnsFolder = false;
    stopSyncResult = true;
    appEvents.get('before-quit')({ preventDefault() {} });
    await new Promise((resolve) => setImmediate(resolve));
    assert.equal(quitCount, 1, 'the application may quit after the child and recovery point are confirmed safe');
  } finally {
    Module._load = originalLoad;
    delete require.cache[entry];
    rmSync(root, { recursive: true, force: true });
    rmSync(protectedTemp, { recursive: true, force: true });
  }
});
