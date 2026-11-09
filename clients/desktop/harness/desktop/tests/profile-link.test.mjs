// The profile's plugin link (src/profile-link.ts): a dangling link — the earlier version's
// install folder is gone — must be replaced, not left in the way of `symlinkSync` (0.1.39 on
// Windows: "EEXIST: file already exists, symlink …" at launch). Run after `tsc -p tsconfig.json`.
import assert from "node:assert/strict";
import fs, { lstatSync, mkdirSync, mkdtempSync, readlinkSync, rmSync, symlinkSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { after, test } from "node:test";
import { linkTarget, relink, removeLink, sameTarget } from "../out/profile-link.js";

const roots = [];
after(() => {
  for (const dir of roots) rmSync(dir, { recursive: true, force: true });
});

function scratch() {
  const dir = mkdtempSync(join(tmpdir(), "nm-profile-link-"));
  roots.push(dir);
  return dir;
}

test("a fresh link is made and reported", () => {
  const dir = scratch();
  const target = join(dir, "resources", "dsh-nanomuse");
  mkdirSync(target, { recursive: true });
  const link = join(dir, "node_modules", "dsh-nanomuse");
  mkdirSync(join(dir, "node_modules"));
  assert.equal(relink(link, target), true);
  assert.equal(sameTarget(readlinkSync(link), target), true);
  assert.equal(relink(link, target), false, "a link that already points there is left alone");
});

test("a dangling link from an earlier install is replaced", () => {
  const dir = scratch();
  const old = join(dir, "Programs", "nanoMuse", "nanomuse-desktop", "dsh-nanomuse");
  mkdirSync(old, { recursive: true });
  const link = join(dir, "node_modules", "dsh-nanomuse");
  mkdirSync(join(dir, "node_modules"));
  symlinkSync(old, link, process.platform === "win32" ? "junction" : "dir");
  rmSync(join(dir, "Programs"), { recursive: true, force: true }); // the old version is gone
  assert.equal(sameTarget(linkTarget(link), old), true, "the stale link is still there");
  const target = join(dir, "Programs", "nanomuse-desktop", "dsh-nanomuse");
  mkdirSync(target, { recursive: true });
  assert.equal(relink(link, target), true);
  assert.equal(sameTarget(readlinkSync(link), target), true);
});

test("a real directory in the link's place is removed, a missing one is fine", () => {
  const dir = scratch();
  const target = join(dir, "resources", "dsh-nanomuse");
  mkdirSync(target, { recursive: true });
  const link = join(dir, "node_modules", "dsh-nanomuse");
  mkdirSync(link, { recursive: true });
  writeFileSync(join(link, "index.js"), "");
  assert.equal(linkTarget(link), "(not a link)");
  assert.equal(relink(link, target), true);
  assert.equal(lstatSync(link).isSymbolicLink(), true);
  removeLink(link);
  removeLink(link); // nothing there: no error
  assert.equal(linkTarget(link), undefined);
});

/**
 * Make one `fs` function fail `times` times with `code`, then answer as it did. The compiled
 * module reads the function off the same `node:fs` object at every call, so the patch is
 * what it sees; the returned function puts the original back.
 */
function failing(name, times, code) {
  const original = fs[name];
  let left = times;
  let calls = 0;
  fs[name] = (...args) => {
    calls += 1;
    if (left > 0) {
      left -= 1;
      throw Object.assign(new Error(`${code}: the entry is busy`), { code });
    }
    return original(...args);
  };
  return { restore: () => { fs[name] = original; }, calls: () => calls };
}

test("a removal the file system calls busy for a moment is completed", () => {
  const dir = scratch();
  const target = join(dir, "resources", "dsh-nanomuse");
  mkdirSync(target, { recursive: true });
  const link = join(dir, "node_modules", "dsh-nanomuse");
  mkdirSync(link, { recursive: true });
  writeFileSync(join(link, "index.js"), ""); // a real directory in the link's place
  // the shape of the first start after an in-app update, 1.0.6 on Windows (#618)
  const patch = failing("rmSync", 2, "ENOTEMPTY");
  try {
    assert.equal(relink(link, target), true);
  } finally {
    patch.restore();
  }
  assert.ok(patch.calls() > 2, "the removal was tried again after the busy answers");
  assert.equal(sameTarget(readlinkSync(link), target), true);
});

test("a busy entry that never clears is reported, not waited out forever", () => {
  const dir = scratch();
  const target = join(dir, "resources", "dsh-nanomuse");
  mkdirSync(target, { recursive: true });
  const link = join(dir, "node_modules", "dsh-nanomuse");
  mkdirSync(link, { recursive: true });
  const patch = failing("rmSync", Number.POSITIVE_INFINITY, "EBUSY");
  const started = Date.now();
  try {
    assert.throws(() => relink(link, target), /EBUSY/);
  } finally {
    patch.restore();
  }
  assert.ok(patch.calls() > 1, "it was tried more than once");
  assert.ok(Date.now() - started < 10_000, "the wait is bounded");
});

test("an error that is not the entry being busy is reported at once", () => {
  const dir = scratch();
  const target = join(dir, "resources", "dsh-nanomuse");
  mkdirSync(target, { recursive: true });
  const link = join(dir, "node_modules", "dsh-nanomuse");
  mkdirSync(link, { recursive: true });
  const patch = failing("rmSync", Number.POSITIVE_INFINITY, "EINVAL");
  try {
    assert.throws(() => relink(link, target), /EINVAL/);
  } finally {
    patch.restore();
  }
  assert.equal(patch.calls(), 1, "nothing else might remove it, so it is not tried again");
});

test("a link whose creation is busy for a moment is still made", () => {
  const dir = scratch();
  const target = join(dir, "resources", "dsh-nanomuse");
  mkdirSync(target, { recursive: true });
  const link = join(dir, "node_modules", "dsh-nanomuse");
  mkdirSync(join(dir, "node_modules"));
  const patch = failing("symlinkSync", 2, "EPERM");
  try {
    assert.equal(relink(link, target), true);
  } finally {
    patch.restore();
  }
  assert.equal(sameTarget(readlinkSync(link), target), true);
});

test("targets compare without a trailing separator or the \\\\?\\ prefix", () => {
  assert.equal(sameTarget("/a/b/", "/a/b"), true);
  assert.equal(sameTarget(undefined, "/a/b"), false);
  assert.equal(sameTarget("/a/c", "/a/b"), false);
  if (process.platform === "win32") assert.equal(sameTarget("\\\\?\\C:\\a\\b\\", "C:\\a\\b"), true);
});
