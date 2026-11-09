import { MonitorSmartphone, X } from "lucide-react";
import { useEffect, useState } from "react";
import { isDesktopApp } from "../desktop";
import { useT } from "../i18n";
import { useStore } from "../store";
import { readStorage, writeStorage } from "../util";

const SEEN_KEY = "nm.desktop.remote_hint";

/**
 * The desktop app's one first-run line: once this computer is on the hub with remote control
 * on (the default), the phone can drive it — say so once, with the way to Devices, and let it
 * be dismissed for good. Only in the Electron shell; a browser tab has no hands to speak of.
 */
export function DesktopRemoteHint() {
  const { state, setTab } = useStore();
  const t = useT();
  const [seen, setSeen] = useState(() => !isDesktopApp() || readStorage(SEEN_KEY) === "1");
  const hub = state.hub;
  const due = !seen && !!hub && hub.state === "connected" && hub.remote_control && hub.account.signed_in;

  // the hint is owed only once the hub is actually up; remember that it was shown, not that it was due
  useEffect(() => {
    if (due) writeStorage(SEEN_KEY, "1");
  }, [due]);

  if (!due) return null;
  const dismiss = () => setSeen(true);
  return (
    <div className="flex items-center gap-2.5 border-b border-border bg-accent/10 px-4 py-2 text-[12.5px] text-fg">
      <MonitorSmartphone size={16} className="shrink-0 text-accent" />
      <span className="min-w-0 flex-1">
        {t("Your phone can now operate this computer: run commands, fetch files, hand over whole tasks. Each risky step still asks here first.")}
      </span>
      <button
        type="button"
        onClick={() => {
          dismiss();
          setTab("devices");
        }}
        className="shrink-0 rounded-full border border-border bg-surface px-2.5 py-1 text-[12px] font-medium hover:bg-surface-2"
      >
        {t("Devices")}
      </button>
      <button type="button" onClick={dismiss} aria-label={t("Dismiss")} className="shrink-0 rounded-full p-1 text-muted hover:text-fg">
        <X size={15} />
      </button>
    </div>
  );
}
