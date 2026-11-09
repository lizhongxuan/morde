import assert from "node:assert/strict";
import { test, before, after } from "node:test";
import { createRequire, Module } from "node:module";
import { EventEmitter } from "node:events";
import { writeFileSync } from "node:fs";
import { updateFeed } from "./helpers/update-feed.mjs";

// Keep the real quitAndInstall, install and NSIS argument/fallback logic. The
// Electron event emitter, process spawn and app quit are the external boundaries.
const require = createRequire(import.meta.url);
const electronPath = require.resolve("electron");
const original = require.cache[electronPath];
before(() => {
  const stub = new Module(electronPath);
  stub.loaded = true;
  stub.exports = { autoUpdater: new EventEmitter() };
  require.cache[electronPath] = stub;
});
after(() => {
  if (original) require.cache[electronPath] = original;
  else delete require.cache[electronPath];
});

test("real quitAndInstall cannot quit after an asynchronous installer spawn failure", async (t) => {
  const demo = await updateFeed(t, { realInstall: true });
  await demo.flow.check();
  await demo.flow.download();
  demo.answers.installError = true;
  await demo.flow.restart();
  await new Promise((resolve) => setImmediate(resolve));
  assert.equal(demo.flow.snapshot().phase, "failed");
  assert.equal(demo.gateOpen(), true);
  assert.deepEqual(demo.installs, []);
  assert.deepEqual(demo.quits, [], "the library's queued quit must not stop the Host after a failed spawn");
  demo.answers.installError = false;
  await demo.flow.download();
  await demo.flow.restart();
  await new Promise((resolve) => setImmediate(resolve));
  assert.deepEqual(demo.quits, [{ gateOpen: false, busy: [] }], "a manual retry can complete a new protected handoff");
});

test("real quitAndInstall hands a verified update to NSIS with reopen and a closed gate", async (t) => {
  const demo = await updateFeed(t, { realInstall: true });
  await demo.flow.check();
  await demo.flow.download();
  await demo.flow.restart();
  await new Promise((resolve) => setImmediate(resolve));
  assert.equal(demo.flow.snapshot().phase, "installing");
  assert.deepEqual(demo.installs, [{ args: ["--updated", "/S", "--force-run"], gateOpen: false, busy: [] }]);
  assert.deepEqual(demo.quits, [{ gateOpen: false, busy: [] }]);
});

test("a real user quit installs a verified update without reopening the app", async (t) => {
  const demo = await updateFeed(t, { realInstall: true });
  await demo.flow.check();
  await demo.flow.download();
  await demo.flow.check();
  await new Promise((resolve) => setImmediate(resolve));
  assert.deepEqual(demo.installs, [], "a ready cache and an idle refresh are not an installation request");
  await demo.flow.quit();
  await new Promise((resolve) => setImmediate(resolve));
  assert.deepEqual(demo.installs, [{ args: ["--updated", "/S"], gateOpen: false, busy: [] }]);
  assert.deepEqual(demo.quits, [{ gateOpen: false, busy: [] }]);
});

test("a quit installer spawn failure keeps the app running and permits a restart retry", async (t) => {
  const demo = await updateFeed(t, { realInstall: true });
  await demo.flow.check();
  await demo.flow.download();
  demo.answers.installError = true;
  await demo.flow.quit();
  await new Promise((resolve) => setImmediate(resolve));
  assert.equal(demo.flow.snapshot().phase, "failed");
  assert.equal(demo.gateOpen(), true);
  assert.deepEqual(demo.quits, []);
  demo.answers.installError = false;
  await demo.flow.download();
  await demo.flow.restart();
  await new Promise((resolve) => setImmediate(resolve));
  assert.deepEqual(demo.installs, [{ args: ["--updated", "/S", "--force-run"], gateOpen: false, busy: [] }]);
});

test("a tampered ready cache cannot install or quit when neither source repairs it", async (t) => {
  const demo = await updateFeed(t, { realInstall: true });
  await demo.flow.check();
  await demo.flow.download();
  writeFileSync(demo.cacheFiles.at(-1), Buffer.alloc(demo.bytes.length, 0x32));
  demo.answers.mirrorPackage = null;
  demo.answers.githubPackage = null;
  await demo.flow.quit();
  assert.equal(demo.flow.snapshot().phase, "failed");
  assert.equal(demo.gateOpen(), true);
  assert.deepEqual(demo.installs, []);
  assert.deepEqual(demo.quits, []);
});

test("the first quit or restart intent owns a single installation even during cache verification", async (t) => {
  for (const first of ["quit", "restart"]) {
    const demo = await updateFeed(t, { realInstall: true });
    await demo.flow.check();
    await demo.flow.download();
    const pending = demo.flow[first]();
    const other = demo.flow[first === "quit" ? "restart" : "quit"]();
    await Promise.all([pending, other, demo.flow.quit(), demo.flow.restart()]);
    await new Promise((resolve) => setImmediate(resolve));
    assert.equal(demo.installs.length, 1);
    assert.equal(demo.installs[0].args.includes("--force-run"), first === "restart");
    assert.equal(demo.quits.length, 1);
  }
});

test("busy quit defaults to continuing each kind of work and restores admission", async (t) => {
  for (const kind of ["reply", "computer", "task", "approval"]) {
    const busy = [{ kind, label: "" }];
    const demo = await updateFeed(t, { realInstall: true, busy });
    await demo.flow.check();
    await demo.flow.download();
    await demo.flow.quit();
    assert.deepEqual(demo.questions, [{ items: busy, intent: "quit" }]);
    assert.equal(demo.flow.snapshot().phase, "ready");
    assert.equal(demo.gateOpen(), true);
    assert.deepEqual(demo.installs, []);
    assert.deepEqual(demo.quits, []);
  }
});

test("confirmed quit waits for work to end with the gate closed before NSIS runs", async (t) => {
  const demo = await updateFeed(t, { realInstall: true, confirm: true, busy: [{ kind: "task", label: "" }] });
  await demo.flow.check();
  await demo.flow.download();
  let release;
  const ended = new Promise((resolve) => { release = resolve; });
  const stop = demo.work.stopAll;
  demo.work.stopAll = async () => { await ended; await stop(); };
  const pending = demo.flow.quit();
  while (!demo.questions.length) await new Promise((resolve) => setImmediate(resolve));
  assert.equal(demo.gateOpen(), false);
  assert.deepEqual(demo.installs, []);
  assert.deepEqual(demo.quits, []);
  release();
  await pending;
  await new Promise((resolve) => setImmediate(resolve));
  assert.deepEqual(demo.installs, [{ args: ["--updated", "/S"], gateOpen: false, busy: [] }]);
});

test("unknown work, a failed stop or work remaining at the final check cancels quit installation", async (t) => {
  for (const failure of ["unknown", "gate", "stop", "remaining", "new work"]) {
    const demo = await updateFeed(t, { realInstall: true, confirm: true, busy: [{ kind: "computer", label: "" }] });
    await demo.flow.check();
    await demo.flow.download();
    if (failure === "unknown") demo.work.busy = async () => { throw new Error("Host did not acknowledge work state"); };
    if (failure === "gate") {
      const gate = demo.work.setGate;
      demo.work.setGate = async (open) => { if (!open) throw new Error("Host did not close the gate"); await gate(open); };
    }
    if (failure === "stop") demo.answers.stopError = true;
    if (failure === "remaining") demo.work.stopAll = async () => undefined;
    if (failure === "new work") {
      let stopped = false;
      demo.work.busy = async () => [{ kind: stopped ? "approval" : "computer", label: "" }];
      demo.work.stopAll = async () => { stopped = true; };
    }
    await demo.flow.quit();
    assert.equal(demo.flow.snapshot().phase, "ready");
    assert.equal(demo.gateOpen(), true);
    assert.deepEqual(demo.installs, []);
    assert.deepEqual(demo.quits, []);
  }
});

test("quit without a ready package keeps normal exit compatibility and never installs", async (t) => {
  for (const phase of ["idle", "current", "available", "failed", "downloading"]) {
    const demo = await updateFeed(t, { realInstall: true, slow: phase === "downloading", current: phase === "current" ? "1.0.4" : "1.0.3" });
    let pending;
    if (phase !== "idle") await demo.flow.check();
    if (phase === "failed") {
      demo.answers.mirrorPackage = null;
      demo.answers.githubPackage = null;
      await demo.flow.download();
    }
    if (phase === "downloading") {
      pending = demo.flow.download();
      while (demo.flow.snapshot().phase !== "downloading") await new Promise((resolve) => setImmediate(resolve));
    } else assert.equal(demo.flow.snapshot().phase, phase);
    demo.work.busy = async () => { throw new Error("Host offline"); };
    await demo.flow.quit();
    assert.equal(demo.quits.length, 1, "normal quit remains tolerant of an offline Host");
    assert.deepEqual(demo.installs, []);
    await pending;
    assert.deepEqual(demo.installs, []);
  }
});
