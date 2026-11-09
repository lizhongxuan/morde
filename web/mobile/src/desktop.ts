/**
 * The desktop app (desktop/) shows this web app in an Electron window and exposes a little of
 * itself as `window.nanomuseDesktop` (desktop/app/src/preload/index.ts). Knowing it is in that
 * shell, the app lays itself out like a desktop program at any window width — the sidebar
 * always, the content across the window — and on a Mac leaves the traffic lights room.
 */
export interface DesktopBridge {
  /** Node's process.platform: "darwin", "win32", "linux" */
  platform: string;
  version: string;
  /** whether the app starts with the computer (Electron's login item); older shells have neither */
  loginItem?: () => Promise<boolean>;
  setLoginItem?: (on: boolean) => void;
  /** the quick-chat shortcut was pressed: the window is up, the composer should take focus */
  onQuickChat?: (cb: () => void) => void | (() => void);
}

declare global {
  interface Window {
    nanomuseDesktop?: DesktopBridge;
  }
}

export function desktopBridge(): DesktopBridge | null {
  return window.nanomuseDesktop ?? null;
}

export function isDesktopApp(): boolean {
  return !!window.nanomuseDesktop;
}

/** The desktop title bar is hidden on macOS ("hiddenInset"): the window's controls sit over our page. */
export function hasInsetTitleBar(): boolean {
  return window.nanomuseDesktop?.platform === "darwin";
}

/** Marks <html> so the stylesheet's `wide:` variant and the title-bar spacing apply (index.css). */
export function markDesktopShell(): void {
  const bridge = desktopBridge();
  if (!bridge) return;
  document.documentElement.classList.add("desktop");
  document.documentElement.dataset.platform = bridge.platform;
}
