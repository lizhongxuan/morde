// The link from the dsh profile's node_modules to the plugin shipped in the app's resources:
// `<home>/profiles/nanomuse/node_modules/dsh-nanomuse` → `<resources>/dsh/node_modules/dsh-nanomuse`.
// A symlink on macOS and Linux, a junction on Windows (no privilege needed, and it points at
// a directory). Rewritten whenever its target is not the one this build ships.
import { lstatSync, readlinkSync, rmdirSync, rmSync, symlinkSync, unlinkSync } from "node:fs";

/** Where `link` points today: the target path, "(not a link)", or undefined when nothing is there. */
export function linkTarget(link: string): string | undefined {
  try {
    return lstatSync(link).isSymbolicLink() ? readlinkSync(link) : "(not a link)";
  } catch {
    return undefined;
  }
}

/**
 * Two paths to the same place, as the file system reports them. `readlink` of a junction comes
 * back as Windows stored it — with a trailing backslash, sometimes behind the `\\?\` prefix —
 * and a comparison that minded those re-created the link on every launch.
 */
export function sameTarget(a: string | undefined, b: string): boolean {
  if (a === undefined) return false;
  const norm = (p: string): string => {
    let s = p.startsWith("\\\\?\\") ? p.slice(4) : p;
    s = s.replace(/[\\/]+$/, "");
    return process.platform === "win32" ? s.toLowerCase() : s;
  };
  return norm(a) === norm(b);
}

/**
 * The errors Windows answers with while another process still holds the entry: an update
 * that is replacing the files under resources, the installer's own exit, an antivirus
 * reading the new files. They are gone a moment later, so they are waited out; everything
 * else (a read-only folder, a path that is a file) is the real thing and is reported.
 */
const BUSY = new Set(["EBUSY", "ENOTEMPTY", "EPERM", "EACCES"]);

function isBusy(exc: unknown): boolean {
  const code = (exc as NodeJS.ErrnoException | undefined)?.code;
  return typeof code === "string" && BUSY.has(code);
}

/**
 * Wait in place. The boot is a synchronous chain, so this wait is too, and `Atomics.wait` is
 * the one sleep that works on the main thread of Node. Under a second here is worth more
 * than a start that fails and needs a second launch.
 */
function pause(ms: number): void {
  Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, ms);
}

/** `step` now, and again after 100, 250 and 500 ms while the file system says it is busy. */
function retrying<T>(step: () => T): T {
  const waits = [0, 100, 250, 500];
  for (let i = 0; ; i += 1) {
    const wait = waits[i] ?? 0;
    if (wait > 0) pause(wait);
    try {
      return step();
    } catch (exc) {
      if (i === waits.length - 1 || !isBusy(exc)) throw exc;
    }
  }
}

/**
 * Remove whatever is at `link` so a new link can be made there. A symlink or junction goes
 * as a link: `unlinkSync` (`rmdirSync` for a junction, which is a directory to Windows) —
 * never through `rmSync`, which follows the link first and, when the target is gone (the
 * install folder of an earlier version, say), sees nothing to remove and leaves the stale
 * link standing; `symlinkSync` then fails with "EEXIST … symlink" and the app does not
 * start (0.1.39 on Windows after an install path had changed). A real directory — someone
 * copied the plugin in — is removed as one. Every removal is waited out while the file
 * system says it is busy: the first start after an in-app update failed with ENOTEMPTY on
 * the junction the installer was still replacing (1.0.6 on Windows, #618).
 */
export function removeLink(link: string): void {
  let isLink: boolean;
  try {
    isLink = lstatSync(link).isSymbolicLink();
  } catch {
    return;
  }
  if (isLink) {
    try {
      // not waited out: on Windows this is where a junction says it is one, and it says it
      // at once (EPERM). The waits below are the ones that apply to a real removal there
      unlinkSync(link);
      return;
    } catch {
      // a junction: Windows removes those as directories
    }
    try {
      retrying(() => rmdirSync(link));
      return;
    } catch {
      // fall through to the general case
    }
  }
  retrying(() => rmSync(link, { recursive: true, force: true }));
}

/**
 * Make `link` point at `target`; true when the link was (re)made, false when it already did.
 * A link that cannot be made is reported by the thrown error, with what was there before.
 */
export function relink(link: string, target: string): boolean {
  if (sameTarget(linkTarget(link), target)) return false;
  removeLink(link);
  const make = (): void => {
    symlinkSync(target, link, process.platform === "win32" ? "junction" : "dir");
  };
  try {
    retrying(make);
  } catch (exc) {
    // one more try: whatever lstat missed the first time is gone now, or it is not ours to fix
    removeLink(link);
    try {
      retrying(make);
    } catch {
      const left = linkTarget(link);
      throw new Error(
        `${String((exc as Error).message ?? exc)} (in the way: ${left ?? "nothing"}; remove ${link} by hand and start Morde again)`,
      );
    }
  }
  return true;
}
