import assert from "node:assert/strict";
import { test } from "node:test";
import { writeFileSync } from "node:fs";
import { updateFeed } from "./helpers/update-feed.mjs";

test("the public update bridge downloads, verifies, reuses the cache and protects restart handoff", async (t) => {
  const demo = await updateFeed(t, { slow: true });
  await demo.flow.check();
  assert.equal(demo.flow.snapshot().phase, "available");
  await demo.flow.download();
  assert.equal(demo.flow.snapshot().phase, "ready");
  assert.ok(demo.states.some((state) => state.phase === "downloading"));
  assert.ok(demo.states.some((state) => state.phase === "verifying"));
  assert.deepEqual(demo.installs, []);
  const requests = demo.seen.filter((path) => path.endsWith(".exe")).length;
  await demo.flow.check();
  assert.equal(demo.flow.snapshot().phase, "ready", "a refresh retains the verified candidate");
  await demo.flow.restart();
  assert.equal(demo.flow.snapshot().phase, "installing");
  assert.equal(demo.seen.filter((path) => path.endsWith(".exe")).length, requests, "the library hashes and reuses the complete cache");
  assert.deepEqual(demo.installs, [{ silent: true, reopen: true, gateOpen: false, busy: [] }]);
});

test("a failed mirror download falls back to the same candidate's GitHub assets", async (t) => {
  const demo = await updateFeed(t);
  await demo.flow.check();
  demo.answers.mirrorPackage = null;
  demo.answers.githubCheck = "1.0.5";
  await demo.flow.download();
  assert.equal(demo.flow.snapshot().phase, "ready");
  assert.equal(demo.flow.snapshot().version, "1.0.4");
  assert.equal(demo.flow.snapshot().source, "github");
  assert.ok(demo.seen.includes("/github/v1.0.4/nanoMuse-Desktop-1.0.4-win-x64.exe"));
  assert.equal(demo.seen.some((path) => path.includes("1.0.5")), false);
});

test("mismatched fallback metadata never downloads a different version", async (t) => {
  const demo = await updateFeed(t);
  await demo.flow.check();
  demo.answers.mirrorPackage = null;
  demo.answers.githubMetadata = "1.0.5";
  await demo.flow.download();
  assert.equal(demo.flow.snapshot().phase, "failed");
  assert.equal(demo.seen.some((path) => path.startsWith("/github/") && path.endsWith(".exe")), false);
  await demo.flow.restart();
  assert.deepEqual(demo.installs, []);
});

test("fallback metadata for the same version must keep the original candidate checksum", async (t) => {
  const demo = await updateFeed(t);
  await demo.flow.check();
  demo.answers.mirrorPackage = null;
  demo.answers.githubDigest = Buffer.alloc(64, 0x32).toString("base64");
  await demo.flow.download();
  assert.equal(demo.flow.snapshot().phase, "failed");
  assert.equal(demo.seen.some((path) => path.startsWith("/github/") && path.endsWith(".exe")), false);
  assert.deepEqual(demo.installs, []);
});

test("corrupt packages fail integrity verification and a manual retry can recover", async (t) => {
  const demo = await updateFeed(t);
  await demo.flow.check();
  demo.answers.mirrorPackage = Buffer.alloc(demo.bytes.length, 0x32);
  demo.answers.githubPackage = demo.answers.mirrorPackage;
  await demo.flow.download();
  assert.equal(demo.flow.snapshot().phase, "failed");
  assert.equal(demo.flow.snapshot().failure, "download");
  await demo.flow.restart();
  assert.deepEqual(demo.installs, []);
  demo.answers.mirrorPackage = demo.bytes;
  await demo.flow.download();
  assert.equal(demo.flow.snapshot().phase, "ready");
});

test("a tampered ready cache is revalidated before restart and cannot be installed", async (t) => {
  const demo = await updateFeed(t);
  await demo.flow.check();
  await demo.flow.download();
  writeFileSync(demo.cacheFiles.at(-1), Buffer.alloc(demo.bytes.length, 0x32));
  demo.answers.mirrorPackage = null;
  demo.answers.githubPackage = null;
  await demo.flow.restart();
  assert.equal(demo.flow.snapshot().phase, "failed");
  assert.deepEqual(demo.installs, []);
  assert.equal(demo.gateOpen(), true);
});

test("keeping busy work running cancels restart and restores normal work admission", async (t) => {
  const demo = await updateFeed(t, { busy: [{ kind: "reply", label: "" }, { kind: "approval", label: "" }] });
  await demo.flow.check();
  await demo.flow.download();
  await demo.flow.restart();
  assert.equal(demo.flow.snapshot().phase, "ready");
  assert.equal(demo.gateOpen(), true);
  assert.deepEqual(demo.installs, []);
});

test("confirmed work finishes before restart, and a stop failure never installs", async (t) => {
  const demo = await updateFeed(t, { confirm: true, busy: [{ kind: "task", label: "Phone" }, { kind: "computer", label: "" }] });
  await demo.flow.check();
  await demo.flow.download();
  demo.answers.stopError = true;
  await demo.flow.restart();
  assert.equal(demo.gateOpen(), true);
  assert.deepEqual(demo.installs, []);
  demo.answers.stopError = false;
  await demo.flow.restart();
  assert.deepEqual(demo.installs, [{ silent: true, reopen: true, gateOpen: false, busy: [] }]);
});

test("an installer start error is a failure and restores the work gate", async (t) => {
  const demo = await updateFeed(t);
  await demo.flow.check();
  await demo.flow.download();
  demo.answers.installError = true;
  await demo.flow.restart();
  assert.equal(demo.flow.snapshot().phase, "failed");
  assert.equal(demo.flow.snapshot().failure, "restart");
  assert.equal(demo.gateOpen(), true);
  assert.deepEqual(demo.installs, []);
});

test("an asynchronous installer error restores the gate before offering a retry", async (t) => {
  const demo = await updateFeed(t);
  await demo.flow.check();
  await demo.flow.download();
  demo.answers.installError = "late";
  await demo.flow.restart();
  await new Promise((resolve) => setImmediate(resolve));
  assert.equal(demo.flow.snapshot().phase, "failed");
  assert.equal(demo.flow.snapshot().failure, "restart");
  assert.equal(demo.gateOpen(), true);
  assert.deepEqual(demo.installs, []);
});
