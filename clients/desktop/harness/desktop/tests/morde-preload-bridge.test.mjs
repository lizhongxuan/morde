import assert from 'node:assert/strict';
import { test } from 'node:test';
import { createRequire } from 'node:module';

const require = createRequire(import.meta.url);
const Module = require('node:module');
const preloadPath = require.resolve('../out/morde-preload.js');

function loadBridge(mode) {
  let exposed;
  const calls = [], listeners = new Map(), removed = [];
  const electron = {
    contextBridge: { exposeInMainWorld: (name, value) => { exposed = { name, value }; } },
    ipcRenderer: {
      invoke: (channel, ...args) => { calls.push([channel, ...args]); return Promise.resolve({ ok: true }); },
      on: (channel, callback) => listeners.set(channel, callback),
      off: (channel, callback) => removed.push([channel, callback]),
    },
  };
  const previousArgv = [...process.argv];
  const originalLoad = Module._load;
  process.argv.splice(0, process.argv.length, ...previousArgv.filter((argument) => !argument.startsWith('--morde-preload-mode=')));
  if (mode) process.argv.push(`--morde-preload-mode=${mode}`);
  Module._load = function (request, parent, isMain) {
    if (request === 'electron') return electron;
    return originalLoad.call(this, request, parent, isMain);
  };
  delete require.cache[preloadPath];
  try { require(preloadPath); }
  finally {
    Module._load = originalLoad;
    process.argv.splice(0, process.argv.length, ...previousArgv);
    delete require.cache[preloadPath];
  }
  return { exposed, calls, listeners, removed };
}

test('Portal preload exposes project and restricted local recovery operations', async () => {
  const bridge = loadBridge('portal');
  assert.equal(bridge.exposed.name, 'morde');
  assert.deepEqual(Object.keys(bridge.exposed.value).sort(), [
    'deletePreserved', 'legacyDiscardSync', 'legacyPauseSync', 'legacyResumeSync', 'legacyStopLiveSync', 'legacyStopSync', 'legacySyncState',
    'onState', 'onSync', 'preserved', 'projectUploadTargets', 'restorePreserved', 'selectProjectDirectory', 'state', 'stopProjectSync', 'uploadProject',
  ]);
  await bridge.exposed.value.projectUploadTargets();
  await bridge.exposed.value.selectProjectDirectory('h1234567', 'w1234567');
  await bridge.exposed.value.uploadProject('selection', 'project');
  await bridge.exposed.value.stopProjectSync('selection');
  await bridge.exposed.value.state();
  await bridge.exposed.value.legacySyncState();
  await bridge.exposed.value.restorePreserved('w1234567', 'a'.repeat(32));
  assert.deepEqual(bridge.calls, [
    ['morde:portal-project-upload-targets'],
    ['morde:portal-project-select-directory', 'h1234567', 'w1234567'],
    ['morde:portal-project-upload', 'selection', 'project'],
    ['morde:portal-project-stop', 'selection'],
    ['morde:portal-state'],
    ['morde:portal-legacy-state'],
    ['morde:portal-preserved-restore', 'w1234567', 'a'.repeat(32)],
  ]);

  const received = [];
  const unsubscribe = bridge.exposed.value.onState((value) => received.push(value));
  const safeState = { projectMappings: [], syncState: 'idle', syncCheckpoint: null, syncRecovery: null };
  bridge.listeners.get('morde:portal-state')({}, safeState);
  assert.deepEqual(received, [safeState]);
  for (const key of ['accountName', 'origin', 'installationId', 'mappings', 'workspaceSync', 'liveSyncs']) {
    assert.equal(Object.hasOwn(received[0], key), false, `Portal preload state must not expose ${key}`);
  }
  unsubscribe();
  assert.equal(bridge.removed[0][0], 'morde:portal-state');
});

test('Host Workbench preload exposes only the validated return action', async () => {
  const bridge = loadBridge('host-workbench');
  assert.deepEqual(Object.keys(bridge.exposed.value), ['returnToPortalHosts']);
  await bridge.exposed.value.returnToPortalHosts();
  assert.deepEqual(bridge.calls, [['morde:return-to-portal-hosts']]);
});

test('main window preload retains its existing desktop API', () => {
  const bridge = loadBridge(undefined);
  assert.equal(bridge.exposed.name, 'morde');
  assert.equal(typeof bridge.exposed.value.login, 'function');
  assert.equal(typeof bridge.exposed.value.sync, 'function');
  assert.equal(typeof bridge.exposed.value.returnToPortalHosts, 'undefined');
});
