import {
  chmodSync,
  closeSync,
  copyFileSync,
  existsSync,
  fsyncSync,
  mkdirSync,
  openSync,
  readFileSync,
  renameSync,
  rmSync,
  writeSync,
} from "node:fs";
import { dirname } from "node:path";

function syncDirectory(path: string): void {
  try {
    const fd = openSync(path, "r");
    try {
      fsyncSync(fd);
    } finally {
      closeSync(fd);
    }
  } catch {
    // Windows does not let Node fsync a directory.
  }
}

/** Write private JSON beside its target, flush it, then atomically replace the target. */
export function writePrivateJson(path: string, value: unknown): void {
  const dir = dirname(path);
  const tmp = `${path}.${process.pid}.${Date.now()}.tmp`;
  const backup = `${path}.bak`;
  mkdirSync(dir, { recursive: true });
  let fd: number | undefined;
  try {
    fd = openSync(tmp, "wx", 0o600);
    writeSync(fd, `${JSON.stringify(value, null, 2)}\n`);
    fsyncSync(fd);
    closeSync(fd);
    fd = undefined;
    chmodSync(tmp, 0o600);
    if (existsSync(path)) {
      rmSync(backup, { force: true });
      renameSync(path, backup);
      chmodSync(backup, 0o600);
    }
    try {
      renameSync(tmp, path);
    } catch (error) {
      if (existsSync(backup) && !existsSync(path)) renameSync(backup, path);
      throw error;
    }
    chmodSync(path, 0o600);
    syncDirectory(dir);
  } finally {
    if (fd !== undefined) closeSync(fd);
    rmSync(tmp, { force: true });
  }
}

/**
 * Read JSON, falling back to the last complete replacement when the current
 * file is missing or damaged. A recovered target is private too.
 */
export function readPrivateJson<T>(path: string, accept: (value: unknown) => T | undefined): T | undefined {
  const parse = (candidate: string): T | undefined => {
    try {
      return accept(JSON.parse(readFileSync(candidate, "utf8")) as unknown);
    } catch {
      return undefined;
    }
  };
  const current = parse(path);
  if (current !== undefined) {
    try {
      chmodSync(path, 0o600);
    } catch {
      // Reading an older read-only file is still better than losing its settings.
    }
    return current;
  }
  const backup = `${path}.bak`;
  const recovered = parse(backup);
  if (recovered === undefined) return undefined;
  try {
    copyFileSync(backup, path);
    chmodSync(path, 0o600);
    syncDirectory(dirname(path));
  } catch {
    // The complete backup is still usable for this launch.
  }
  return recovered;
}
