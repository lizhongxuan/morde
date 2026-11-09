/**
 * The glow on Linux (X11): the decisions behind keeping it click-through, as plain
 * functions so they can be tested without Electron (tests/glow.test.mjs). main.ts wires
 * them to the window.
 *
 * What goes wrong without this. The glow is a transparent, always-on-top X window the
 * size of the display, and Electron's `setIgnoreMouseEvents(true)` makes it click-through
 * by giving it an XShape *input* region of one pixel at (0,0) (`NativeWindowViews::
 * SetIgnoreMouseEvents`). Chromium, however, resets that region to the whole window every
 * time the window's bounds change: `DesktopWindowTreeHostLinux::OnBoundsChanged` →
 * `UpdateFrameHints` → `X11Window::SetInputRegion(nullopt)` → `ShapeMask(Input, None)`.
 * Bounds change when the window is created (Chromium clamps a new window to the work
 * area — under GNOME Shell with its dock and top bar, every time), when it is first
 * mapped, on every `setBounds`, and whenever the X server's ConfigureNotify for one of
 * those arrives — tens of milliseconds after the call that caused it. With the region
 * gone, the glow takes every click on the screen: the hands' own (the pointer is moved
 * to the target, pressed, and the press lands on the glow drawn over the target) and the
 * person's (nothing on the screen can be clicked while the glow is up). Measured in a
 * nested X server (Xephyr) with GNOME Shell 3.36 at 3840×2160, scale 2, by reading the
 * region back with ShapeGetRectangles while the app ran.
 *
 * What is done about it, in three layers:
 * 1. The glow steps aside for the pointer: it is hidden (unmapped) while a pointer action
 *    runs and shown again right after. An unmapped window receives no input whatever its
 *    shape, so a synthetic click cannot hit the glow. The marker is drawn when it comes back.
 * 2. The region is set again after every event that can cost it — at once (after the
 *    native call that cleared it), when the X server has answered, and once more when
 *    things have settled (`REARM_DELAYS_MS`).
 * 3. A watchdog with the X server as the witness: a click-through glow never receives a
 *    pointer event. When its page does see one, the region is gone — it is set again on
 *    the spot and the log says so once per episode. This is what protects the person's own
 *    mouse when a bounds change nobody expected (a display changing size) clears the region.
 *    One care: Chromium makes up a mouse-enter and a mouse-move of its own each time the
 *    window is shown or re-bounded (aura's synthesized mouse move, at the last position
 *    *it* knew — a stale point, not where the pointer is), so an enter or a move inside
 *    LEAK_GRACE_MS of a show is not evidence; a press or a wheel is never made up and
 *    always counts. Seen in the rig: the glow's page got `pointerenter 1618,145` 30–70 ms
 *    after every show while the pointer stood at 1770,725 and the region was intact.
 */

/** The marker kinds (operator.ts `mark(...)`) that move the pointer: the glow steps aside for these. */
const POINTER_KINDS = new Set(["move", "click", "double click", "right click", "middle click", "drag", "scroll down", "scroll up"]);

/** Whether an action of this kind moves or presses the pointer (typing and keys do not). */
export function pointerAction(kind: string): boolean {
  return POINTER_KINDS.has(kind);
}

/**
 * After a show, a resize or a move: when to set the input region again, in ms. 0 means
 * right after the current native call (a `setImmediate`); 150 covers the X server's
 * ConfigureNotify for the change (100 was enough in a quiet rig, 50 was not); 600 is the
 * late one for a loaded machine, where the first two ran before Chromium got to its reset.
 */
export const REARM_DELAYS_MS: readonly number[] = [0, 150, 600];

/** How long the operator waits after a show before it moves the pointer with the glow up (the second re-arm). */
export const GLOW_SETTLE_MS = 150;

/** How long after `hide()` the X server is sure to have unmapped the glow before the pointer moves. */
export const GLOW_HIDE_MS = 40;

/** Leaks closer together than this are one episode (one log line). */
export const LEAK_EPISODE_MS = 3000;

/**
 * After a show, a resize or a move: for how long an enter or a move reported by the page
 * is taken for Chromium's own synthesized one and not counted. The made-up pair arrives
 * 30–70 ms after the show in a quiet rig; a real loss of the region in this window is
 * what REARM_DELAYS_MS repairs anyway, so nothing is given up by waiting.
 */
export const LEAK_GRACE_MS = 1000;

/** A pointer event the glow's page reported: where, and what kind. */
export interface Leak {
  x: number;
  y: number;
  type: string;
}

/** The event kinds Chromium never makes up: a button or a wheel comes from the X server or not at all. */
const PRESSED = new Set(["pointerdown", "mousedown", "wheel", "auxclick", "contextmenu"]);

/**
 * Whether a reported pointer event is well-formed and not the one pixel Electron leaves.
 * The pixel at (0,0) is the region a click-through glow still has, so an event there is
 * expected and ignored; anything else is a pointer event at a window that should take none.
 */
export function leakIsReal(leak: unknown): leak is Leak {
  if (!leak || typeof leak !== "object") return false;
  const { x, y, type } = leak as Partial<Leak>;
  if (typeof x !== "number" || typeof y !== "number" || !Number.isFinite(x) || !Number.isFinite(y)) return false;
  if (typeof type !== "string" || !type) return false;
  return !(x <= 0 && y <= 0);
}

/**
 * Whether a real leak is evidence from the X server, given how long ago (ms) the window
 * was last shown or re-bounded: a press or a wheel always; an enter or a move only once
 * the grace for Chromium's synthesized pair has passed.
 */
export function leakIsEvidence(leak: Leak, sinceShownMs: number): boolean {
  if (PRESSED.has(leak.type)) return true;
  return sinceShownMs >= LEAK_GRACE_MS;
}

/** The watchdog's memory: how many repairs, and whether this leak is a new episode worth a log line. */
export class LeakLog {
  repairs = 0;
  episodes = 0;
  private lastAt = Number.NEGATIVE_INFINITY;

  /** Record a real leak at `now` (ms); true when the log should say so (the first of an episode). */
  record(now: number): boolean {
    this.repairs += 1;
    const fresh = now - this.lastAt > LEAK_EPISODE_MS;
    this.lastAt = now;
    if (fresh) this.episodes += 1;
    return fresh;
  }
}

/**
 * What the second instance of the app sent with its lock request, against the running
 * copy's version: true when the launcher started a *newer* (or just different) build
 * while this one — from before an upgrade — is still in the tray. The running copy then
 * relaunches itself, which starts the installed binary, i.e. the new version.
 */
export function staleInstance(runningVersion: string, arrived: unknown): boolean {
  if (!arrived || typeof arrived !== "object") return false;
  const version = (arrived as { version?: unknown }).version;
  return typeof version === "string" && version !== "" && version !== runningVersion;
}
