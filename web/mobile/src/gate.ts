import type { HubView, SettingsView } from "./types";

/**
 * Whether the sign-in door stands in front of the app. It does when this runtime asks for an
 * account (`cloud.required`, the default), none is signed in, no model of one's own is ready
 * (`llm_ready`), and the person has not stepped past it for a key of their own. With a model of
 * one's own the console works signed out: chat, hands, pictures, clips. The sign-in waits under
 * Connections for the Cloud models, sync and the hub.
 */
export function signInDoorNeeded(hub: Pick<HubView, "account"> | null, settings: Pick<SettingsView, "llm_ready"> | null, skipped: boolean): boolean {
  if (!hub || !hub.account.required || hub.account.signed_in) return false;
  if (settings?.llm_ready) return false;
  return !skipped;
}
