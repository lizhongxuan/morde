/**
 * The developer side of the app — the Coding screen (the coding agents on this computer),
 * the runtime's address and token, the harness — is there for whoever wants it and out of
 * the way for everyone else: one switch in Settings, kept on this device like the theme.
 * `#coding` in the address still opens the screen whatever the switch says.
 */
import { useSyncExternalStore } from "react";

const STORAGE_KEY = "nanomuse_devtools";

function load(): boolean {
  try {
    return localStorage.getItem(STORAGE_KEY) === "1";
  } catch {
    return false;
  }
}

let on = load();
const listeners = new Set<() => void>();

export function developerTools(): boolean {
  return on;
}

export function setDeveloperTools(next: boolean): void {
  on = next;
  try {
    localStorage.setItem(STORAGE_KEY, next ? "1" : "0");
  } catch {
    // fine — the choice lasts for this page then
  }
  listeners.forEach((fn) => fn());
}

function subscribe(fn: () => void): () => void {
  listeners.add(fn);
  return () => listeners.delete(fn);
}

export function useDeveloperTools(): boolean {
  return useSyncExternalStore(subscribe, developerTools, developerTools);
}
