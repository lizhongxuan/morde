/**
 * The few pieces of shared state between our seats: how to open Settings, and
 * whether the agent's profile drawer is open. The settings shell (ours, when it
 * occupies `sidebar.settings`) sets `open` while it is mounted; the rail's
 * menu, the header's face and the pages call it. The drawer is a small
 * observable the overlay entry reads and the rail's face and the header flip.
 */
import { useSyncExternalStore } from 'react'

export const settingsBus: { open?: (() => boolean) | undefined; openSection?: ((id: string) => boolean) | undefined; openOnboarding?: ((id: string) => void) | undefined } = {}

let drawerOpen = false
const drawerListeners = new Set<() => void>()

export const profileBus = {
  isOpen: () => drawerOpen,
  open: () => { if (!drawerOpen) { drawerOpen = true; for (const l of drawerListeners) l() } },
  close: () => { if (drawerOpen) { drawerOpen = false; for (const l of drawerListeners) l() } },
  toggle: () => { drawerOpen = !drawerOpen; for (const l of drawerListeners) l() },
  subscribe: (listener: () => void) => { drawerListeners.add(listener); return () => { drawerListeners.delete(listener) } },
}

export function useProfileOpen(): boolean {
  return useSyncExternalStore(profileBus.subscribe, profileBus.isOpen, profileBus.isOpen)
}
