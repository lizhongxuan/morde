// The Morde installer must carry only the Morde runtime, its sync child, and legal notices.
import assert from "node:assert/strict";
import { existsSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";

const here = new URL("..", import.meta.url);
// read with LF line ends whatever the checkout wrote (a Windows runner with autocrlf writes CRLF)
const config = readFileSync(new URL("electron-builder.yml", here), "utf8").replace(/\r\n/g, "\n");

/** The lines of one top-level block of the YAML, by its key. */
function block(key) {
  const lines = config.split("\n");
  const start = lines.findIndex((line) => line === `${key}:`);
  assert.notEqual(start, -1, `electron-builder.yml has a ${key} block`);
  const body = [];
  for (const line of lines.slice(start + 1)) {
    if (line && !line.startsWith(" ") && !line.startsWith("#")) break;
    body.push(line);
  }
  return body;
}

test("the installer includes only the Morde desktop runtime", () => {
  const files = block("files").filter((line) => line.trim() && !line.trim().startsWith("#"));
  assert.deepEqual(files, [
    "  - out/morde-*.js",
    "  - resources/morde.html",
    "  - resources/morde.css",
    "  - resources/morde.js",
    "  - resources/morde-icon.png",
    "  - package.json",
  ]);
  const extra = block("extraResources").filter((line) => line.trim() && !line.trim().startsWith("#"));
  assert.ok(extra.includes('    filter: ["morde-sync", "morde-sync.exe", "morde-sync.sha256", "morde-syncthing", "morde-syncthing.exe", "morde-syncthing.sha256"]'));
  assert.ok(extra.includes("  - from: resources/THIRD_PARTY_NOTICES.morde-sync.md"));
  assert.ok(extra.includes("    to: legal/THIRD_PARTY_NOTICES.morde-sync.md"));
  assert.ok(extra.includes("  - from: resources/LICENSE.syncthing.txt"));
  assert.ok(extra.includes("    to: legal/LICENSE.syncthing.txt"));
  assert.ok(extra.includes("  - from: resources/THIRD_PARTY_NOTICES.syncthing.md"));
  assert.ok(extra.includes("    to: legal/THIRD_PARTY_NOTICES.syncthing.md"));
  assert.ok(!extra.some((line) => /dsh\/|runtime\/|nanomuse-desktop/.test(line)));
  const metadata = JSON.parse(readFileSync(new URL("package.json", here), "utf8"));
  assert.equal(metadata.description, "Morde desktop client");
});

test("the Windows build has no upstream updater feed", () => {
  const win = block("win").filter((line) => !line.trim().startsWith("#"));
  assert.ok(config.split("\n").includes("publish: null"), "automatic publishing is disabled");
  assert.ok(!win.some((line) => line.trim().startsWith("publish:")), "Windows has no update feed");
  assert.ok(!config.includes("nanomuse.cn"), "no upstream download URL remains");
});

test("the Linux x64 package uses the Portal download manifest name", () => {
  assert.ok(block("linux").includes("  artifactName: morde-desktop-${version}-linux-x64.${ext}"));
});

test("the whitelisted Morde resources exist in the source tree", () => {
  const resources = fileURLToPath(new URL("resources/", here));
  for (const name of ["morde.html", "morde.css", "morde.js", "morde-icon.png", "MORDE_NOTICE.txt", "THIRD_PARTY_NOTICES.morde-sync.md", "LICENSE.syncthing.txt", "THIRD_PARTY_NOTICES.syncthing.md"]) {
    assert.ok(existsSync(join(resources, name)), name);
  }
  const syncNotices = readFileSync(join(resources, "THIRD_PARTY_NOTICES.morde-sync.md"), "utf8");
  assert.match(syncNotices, /fsnotify v1\.9\.0/);
  assert.match(syncNotices, /golang\.org\/x\/sys/);
  assert.match(syncNotices, /source code is licensed under GNU GPL version 3 or later/);
});
