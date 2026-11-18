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
    'closeHostWorkbenchEmbed', 'codexAddProfile', 'codexAuthorizeProfile', 'codexAuthorizeThread', 'codexListThreads', 'codexRevoke', 'codexRevokeProfile', 'codexSelectProfile', 'codexState', 'codexUpdateScopes',
    'deletePreserved', 'legacyDiscardSync', 'legacyPauseSync', 'legacyResumeSync', 'legacyStopLiveSync', 'legacyStopSync', 'legacySyncState',
    'onCodexState', 'onHostWorkbenchMessage', 'onState', 'onSync', 'openHostWorkbenchEmbed', 'preserved', 'projectUploadTargets',
    'restorePreserved', 'selectProjectDirectory', 'sendHostWorkbenchCommand', 'setHostWorkbenchBounds', 'state', 'stopProjectSync', 'uploadProject',
  ]);
  await bridge.exposed.value.projectUploadTargets();
  await bridge.exposed.value.selectProjectDirectory('h1234567', 'w1234567');
  await bridge.exposed.value.uploadProject('selection', 'project');
  await bridge.exposed.value.stopProjectSync('selection');
  await bridge.exposed.value.state();
  await bridge.exposed.value.legacySyncState();
  await bridge.exposed.value.restorePreserved('w1234567', 'a'.repeat(32));
  await bridge.exposed.value.codexAuthorizeProfile('profile-a', ['read', 'live']);
  await bridge.exposed.value.codexRevokeProfile('profile-a');
  const embedRequest = { hostID: 'h1234567', conversationID: 'c1234567', initialTarget: 'browser', targets: ['browser'], shared: false };
  const bounds = { x: 10, y: 20, width: 640, height: 480 };
  const command = { type: 'visibility', conversation_id: 'c1234567', visible: true };
  const selectTarget = { type: 'select_target', target: 'terminal', seq: 1 };
  const controlLock = { type: 'control_lock', locked: true, seq: 1 };
  await bridge.exposed.value.openHostWorkbenchEmbed(embedRequest);
  await bridge.exposed.value.setHostWorkbenchBounds('nonce-1', bounds);
  await bridge.exposed.value.sendHostWorkbenchCommand('nonce-1', command);
  await bridge.exposed.value.sendHostWorkbenchCommand('nonce-1', selectTarget);
  await bridge.exposed.value.sendHostWorkbenchCommand('nonce-1', controlLock);
  await bridge.exposed.value.closeHostWorkbenchEmbed('nonce-1');
  assert.deepEqual(bridge.calls, [
    ['morde:portal-project-upload-targets'],
    ['morde:portal-project-select-directory', 'h1234567', 'w1234567'],
    ['morde:portal-project-upload', 'selection', 'project'],
    ['morde:portal-project-stop', 'selection'],
    ['morde:portal-state'],
    ['morde:portal-legacy-state'],
    ['morde:portal-preserved-restore', 'w1234567', 'a'.repeat(32)],
    ['morde:codex-authorize-profile', 'profile-a', ['read', 'live']],
    ['morde:codex-revoke-profile', 'profile-a'],
    ['morde:portal-workbench-open', embedRequest],
    ['morde:portal-workbench-bounds', 'nonce-1', bounds],
    ['morde:portal-workbench-command', 'nonce-1', command],
    ['morde:portal-workbench-command', 'nonce-1', selectTarget],
    ['morde:portal-workbench-command', 'nonce-1', controlLock],
    ['morde:portal-workbench-close', 'nonce-1'],
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

  const workbenchMessages = [];
  const unsubscribeWorkbench = bridge.exposed.value.onHostWorkbenchMessage((value) => workbenchMessages.push(value));
  const workbenchMessage = { type: 'ready', nonce: 'nonce-1' };
  bridge.listeners.get('morde:portal-workbench-message')({}, workbenchMessage);
  assert.deepEqual(workbenchMessages, [workbenchMessage]);
  unsubscribeWorkbench();
  assert.equal(bridge.removed.at(-1)[0], 'morde:portal-workbench-message');
});

test('Codex settings preload exposes profile connection consent and revocation', async () => {
  const bridge = loadBridge('codex');
  assert.deepEqual(Object.keys(bridge.exposed.value).sort(), [
    'addProfile', 'authorizeProfile', 'authorizeThread', 'listThreads', 'onState', 'revoke', 'revokeProfile', 'selectProfile', 'state', 'updateScopes',
  ]);
  await bridge.exposed.value.authorizeProfile('profile-a', ['read', 'live']);
  await bridge.exposed.value.revokeProfile('profile-a');
  assert.deepEqual(bridge.calls, [
    ['morde:codex-authorize-profile', 'profile-a', ['read', 'live']],
    ['morde:codex-revoke-profile', 'profile-a'],
  ]);
});

test('Host Workbench preload exposes return and bounded state signaling methods', async () => {
  const bridge = loadBridge('host-workbench');
  assert.deepEqual(Object.keys(bridge.exposed.value).sort(), [
    'hostWorkbenchReady', 'onHostWorkbenchMessage', 'returnToPortalHosts', 'sendHostWorkbenchState',
  ]);
  const state = { control_state: 'ai', control_lease_active: false };
  await bridge.exposed.value.returnToPortalHosts();
  await bridge.exposed.value.hostWorkbenchReady('nonce-1');
  await bridge.exposed.value.sendHostWorkbenchState(state);
  assert.deepEqual(bridge.calls, [
    ['morde:return-to-portal-hosts'],
    ['morde:host-workbench-ready', 'nonce-1'],
    ['morde:host-workbench-state', state],
  ]);
  const messages = [];
  const unsubscribe = bridge.exposed.value.onHostWorkbenchMessage((value) => messages.push(value));
  const message = { type: 'init', nonce: 'nonce-1' };
  bridge.listeners.get('morde:host-workbench-message')({}, message);
  assert.deepEqual(messages, [message]);
  unsubscribe();
  assert.deepEqual(bridge.removed.map(([channel]) => channel), ['morde:host-workbench-message']);
});

test('main window preload retains its existing desktop API', () => {
  const bridge = loadBridge(undefined);
  assert.equal(bridge.exposed.name, 'morde');
  assert.equal(typeof bridge.exposed.value.login, 'function');
  assert.equal(typeof bridge.exposed.value.sync, 'function');
  assert.equal(typeof bridge.exposed.value.returnToPortalHosts, 'undefined');
});
