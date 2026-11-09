import { ArrowLeft } from "lucide-react";
import type { ReactNode } from "react";
import { useT } from "../i18n";
import { useStore } from "../store";
import { MuseRoundButton } from "./MuseHeader";

/**
 * Muse's page bar, the way the Android app draws it on every screen reached from the drawer or
 * the "•••" menu: the back glyph on a white disc at the start, the title centred on the whole bar,
 * actions at the end, no rule under it. The optional description is the small grey caption Muse
 * sets under a bar.
 */
export function PageBar({
  title,
  description,
  actions,
  onBack,
  backLabel,
}: {
  title: ReactNode;
  description?: ReactNode;
  actions?: ReactNode;
  /** where the back disc goes; the chat by default */
  onBack?: () => void;
  backLabel?: string;
}) {
  const { setTab } = useStore();
  const t = useT();
  return (
    <header className="safe-top shrink-0">
      <div className="relative flex h-14 items-center justify-center px-3">
        <div className="absolute left-3 top-1/2 -translate-y-1/2">
          <MuseRoundButton small onClick={onBack ?? (() => setTab("chat"))} label={backLabel ?? t("Back")}>
            <ArrowLeft size={20} />
          </MuseRoundButton>
        </div>
        <h1 className="max-w-[calc(100%-128px)] truncate text-[17px] font-semibold leading-[22px]">{title}</h1>
        {actions && <div className="absolute right-3 top-1/2 flex -translate-y-1/2 items-center gap-2">{actions}</div>}
      </div>
      {description && <p className="px-5 pb-2 text-[13px] leading-[18px] text-muted">{description}</p>}
    </header>
  );
}

/** "Back to chat" on its own, for a screen that draws its own title. */
export function BackBar({ label = "Chat" }: { label?: string }) {
  const { setTab } = useStore();
  const t = useT();
  return (
    <div className="mb-1 flex h-12 items-center">
      <MuseRoundButton small onClick={() => setTab("chat")} label={t(label)}>
        <ArrowLeft size={20} />
      </MuseRoundButton>
    </div>
  );
}
