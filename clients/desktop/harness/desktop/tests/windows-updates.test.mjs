import assert from "node:assert/strict";
import { test } from "node:test";
import { EventEmitter } from "node:events";
import { WindowsUpdates } from "../out/windows-updates.js";

// The external updater boundary: the real library is exercised by the loopback tests.
function fixture(version = "1.0.4", options = {}) {
  let installs = 0;
  let downloads = 0;
  const updater = Object.assign(new EventEmitter(), {
    currentVersion: { compare: (v) => "1.0.3".localeCompare(v, undefined, { numeric: true }) },
    setFeedURL() {},
    checkForUpdates: async () => ({ isUpdateAvailable: version !== "1.0.3", updateInfo: {
      version, files: [{ url: `nanoMuse-Desktop-${version}-win-x64.exe`, sha512: "digest", size: 10 }],
    } }),
    downloadUpdate: async () => { downloads++; updater.emit("download-progress", { percent: 42 }); return ["verified.exe"]; },
    quitAndInstall: (silent, reopen) => { assert.equal(silent, true); assert.equal(reopen, true); installs++; },
  });
  const flow = new WindowsUpdates({ currentVersion: "1.0.3", supported: true, createUpdater: () => updater,
    handoff: async (_intent, install) => { install?.(); return true; }, cancelHandoff: async () => undefined, ...options });
  return { flow, updater, counts: () => ({ installs, downloads }) };
}

test("a user checks, downloads with progress and restarts only a verified update", async () => {
  const { flow, updater, counts } = fixture();
  const states = [];
  const off = flow.subscribe((s) => states.push(s));
  await flow.check();
  assert.equal(flow.snapshot().phase, "available");
  assert.equal(updater.autoDownload, false);
  assert.equal(updater.autoInstallOnAppQuit, false);
  assert.deepEqual(counts(), { installs: 0, downloads: 0 });
  await flow.download();
  assert.ok(states.some((s) => s.phase === "downloading" && s.progress === 42));
  assert.equal(flow.snapshot().phase, "ready");
  await flow.restart();
  assert.equal(flow.snapshot().phase, "installing");
  assert.deepEqual(counts(), { installs: 1, downloads: 2 });
  off();
});

test("the running version and a prerelease never offer download or installation", async () => {
  for (const version of ["1.0.3", "1.0.2", "1.0.4-rc.1"]) {
    const { flow, counts } = fixture(version);
    await flow.check();
    assert.equal(flow.snapshot().phase, "current");
    await flow.download();
    await flow.restart();
    assert.deepEqual(counts(), { installs: 0, downloads: 0 });
  }
});

test("startup and six-hour checks share the manual refresh without automatic downloading", async () => {
  let scheduled;
  let cancelled = false;
  const { flow, updater, counts } = fixture("1.0.4", {
    setInterval: (fn, ms) => { assert.equal(ms, 21_600_000); scheduled = fn; return { unref() {} }; },
    clearInterval: () => { cancelled = true; },
  });
  let checks = 0;
  const check = updater.checkForUpdates;
  updater.checkForUpdates = () => { checks++; return check(); };
  flow.start();
  await new Promise((resolve) => setImmediate(resolve));
  assert.equal(checks, 1);
  scheduled();
  await new Promise((resolve) => setImmediate(resolve));
  assert.equal(checks, 2);
  await flow.check();
  assert.equal(checks, 3);
  assert.deepEqual(counts(), { installs: 0, downloads: 0 });
  flow.dispose();
  assert.equal(cancelled, true);
});

test("duplicate downloads and checks during a download do not change the candidate or run twice", async () => {
  const { flow, updater } = fixture();
  await flow.check();
  let release;
  let downloads = 0;
  updater.downloadUpdate = () => { downloads++; return new Promise((resolve) => { release = () => resolve(["verified.exe"]); }); };
  const download = flow.download();
  await new Promise((resolve) => setImmediate(resolve));
  const duplicate = flow.download();
  const check = flow.check();
  assert.equal(flow.snapshot().phase, "downloading");
  release();
  await Promise.all([download, duplicate, check]);
  assert.equal(downloads, 1);
  assert.equal(flow.snapshot().phase, "ready");
});

test("failed checks are recoverable and never reported as current", async () => {
  const { flow, updater } = fixture();
  const check = updater.checkForUpdates;
  updater.checkForUpdates = async () => { throw new Error("offline"); };
  await flow.check();
  assert.equal(flow.snapshot().phase, "failed");
  assert.equal(flow.snapshot().failure, "check");
  updater.checkForUpdates = check;
  await flow.check();
  assert.equal(flow.snapshot().phase, "available");
});

test("development and other platforms never invoke the installer or updater", async () => {
  const { flow, updater, counts } = fixture("1.0.4", { supported: false });
  updater.checkForUpdates = async () => { throw new Error("updater invoked"); };
  await flow.check();
  await flow.download();
  await flow.restart();
  flow.start();
  assert.equal(flow.snapshot().phase, "idle");
  assert.deepEqual(counts(), { installs: 0, downloads: 0 });
});
