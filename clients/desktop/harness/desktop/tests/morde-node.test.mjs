import { test } from 'node:test';
import assert from 'node:assert/strict';
import { EventEmitter } from 'node:events';
import { generateKeyPairSync, verify } from 'node:crypto';
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { MordeSyncthingNode, syncthingEnvironment } from '../out/morde-node.js';

const deviceID = 'ABCDEFG-ABCDEFG-ABCDEFG-ABCDEFG-ABCDEFG-ABCDEFG-ABCDEFG-ABCDEFG';

class FakeChild extends EventEmitter {
  exitCode = null;
  signalCode = null;
  kill(signal = 'SIGTERM') {
    if (this.exitCode !== null) return false;
    this.exitCode = 0;
    this.signalCode = signal;
    queueMicrotask(() => this.emit('exit', 0, signal));
    return true;
  }
}

function fakeNode(home, fetcher = async () => new Response(JSON.stringify({ myID: deviceID }))) {
  const spawned = [];
  const node = new MordeSyncthingNode(() => '/tmp/morde-syncthing', (_binary, args, options) => {
    const child = new FakeChild();
    spawned.push({ args, options, child });
    if (args.includes('generate')) {
      child.exitCode = 0;
      queueMicrotask(() => child.emit('exit', 0, null));
    }
    return child;
  }, fetcher, async () => 43129, () => ({ PATH: '/usr/bin', STGUIAPIKEY: 'inherited-key', STPROFILER: '0.0.0.0:6060' }));
  return { node, spawned };
}

test('the Syncthing environment strips inherited ST settings and keeps control on loopback', () => {
  const env = syncthingEnvironment({ PATH: '/usr/bin', STGUIAPIKEY: 'old-key', STPROFILER: '0.0.0.0:6060', STGUIASSETS: '/tmp/gui' }, 'http://127.0.0.1:43129', 'new-key');
  assert.deepEqual(env, { PATH: '/usr/bin', STGUIADDRESS: 'http://127.0.0.1:43129', STGUIAPIKEY: 'new-key' });
});

test('the node initializes safely, keeps the REST key out of argv, and stops before releasing its folder lock', async () => {
  const home = mkdtempSync(join(tmpdir(), 'morde-node-test-'));
  const fetchCalls = [];
  const { node, spawned } = fakeNode(home, async (url, options) => {
    fetchCalls.push({ url: String(url), options });
    return new Response(JSON.stringify({ myID: deviceID }));
  });
  let unexpectedExit = 0;
  try {
    assert.equal(await node.start(home, () => { unexpectedExit++; }), deviceID);
    assert.equal(spawned.length, 2);
    assert.ok(spawned[0].args.includes('generate'));
    assert.ok(spawned[0].args.includes('--morde-desktop'));
    assert.ok(spawned[1].args.includes('serve'));
    assert.ok(spawned[1].args.includes('--paused'));
    assert.equal(spawned[1].args.some((value) => value.includes(spawned[1].options.env.STGUIAPIKEY)), false);
    assert.equal(spawned[1].options.env.STPROFILER, undefined);
    assert.match(fetchCalls[0].url, /^http:\/\/127\.0\.0\.1:43129\/rest\/system\/status$/);
    assert.equal(fetchCalls[0].options.headers['X-API-Key'], spawned[1].options.env.STGUIAPIKEY);
    assert.equal(await node.stop(), true);
    assert.equal(node.mayOwnWorkspace(), false);
    assert.equal(unexpectedExit, 0);
  } finally { await node.stop(); rmSync(home, { recursive: true, force: true }); }
});

test('device proofs are signed from the private key file and local REST errors do not echo secrets', async () => {
  const home = mkdtempSync(join(tmpdir(), 'morde-node-proof-test-'));
  const { privateKey, publicKey } = generateKeyPairSync('ed25519', { privateKeyEncoding: { type: 'pkcs8', format: 'pem' }, publicKeyEncoding: { type: 'spki', format: 'pem' } });
  writeFileSync(join(home, 'key.pem'), privateKey, { mode: 0o600 });
  const secret = 'super-secret-relay-token';
  const { node } = fakeNode(home, async (url) => String(url).endsWith('/rest/system/status')
    ? new Response(JSON.stringify({ myID: deviceID }))
    : new Response(secret, { status: 403 }));
  try {
    await node.start(home);
    const payload = Buffer.from('morde workspace sync registration proof');
    const signature = await node.signDeviceProof(payload);
    assert.equal(verify(null, payload, publicKey, signature), true);
    await assert.rejects(() => node.request('/rest/config/system'), (error) => !error.message.includes(secret));
    await assert.rejects(() => node.request('//attacker.example/rest/system/status'), /unavailable/);
  } finally { await node.stop(); rmSync(home, { recursive: true, force: true }); }
});

test('managed folders use only explicit relay peers, and paused workspaces stay disconnected', async () => {
  const home = mkdtempSync(join(tmpdir(), 'morde-node-config-test-'));
  const serverDeviceID = 'BCDEFGH-BCDEFGH-BCDEFGH-BCDEFGH-BCDEFGH-BCDEFGH-BCDEFGH-BCDEFGH';
  const token = 'r'.repeat(40);
  let options = {
    listenAddresses: [''], globalAnnounceServers: ['default'], globalAnnounceEnabled: false,
    localAnnounceEnabled: false, relaysEnabled: false, natEnabled: false, announceLANAddresses: false,
    crashReportingEnabled: false, autoUpgradeIntervalH: 0, startBrowser: false,
  };
  let folders = [];
  let devices = [{ deviceID, name: 'local node', addresses: ['dynamic'] }];
  const calls = [];
  const reply = (value) => new Response(JSON.stringify(value));
  const { node } = fakeNode(home, async (url, request) => {
    const path = new URL(url).pathname;
    calls.push([request.method, path, request.body]);
    if (path === '/rest/system/status') return reply({ myID: deviceID });
    if (path === '/rest/config/options' && request.method === 'GET') return reply(options);
    if (path === '/rest/config/options' && request.method === 'PATCH') { options = { ...options, ...JSON.parse(request.body) }; return reply(options); }
    if (path === '/rest/config/folders' && request.method === 'GET') return reply(folders);
    if (path === '/rest/config/folders' && request.method === 'PUT') { folders = JSON.parse(request.body); return reply({}); }
    if (path === '/rest/config/devices' && request.method === 'GET') return reply(devices);
    if (path === '/rest/config/devices' && request.method === 'PUT') { devices = JSON.parse(request.body); return reply({}); }
    if (path === '/rest/config/defaults/folder') return reply({ id: '', path: '', devices: [], filesystemType: 'basic', type: 'sendreceive' });
    if (path === '/rest/config/defaults/device') return reply({ deviceID: '', name: '', addresses: ['dynamic'], autoAcceptFolders: false });
    if (path === '/rest/system/resume') return new Response(null, { status: 200 });
    return new Response('unexpected endpoint', { status: 404 });
  });
  const base = {
    workspaceID: 'w1234567', folderID: 'morde-0123456789abcdef0123456789abcdef', directory: '/tmp/workspace-a',
    bindingState: 'active', serverDeviceID, relayAddress: `relay://relay.example.test:22067/?id=${serverDeviceID}&token=${token}`,
  };
  try {
    await node.start(home);
    await node.configureWorkspaces([{ ...base, enabled: true }]);
    assert.equal(options.relaysEnabled, true);
    assert.equal(folders[0].paused, false);
    assert.equal(folders[0].filesystemType, 'basic');
    assert.equal(folders[0].mordeMaxSingleFileBytes, 314572800);
    assert.deepEqual(folders[0].devices.map((item) => item.deviceID).sort(), [deviceID, serverDeviceID].sort());
    assert.deepEqual(devices.find((item) => item.deviceID === serverDeviceID).addresses, [base.relayAddress]);

    await node.configureWorkspaces([{ ...base, enabled: false }]);
    assert.equal(options.relaysEnabled, false);
    assert.equal(folders[0].paused, true);
    assert.deepEqual(folders[0].devices, [{ deviceID }]);
    assert.deepEqual(devices.map((item) => item.deviceID), [deviceID]);
    assert.equal(calls.filter(([, path]) => path === '/rest/system/resume').length, 2);
  } finally { await node.stop(); rmSync(home, { recursive: true, force: true }); }
});

test('managed folders fail closed when public network defaults are enabled', async () => {
  const home = mkdtempSync(join(tmpdir(), 'morde-node-unsafe-config-test-'));
  let wroteConfig = false;
  const { node } = fakeNode(home, async (url, request) => {
    const path = new URL(url).pathname;
    if (path === '/rest/system/status') return new Response(JSON.stringify({ myID: deviceID }));
    if (path === '/rest/config/options') return new Response(JSON.stringify({
      listenAddresses: [''], globalAnnounceEnabled: true, localAnnounceEnabled: false, natEnabled: false,
      announceLANAddresses: false, crashReportingEnabled: false, autoUpgradeIntervalH: 0, startBrowser: false,
    }));
    if (request.method === 'PUT' || request.method === 'PATCH') wroteConfig = true;
    return new Response(JSON.stringify([]));
  });
  try {
    await node.start(home);
    await assert.rejects(() => node.configureWorkspaces([]), /private-node defaults/);
    assert.equal(wroteConfig, false);
  } finally { await node.stop(); rmSync(home, { recursive: true, force: true }); }
});

test('the node refuses to resume when Syncthing did not retain the explicit relay peer', async () => {
  const home = mkdtempSync(join(tmpdir(), 'morde-node-peer-readback-test-'));
  const serverDeviceID = 'BCDEFGH-BCDEFGH-BCDEFGH-BCDEFGH-BCDEFGH-BCDEFGH-BCDEFGH-BCDEFGH';
  const base = {
    workspaceID: 'w1234567', folderID: 'morde-0123456789abcdef0123456789abcdef', directory: '/tmp/workspace-a',
    bindingState: 'active', enabled: true, serverDeviceID,
    relayAddress: `relay://relay.example.test:22067/?id=${serverDeviceID}&token=${'r'.repeat(40)}`,
  };
  const options = {
    listenAddresses: [''], globalAnnounceServers: ['default'], globalAnnounceEnabled: false,
    localAnnounceEnabled: false, relaysEnabled: false, natEnabled: false, announceLANAddresses: false,
    crashReportingEnabled: false, autoUpgradeIntervalH: 0, startBrowser: false,
  };
  let configuredOptions = { ...options };
  let devices = [{ deviceID, name: 'local node', addresses: ['dynamic'] }];
  let resumed = false;
  const reply = (value) => new Response(JSON.stringify(value));
  const { node } = fakeNode(home, async (url, request) => {
    const path = new URL(url).pathname;
    if (path === '/rest/system/status') return reply({ myID: deviceID });
    if (path === '/rest/config/options' && request.method === 'GET') return reply(configuredOptions);
    if (path === '/rest/config/options' && request.method === 'PATCH') { configuredOptions = { ...configuredOptions, ...JSON.parse(request.body) }; return reply(configuredOptions); }
    if (path === '/rest/config/devices' && request.method === 'GET') return reply(devices);
    if (path === '/rest/config/devices' && request.method === 'PUT') { devices = JSON.parse(request.body).filter((item) => item.deviceID === deviceID); return reply({}); }
    if (path === '/rest/config/folders' && request.method === 'GET') return reply([]);
    if (path === '/rest/config/folders' && request.method === 'PUT') return reply({});
    if (path === '/rest/config/defaults/folder') return reply({ id: '', path: '', devices: [], filesystemType: 'basic', type: 'sendreceive' });
    if (path === '/rest/config/defaults/device') return reply({ deviceID: '', name: '', addresses: ['dynamic'], autoAcceptFolders: false });
    if (path === '/rest/system/resume') { resumed = true; return new Response(null, { status: 204 }); }
    return new Response('unexpected endpoint', { status: 404 });
  });
  try {
    await node.start(home);
    await assert.rejects(() => node.configureWorkspaces([base]), /did not apply/);
    assert.equal(resumed, false);
  } finally { await node.stop(); rmSync(home, { recursive: true, force: true }); }
});

test('Workspace status reads a validated aggregate snapshot for only the requested folder', async () => {
  const home = mkdtempSync(join(tmpdir(), 'morde-node-status-test-'));
  const folderID = 'morde-0123456789abcdef0123456789abcdef';
  const calls = [];
  const reply = (value) => new Response(JSON.stringify(value));
  const { node } = fakeNode(home, async (url) => {
    const parsed = new URL(url);
    calls.push(parsed);
    if (parsed.pathname === '/rest/system/status') return reply({ myID: deviceID });
    if (parsed.pathname === '/rest/db/status') return reply({ state: 'syncing', needBytes: 4096, needFiles: 2, pullErrors: 1, localPath: '/private/workspace' });
    return new Response('unexpected endpoint', { status: 404 });
  });
  try {
    await node.start(home);
    assert.deepEqual(await node.workspaceStatus(folderID), { state: 'syncing', needBytes: 4096, needFiles: 2, pullErrors: 1 });
    assert.equal(calls.at(-1).pathname, '/rest/db/status');
    assert.equal(calls.at(-1).searchParams.get('folder'), folderID);
    await assert.rejects(() => node.workspaceStatus('../private'), /folder ID is invalid/);
    assert.equal(calls.filter((url) => url.pathname === '/rest/db/status').length, 1);
  } finally { await node.stop(); rmSync(home, { recursive: true, force: true }); }
});

test('Workspace status rejects malformed Syncthing counters', async () => {
  const home = mkdtempSync(join(tmpdir(), 'morde-node-invalid-status-test-'));
  const reply = (value) => new Response(JSON.stringify(value));
  const { node } = fakeNode(home, async (url) => {
    const path = new URL(url).pathname;
    if (path === '/rest/system/status') return reply({ myID: deviceID });
    if (path === '/rest/db/status') return reply({ state: 'syncing', needBytes: -1, needFiles: 2, pullErrors: 0 });
    return new Response('unexpected endpoint', { status: 404 });
  });
  try {
    await node.start(home);
    await assert.rejects(() => node.workspaceStatus('morde-0123456789abcdef0123456789abcdef'), /Invalid Syncthing Workspace status/);
  } finally { await node.stop(); rmSync(home, { recursive: true, force: true }); }
});
