import { useEffect, useState } from "react";
import { createPortal } from "react-dom";
import { RefreshCw } from "lucide-react";
import { useT } from "../i18n";

const regionClass = "pointer-events-none fixed inset-x-4 top-[calc(env(safe-area-inset-top,0px)+4.5rem)] z-[100] mx-auto grid max-w-xl gap-2";

function useLoadErrorRegion() {
  const [region, setRegion] = useState<HTMLDivElement | null>(null);
  useEffect(() => {
    if (typeof document === "undefined") return;
    let target = document.getElementById("morde-mobile-load-errors") as HTMLDivElement | null;
    if (!target) {
      target = document.createElement("div");
      target.id = "morde-mobile-load-errors";
      target.className = regionClass;
      document.body.append(target);
    }
    setRegion(target);
  }, []);
  return region;
}

/** A failed first read stays actionable without taking space from the screen's content. */
export function LoadError({ message, onRetry, label }: { message: string; onRetry: () => void; label?: string }) {
  const t = useT();
  const region = useLoadErrorRegion();
  const content = (
    <div role="alert" aria-label={label} className="pointer-events-auto flex items-center justify-between gap-3 rounded-2xl border border-rose-300 bg-rose-50/95 px-4 py-3 text-left text-rose-800 shadow-lg backdrop-blur dark:border-rose-900 dark:bg-rose-950/95 dark:text-rose-200">
      <p className="min-w-0 flex-1 text-[13.5px]">{t(message) || t("Could not load.")}</p>
      <button
        type="button"
        onClick={onRetry}
        className="flex h-10 shrink-0 items-center gap-1.5 rounded-full bg-surface px-4 text-[13.5px] font-medium shadow-[0_1px_4px_rgba(0,0,0,0.14)] dark:border dark:border-border dark:shadow-none"
      >
        <RefreshCw size={14} /> {t("Try again")}
      </button>
    </div>
  );
  return region ? createPortal(content, region) : <div className={regionClass}>{content}</div>;
}
