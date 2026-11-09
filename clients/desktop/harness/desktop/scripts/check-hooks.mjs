#!/usr/bin/env node
// The stylesheet of the nanoMuse bundle (harness/dsh-nanomuse/src/client/styles.ts) dresses
// the harness's own DOM through its `data-*` hooks (`[data-composer-card]`,
// `[data-chat-flow-kind="user"]`), its test ids (`[data-testid="task-manager-page"]`) and the
// stems of its CSS-module class names (`[class*="_toBottom"]`). None of those is a contract:
// a dsh release may rename any of them, and the rule that leaned on it then matches nothing,
// quietly, so a pill loses its colour or the header its scrim. This reads every such hook out
// of the stylesheet and looks for each in the dsh that was staged (scripts/prepare-dsh.mjs
// installs it by exact version, with no lock file), so a version that moved is caught when it
// is staged and not when someone notices the chat looks wrong.
//
//   node scripts/check-hooks.mjs                              # ./dsh/node_modules, the bundle's styles.ts
//   node scripts/check-hooks.mjs --dsh /tmp/dsh/node_modules  # another staging (harness.yml's)
//   node scripts/check-hooks.mjs --styles path --ours dir     # other sources (the tests)
//
// A hook is fine when the staged dsh's packages mention it, or when the bundle sets it itself
// (`data-nm-*`, the `data-window-drag` our rooms put on their own top strip). Anything else
// is listed and the exit code is 1. Attribute values (`[data-slot="conversation.header"]`,
// `[data-state="error"]`) are not checked: too many are built at run time.

import { existsSync, readdirSync, readFileSync, statSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const appDir = resolve(here, "..");

/** The hooks a stylesheet leans on: `data-*` attribute names, `data-testid` values, class-name stems. */
export function hooksOf(css) {
  const attributes = new Set();
  const testIds = new Set();
  const stems = new Set();
  for (const m of css.matchAll(/\[(data-[a-z0-9-]+)(?:[\]=~|^$*])/g)) attributes.add(m[1]);
  for (const m of css.matchAll(/\[data-testid="([^"]+)"\]/g)) testIds.add(m[1]);
  for (const m of css.matchAll(/\[class\*="(_[A-Za-z0-9]+)"\]/g)) stems.add(m[1]);
  attributes.delete("data-testid");
  return { attributes: [...attributes].sort(), testIds: [...testIds].sort(), stems: [...stems].sort() };
}

const TEXT = /\.(m?js|cjs|css|html|json|tsx?)$/;
const SKIP_DIRS = new Set(["prebuilds", "build", ".bin"]);

/** Every text file under `dir`, read lazily. */
function* textFiles(dir) {
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const p = join(dir, entry.name);
    if (entry.isSymbolicLink()) continue;
    if (entry.isDirectory()) {
      if (SKIP_DIRS.has(entry.name)) continue;
      yield* textFiles(p);
    } else if (TEXT.test(entry.name) && !entry.name.endsWith(".map")) yield p;
  }
}

/**
 * Which of `needles` the files under `roots` mention, as a set. Each file is read once and
 * every needle still missing is tried on it; the walk stops once all are found. A needle
 * with an entry in `spellings` counts as mentioned when any of those texts is, and not by
 * its own literal.
 */
export function findIn(roots, needles, spellings = new Map()) {
  const found = new Set();
  const missing = new Set(needles);
  for (const root of roots) {
    if (!existsSync(root) || !statSync(root).isDirectory()) continue;
    for (const file of textFiles(root)) {
      if (missing.size === 0) return found;
      let text;
      try {
        text = readFileSync(file, "utf8");
      } catch {
        continue;
      }
      for (const needle of missing) {
        if ((spellings.get(needle) ?? [needle]).some((s) => text.includes(s))) {
          found.add(needle);
          missing.delete(needle);
        }
      }
    }
  }
  return found;
}

/**
 * The ways a source of ours *sets* a `data-*` attribute, as opposed to styling it: the name
 * quoted (`setAttribute('data-nm-muse'`, `'data-nm-muse': ''` in JSX) or the camel-cased
 * `dataset` form (`dataset.nmMuse`, `dataset['nmMuse']`). A selector (`[data-nm-muse]`) is
 * neither, so the stylesheet, which names every hook it leans on, vouches for none of them.
 */
export function setterSpellings(attribute) {
  const camel = attribute.slice("data-".length).replace(/-([a-z0-9])/g, (_, c) => c.toUpperCase());
  return [`'${attribute}'`, `"${attribute}"`, `\`${attribute}\``, `dataset.${camel}`, `dataset['${camel}']`, `dataset["${camel}"]`];
}

/**
 * The check: every hook of `styles` is in the dsh packages under `dshNodeModules`
 * (`@deepseek-ai/*`), or set by the bundle's own client under `oursDirs`.
 * @returns {{ checked: number, missing: string[], own: string[] }}
 */
export function checkHooks({ styles, dshNodeModules, oursDirs }) {
  const { attributes, testIds, stems } = hooksOf(readFileSync(styles, "utf8"));
  const scoped = join(dshNodeModules, "@deepseek-ai");
  const dshRoots = existsSync(scoped) ? readdirSync(scoped).map((name) => join(scoped, name)) : [];
  const needles = [...attributes, ...testIds, ...stems];
  const inDsh = findIn(dshRoots, needles);
  // what the bundle sets itself: its client sources, and the shell's (`data-nm-platform` comes
  // from the preload); a selector in the stylesheet is not a setter, so it does not count
  const unseen = attributes.filter((a) => !inDsh.has(a));
  const ours = findIn(oursDirs, unseen, new Map(unseen.map((a) => [a, setterSpellings(a)])));
  const missing = needles.filter((n) => !inDsh.has(n) && !ours.has(n));
  return { checked: needles.length, missing, own: [...ours].sort() };
}

const isMain = process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url);
if (isMain) {
  const args = process.argv.slice(2);
  const opt = (name) => {
    const i = args.indexOf(name);
    return i >= 0 ? args[i + 1] : undefined;
  };
  const dshNodeModules = resolve(opt("--dsh") ?? join(appDir, "dsh", "node_modules"));
  const styles = resolve(opt("--styles") ?? join(appDir, "..", "dsh-nanomuse", "src", "client", "styles.ts"));
  const ours = opt("--ours");
  const oursDirs = ours ? [resolve(ours)] : [join(appDir, "..", "dsh-nanomuse", "src"), join(appDir, "src")];
  if (!existsSync(join(dshNodeModules, "@deepseek-ai"))) {
    console.error(`no dsh under ${dshNodeModules}: stage it first (node scripts/prepare-dsh.mjs), or point --dsh at a node_modules that has @deepseek-ai/*`);
    process.exit(2);
  }
  let version = "?";
  try {
    version = JSON.parse(readFileSync(join(dshNodeModules, "@deepseek-ai", "dsh", "package.json"), "utf8")).version;
  } catch {
    // a partial staging, or the tests' fake: the version is not the point
  }
  const { checked, missing, own } = checkHooks({ styles, dshNodeModules, oursDirs });
  if (missing.length > 0) {
    console.error(`${missing.length} of the ${checked} hooks styles.ts leans on are not in dsh ${version}:`);
    for (const m of missing) console.error(`  ${m}`);
    console.error("the harness renamed them, or the staging is incomplete; the rules that use them match nothing now. Update styles.ts for this dsh (see its header), or pin the version prepare-dsh.mjs stages.");
    process.exit(1);
  }
  console.log(`${checked} hooks of styles.ts found in dsh ${version}${own.length ? ` (${own.length} set by the bundle itself: ${own.join(", ")})` : ""}`);
}
