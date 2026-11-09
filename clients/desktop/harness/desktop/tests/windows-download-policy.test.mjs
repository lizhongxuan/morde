import assert from "node:assert/strict";
import { test } from "node:test";
import { EventEmitter } from "node:events";
import { mkdtempSync, readFileSync, writeFileSync, rmSync } from "node:fs";
import { join } from "node:path";
import { tmpdir } from "node:os";
import { WindowsUpdates } from "../out/windows-updates.js";

function fixture(options = {}) {
  let downloads = 0;
  let network = "unmetered";
  let fails = false;
  const timers = [];
  const updater = Object.assign(new EventEmitter(), {
    currentVersion: { compare: () => -1 }, setFeedURL() {},
    checkForUpdates: async () => ({ isUpdateAvailable: true, updateInfo: {
      version: "1.0.4", files: [{ url: "nanoMuse-Desktop-1.0.4-win-x64.exe", sha512: "digest", size: 10 }],
    } }),
    downloadUpdate: async () => { downloads++; if (fails) throw new Error("offline"); return ["verified.exe"]; },
  });
  const flow = new WindowsUpdates({ currentVersion: "1.0.3", supported: true,
    createUpdater: () => Object.assign(new EventEmitter(), {
      currentVersion: updater.currentVersion, setFeedURL: (...args) => updater.setFeedURL(...args),
      checkForUpdates: () => updater.checkForUpdates(), downloadUpdate: () => updater.downloadUpdate(),
    }),
    handoff: async () => false, cancelHandoff: async () => undefined,
    networkCost: async () => network,
    setTimeout: (fn, ms) => { const timer = { fn, ms, cancelled: false, unref() {} }; timers.push(timer); return timer; },
    clearTimeout: (timer) => { timer.cancelled = true; }, ...options });
  return { flow, updater, timers, downloads: () => downloads,
    network: (value) => { network = value; }, fail: (value) => { fails = value; } };
}

test("a newer stable update downloads by default only on a known unmetered network", async () => {
  const { flow, downloads } = fixture();
  await flow.check();
  assert.equal(flow.snapshot().autoDownload, true);
  assert.equal(flow.snapshot().phase, "ready");
  assert.equal(downloads(), 1);
});

test("metered and unknown costs allow checks and a manual download without changing the switch", async () => {
  for (const cost of ["metered", "unknown"]) {
    const { flow, network, downloads } = fixture();
    network(cost);
    await flow.check();
    assert.equal(flow.snapshot().phase, "available");
    assert.equal(flow.snapshot().paused, cost);
    assert.equal(downloads(), 0);
    await flow.download();
    assert.equal(flow.snapshot().phase, "ready");
    assert.equal(flow.snapshot().autoDownload, true);
    assert.equal(flow.snapshot().paused, null);
    assert.equal(downloads(), 1);
  }
});

test("a saved disabled switch survives a new controller, still checks and permits manual downloads", async (t) => {
  const dir = mkdtempSync(join(tmpdir(), "nm-update-policy-"));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  const file = join(dir, "setting.json");
  const first = fixture({ saveAutoDownload: (enabled) => writeFileSync(file, JSON.stringify(enabled)) });
  await first.flow.setAutoDownload(false);
  const second = fixture({ autoDownload: JSON.parse(readFileSync(file, "utf8")) });
  await second.flow.check();
  assert.equal(second.flow.snapshot().autoDownload, false);
  assert.equal(second.flow.snapshot().paused, "disabled");
  assert.equal(second.downloads(), 0);
  await second.flow.download();
  assert.equal(second.flow.snapshot().phase, "ready");
  assert.equal(second.flow.snapshot().autoDownload, false);
});

test("a failed save leaves the public switch unchanged", async () => {
  const { flow } = fixture({ saveAutoDownload: () => { throw new Error("disk full"); } });
  await assert.rejects(flow.setAutoDownload(false), /disk full/);
  assert.equal(flow.snapshot().autoDownload, true);
});

const settle = () => new Promise((resolve) => setImmediate(resolve));
async function fire(timer) { if (!timer.cancelled) timer.fn(); await settle(); }

test("download failure retries at one and five minutes only, then waits for manual or later checks", async () => {
  const { flow, fail, downloads, timers } = fixture();
  fail(true);
  await flow.check();
  assert.equal(flow.snapshot().phase, "failed");
  assert.equal(downloads(), 2); // Each attempt tries mirror and GitHub.
  assert.equal(timers[0].ms, 60_000);
  await fire(timers[0]);
  assert.equal(downloads(), 4);
  assert.equal(timers[1].ms, 300_000);
  await fire(timers[1]);
  assert.equal(downloads(), 6);
  assert.equal(timers.length, 2);
  assert.equal(flow.snapshot().failure, "download");
  await flow.check();
  assert.equal(downloads(), 8);
  assert.equal(timers[2].ms, 60_000);
  fail(false);
  await flow.download();
  assert.equal(timers[2].cancelled, true);
  assert.equal(flow.snapshot().phase, "ready");
  assert.equal(downloads(), 9);
});

test("each scheduled retry rechecks changed network cost and does not poll a paused connection", async () => {
  for (const cost of ["metered", "unknown"]) {
    const { flow, fail, downloads, timers, network } = fixture();
    fail(true);
    await flow.check();
    network(cost);
    await fire(timers[0]);
    assert.equal(downloads(), 2);
    assert.equal(flow.snapshot().paused, cost);
    assert.equal(flow.snapshot().phase, "failed");
    assert.equal(timers.length, 1);
    fail(false);
    await flow.download();
    assert.equal(flow.snapshot().phase, "ready");
  }
});

test("disabling cancels scheduled retry and later checks do not start another download", async () => {
  const { flow, fail, downloads, timers } = fixture();
  fail(true);
  await flow.check();
  await flow.setAutoDownload(false);
  assert.equal(timers[0].cancelled, true);
  await fire(timers[0]);
  await flow.check();
  assert.equal(flow.snapshot().paused, "disabled");
  assert.equal(downloads(), 2);
});

test("disabling while Windows reads the cost prevents an automatic start", async () => {
  let release;
  const { flow, downloads } = fixture({ networkCost: () => new Promise((resolve) => { release = resolve; }) });
  const check = flow.check();
  await settle();
  await flow.setAutoDownload(false);
  release("unmetered");
  await check;
  assert.equal(downloads(), 0);
  assert.equal(flow.snapshot().paused, "disabled");
});

test("a manual download while cost detection is pending overrides this request without changing the switch", async () => {
  for (const cost of ["metered", "unknown"]) {
    let release;
    const { flow, downloads } = fixture({ networkCost: () => new Promise((resolve) => { release = resolve; }) });
    const check = flow.check();
    await settle();
    assert.equal(flow.snapshot().phase, "available");
    const manual = flow.download();
    const duplicate = flow.download();
    release(cost);
    await Promise.all([check, manual, duplicate]);
    assert.equal(flow.snapshot().phase, "ready");
    assert.equal(flow.snapshot().autoDownload, true);
    assert.equal(flow.snapshot().paused, null);
    assert.equal(downloads(), 1);
  }
});

test("manual retry during a scheduled cost check is kept even after automatic downloads are disabled", async () => {
  let release;
  let calls = 0;
  const { flow, fail, downloads, timers } = fixture({ networkCost: () => ++calls === 1
    ? Promise.resolve("unmetered") : new Promise((resolve) => { release = resolve; }) });
  fail(true);
  await flow.check();
  timers[0].fn();
  await settle();
  assert.equal(flow.snapshot().phase, "failed");
  await flow.setAutoDownload(false);
  const manual = flow.download();
  const duplicate = flow.download();
  fail(false);
  release("unknown");
  await Promise.all([manual, duplicate]);
  assert.equal(flow.snapshot().phase, "ready");
  assert.equal(flow.snapshot().autoDownload, false);
  assert.equal(flow.snapshot().paused, null);
  assert.equal(downloads(), 3);
  assert.equal(timers.length, 1);
});

test("an unavailable cost API pauses safely and enabling the switch reevaluates the current candidate", async () => {
  const unavailable = fixture({ networkCost: async () => { throw new Error("API unavailable"); } });
  await unavailable.flow.check();
  assert.equal(unavailable.flow.snapshot().paused, "unknown");
  assert.equal(unavailable.downloads(), 0);
  const { flow, downloads } = fixture({ autoDownload: false });
  await flow.check();
  await flow.setAutoDownload(true);
  assert.equal(flow.snapshot().phase, "ready");
  assert.equal(downloads(), 1);
});

test("duplicate requests during automatic download share one package operation; disabling lets that download finish", async () => {
  const { flow, updater, timers } = fixture();
  let release;
  let downloads = 0;
  updater.downloadUpdate = () => { downloads++; return new Promise((resolve) => { release = () => resolve(["verified.exe"]); }); };
  const first = flow.check();
  await settle();
  const duplicate = flow.download();
  const repeatedCheck = flow.check();
  await flow.setAutoDownload(false);
  assert.equal(flow.snapshot().phase, "downloading");
  release();
  await Promise.all([first, duplicate, repeatedCheck]);
  assert.equal(flow.snapshot().phase, "ready");
  assert.equal(flow.snapshot().autoDownload, false);
  assert.equal(timers.length, 0);
  assert.equal(downloads, 1);
});

test("disposing cancels pending retries without installing or downloading again", async () => {
  const { flow, fail, timers, downloads } = fixture();
  fail(true);
  await flow.check();
  flow.dispose();
  await fire(timers[0]);
  await flow.check();
  assert.equal(downloads(), 2);
});

test("the six-hour check resumes after exhausted retries but respects a later disabled switch", async () => {
  let scheduled;
  const { flow, fail, downloads, timers } = fixture({
    setInterval: (fn, ms) => { assert.equal(ms, 21_600_000); scheduled = fn; return { unref() {} }; },
    clearInterval: () => undefined,
  });
  fail(true);
  flow.start();
  await settle();
  await fire(timers[0]);
  await fire(timers[1]);
  assert.equal(downloads(), 6);
  fail(false);
  scheduled();
  await settle();
  assert.equal(downloads(), 7);
  assert.equal(flow.snapshot().phase, "ready");
  await flow.setAutoDownload(false);
  scheduled();
  await settle();
  assert.equal(downloads(), 7);
  flow.dispose();
});
