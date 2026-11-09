// The hook check (scripts/check-hooks.mjs): every `data-*` attribute, test id and class-name
// stem the bundle's stylesheet leans on is either in the staged dsh's packages or set by the
// bundle itself; one that is in neither is named and fails the check. A fake stylesheet and a
// fake node_modules stand in for the real ones; the real check runs in desktop-app.yml once
// the harness is staged, and in harness.yml against the dsh the tarball is installed into.
import assert from "node:assert/strict";
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";
import { checkHooks, hooksOf } from "../scripts/check-hooks.mjs";

const STYLES = `
html[data-nm-muse] [data-composer-card] [class*="_primary"] { color: red; }
[data-conversation-content][data-content-phase="hero"] > [data-conversation-scroll] { display: flex; }
html[data-nm-platform='win32'] [data-testid="task-manager-page"] [class*="_pageHeading"] { padding: 0; }
[data-slot="conversation.header"] ~ [data-chat-flow-kind="user"] [class*="_bubble"] a { color: inherit; }
`;

test("hooksOf reads attribute names, test ids and class stems, and not attribute values", () => {
  const hooks = hooksOf(STYLES);
  assert.deepEqual(hooks.attributes, ["data-chat-flow-kind", "data-composer-card", "data-content-phase", "data-conversation-content", "data-conversation-scroll", "data-nm-muse", "data-nm-platform", "data-slot"]);
  assert.deepEqual(hooks.testIds, ["task-manager-page"]);
  assert.deepEqual(hooks.stems, ["_bubble", "_pageHeading", "_primary"]);
  // `data-testid` itself is not a hook to look for, and `hero`, `user`, `conversation.header` are values
  assert.ok(!hooks.attributes.includes("data-testid"));
  assert.ok(!JSON.stringify(hooks).includes("hero"));
});

function fixture(dshText, oursText) {
  const dir = mkdtemp("nm-check-hooks-");
  const styles = join(dir, "styles.ts");
  writeFileSync(styles, STYLES);
  const pkg = join(dir, "node_modules", "@deepseek-ai", "dsh-client-ui-conversation", "lib");
  mkdirSync(pkg, { recursive: true });
  writeFileSync(join(pkg, "index.js"), dshText);
  writeFileSync(join(pkg, "index.js.map"), "a map mentions everything: _pageHeading task-manager-page data-composer-card");
  const ours = join(dir, "client");
  mkdirSync(ours);
  writeFileSync(join(ours, "App.tsx"), oursText);
  return { dir, styles, dshNodeModules: join(dir, "node_modules"), oursDirs: [ours] };
}

function mkdtemp(prefix) {
  return mkdtempSync(join(tmpdir(), prefix));
}

const DSH_ALL = `
  e.jsx("div",{"data-composer-card":!0,className:s._primary});
  const t={pageHeading:"abc12_pageHeading",bubble:"xyz_bubble"};
  n("data-testid","task-manager-page"); x["data-conversation-content"]; x["data-content-phase"]="hero";
  y("data-conversation-scroll"); z("data-chat-flow-kind"); w("data-slot");
`;
const OURS = `document.documentElement.toggleAttribute('data-nm-muse', on); html.dataset.nmPlatform; el.setAttribute('data-nm-platform', p)`;

test("every hook in dsh or set by the bundle passes; the bundle's own are reported as such", () => {
  const f = fixture(DSH_ALL, OURS);
  try {
    const out = checkHooks(f);
    assert.equal(out.checked, 12);
    assert.deepEqual(out.missing, []);
    assert.deepEqual(out.own, ["data-nm-muse", "data-nm-platform"]);
  } finally {
    rmSync(f.dir, { recursive: true, force: true });
  }
});

test("a renamed attribute, test id or class stem is named, and a source map does not vouch for it", () => {
  // dsh renamed the composer card, the task manager's test id and the heading's class
  const renamed = DSH_ALL.replace('"data-composer-card"', '"data-composer-shell"').replace("task-manager-page", "tasks-page").replace("_pageHeading", "_heading");
  const f = fixture(renamed, OURS);
  try {
    const out = checkHooks(f);
    assert.deepEqual(out.missing, ["data-composer-card", "task-manager-page", "_pageHeading"]);
  } finally {
    rmSync(f.dir, { recursive: true, force: true });
  }
});

test("an attribute the bundle stops setting itself is missing too", () => {
  const f = fixture(DSH_ALL, "nothing of ours sets any data attribute");
  try {
    assert.deepEqual(checkHooks(f).missing, ["data-nm-muse", "data-nm-platform"]);
  } finally {
    rmSync(f.dir, { recursive: true, force: true });
  }
});

test("a selector in the stylesheet is not a setter: a hook only styles.ts mentions is missing", () => {
  // the real run points `oursDirs` at dsh-nanomuse/src, where styles.ts lives and names every
  // hook it leans on; only a quoted name or a `dataset` form counts as the bundle setting it
  const f = fixture(DSH_ALL, "document.querySelector('[data-nm-muse]'); el.dataset['nmPlatform'] = p");
  try {
    const out = checkHooks({ ...f, oursDirs: [f.dir] });
    assert.deepEqual(out.missing, ["data-nm-muse"]);
    assert.deepEqual(out.own, ["data-nm-platform"]);
  } finally {
    rmSync(f.dir, { recursive: true, force: true });
  }
});

test("the real stylesheet's hooks are the kind the check reads", async () => {
  const { readFileSync } = await import("node:fs");
  const real = hooksOf(readFileSync(new URL("../../dsh-nanomuse/src/client/styles.ts", import.meta.url), "utf8"));
  // the rules the header of styles.ts names as leaning on the harness's DOM
  for (const hook of ["data-composer-card", "data-chat-flow-kind", "data-conversation-scroll", "data-window-drag"]) assert.ok(real.attributes.includes(hook), hook);
  assert.ok(real.testIds.includes("task-manager-page"));
  for (const stem of ["_pageHeading", "_toBottom", "_bubble"]) assert.ok(real.stems.includes(stem), stem);
});
