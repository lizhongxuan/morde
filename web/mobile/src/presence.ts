/**
 * Who is working on a synced conversation right now (contract C9): the relay fans out a
 * `working` frame when another device starts or ends a turn, the runtime keeps the live
 * ones, and this module decides how long a "{device} is working…" line stays worth showing.
 * Pure, so the store and the chat screen share one rule and tests can pin it.
 */
import type { WorkingPresence } from "./types";

/** A working line is shown for at most ten minutes after the device last said so. */
export const WORKING_TTL_MS = 10 * 60 * 1000;

/** The entries still live at `now`, by thread; later entries for a thread win. */
export function liveWorking(list: readonly WorkingPresence[] | undefined, now = Date.now()): Record<string, WorkingPresence | undefined> {
  const out: Record<string, WorkingPresence | undefined> = {};
  for (const w of list ?? []) if (now - w.at * 1000 < WORKING_TTL_MS) out[w.thread] = w;
  return out;
}
