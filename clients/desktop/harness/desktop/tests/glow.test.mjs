// The glow's Linux decisions (src/glow.ts): which actions it steps aside for, when the
// input shape is set again, what counts as the X server's word that the shape is gone, and
// when a second instance means a stale copy of the app. Pure functions; no X, no Electron.
// Run after `tsc -p tsconfig.json` (npm test does both).
import assert from "node:assert/strict";
import { test } from "node:test";
import { GLOW_HIDE_MS, GLOW_SETTLE_MS, LEAK_EPISODE_MS, LEAK_GRACE_MS, LeakLog, leakIsEvidence, leakIsReal, pointerAction, REARM_DELAYS_MS, staleInstance } from "../out/glow.js";

test("the glow steps aside for every action that moves or presses the pointer, and for nothing else", () => {
  for (const kind of ["move", "click", "double click", "right click", "middle click", "drag", "scroll up", "scroll down"]) assert.equal(pointerAction(kind), true, kind);
  for (const kind of ["type", "hotkey", "key", "wait", "finished", "call_user", "", "unknown"]) assert.equal(pointerAction(kind), false, kind);
});

test("the shape is set again at once, when the X server has answered, and once more late", () => {
  assert.deepEqual([...REARM_DELAYS_MS], [0, 150, 600]);
  // the operator waits for the middle one before it moves the pointer with the glow up
  assert.ok(REARM_DELAYS_MS.includes(GLOW_SETTLE_MS));
  assert.ok(GLOW_SETTLE_MS > 100, "100 ms was enough in a quiet rig, 50 was not; stay above");
  // the delays are in order, so the promise that settles with GLOW_SETTLE_MS is not the last word
  assert.deepEqual([...REARM_DELAYS_MS].sort((a, b) => a - b), [...REARM_DELAYS_MS]);
  assert.ok(REARM_DELAYS_MS.at(-1) > GLOW_SETTLE_MS);
  // hiding for a pointer action waits for the unmap but not long enough to be felt
  assert.ok(GLOW_HIDE_MS >= 20 && GLOW_HIDE_MS <= 100);
});

test("a pointer event anywhere but the one pixel Electron leaves is a leak", () => {
  assert.equal(leakIsReal({ x: 640, y: 400, type: "pointermove" }), true);
  assert.equal(leakIsReal({ x: 0.5, y: 0, type: "pointerenter" }), true);
  assert.equal(leakIsReal({ x: 1, y: 1, type: "pointerdown" }), true);
  // the one pixel at (0,0) is the shape that is left; events there are expected
  assert.equal(leakIsReal({ x: 0, y: 0, type: "pointermove" }), false);
  assert.equal(leakIsReal({ x: -1, y: -1, type: "pointermove" }), false);
  // only well-formed reports from the page count
  assert.equal(leakIsReal(null), false);
  assert.equal(leakIsReal("640,400"), false);
  assert.equal(leakIsReal({ x: "640", y: 400, type: "pointermove" }), false);
  assert.equal(leakIsReal({ x: Number.NaN, y: 400, type: "pointermove" }), false);
  assert.equal(leakIsReal({ x: 640, y: 400, type: "" }), false);
  assert.equal(leakIsReal({ x: 640, y: 400 }), false);
});

test("the enter and move Chromium makes up after a show are not evidence; a press or a wheel always is", () => {
  // what the rig saw 30–70 ms after every show: pointerenter + pointermove at a stale point, region intact
  assert.equal(leakIsEvidence({ x: 1618, y: 145, type: "pointerenter" }, 30), false);
  assert.equal(leakIsEvidence({ x: 1618, y: 145, type: "pointermove" }, 70), false);
  assert.equal(leakIsEvidence({ x: 1618, y: 145, type: "pointermove" }, LEAK_GRACE_MS - 1), false);
  // the person moving the mouse over a glow whose region is gone, a second after it came up
  assert.equal(leakIsEvidence({ x: 900, y: 500, type: "pointermove" }, LEAK_GRACE_MS), true);
  assert.equal(leakIsEvidence({ x: 900, y: 500, type: "pointerenter" }, 60_000), true);
  // a press or a wheel is the X server's word whenever it comes
  assert.equal(leakIsEvidence({ x: 900, y: 500, type: "pointerdown" }, 0), true);
  assert.equal(leakIsEvidence({ x: 900, y: 500, type: "wheel" }, 10), true);
  // the grace is wider than the synthesized pair's lateness and no wider than the late re-arm needs
  assert.ok(LEAK_GRACE_MS >= 500 && LEAK_GRACE_MS >= REARM_DELAYS_MS.at(-1));
});

test("the watchdog counts every repair but speaks once per episode", () => {
  const seen = new LeakLog();
  assert.equal(seen.record(10_000), true, "the first leak is logged");
  assert.equal(seen.record(10_100), false, "a burst is one episode");
  assert.equal(seen.record(10_000 + LEAK_EPISODE_MS), false, "still the same episode, measured from the last leak");
  assert.equal(seen.record(10_000 + LEAK_EPISODE_MS * 2 + 1), true, "a quiet spell, then a new episode");
  assert.equal(seen.repairs, 4);
  assert.equal(seen.episodes, 2);
});

test("a second instance of another version means this copy is stale", () => {
  assert.equal(staleInstance("0.1.37", { version: "0.1.38" }), true);
  assert.equal(staleInstance("0.1.38", { version: "0.1.37" }), true, "a downgrade is a different build on disk too");
  assert.equal(staleInstance("0.1.38", { version: "0.1.38" }), false, "the launcher clicked again: just show the window");
  // older copies sent nothing with their lock request; they keep the old behaviour
  assert.equal(staleInstance("0.1.38", undefined), false);
  assert.equal(staleInstance("0.1.38", null), false);
  assert.equal(staleInstance("0.1.38", {}), false);
  assert.equal(staleInstance("0.1.38", { version: "" }), false);
  assert.equal(staleInstance("0.1.38", { version: 38 }), false);
});
