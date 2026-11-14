import { RefreshCw } from "lucide-react";
import { useT } from "../i18n";

/** A failed first read stays beside its section and never covers the screen content. */
export function LoadError({ message, onRetry, label }: { message: string; onRetry: () => void; label?: string }) {
  const t = useT();
  return (
    <div role="alert" aria-label={label} className="flex items-center justify-between gap-3 rounded-2xl border border-rose-300 bg-rose-50 px-4 py-3 text-left text-rose-800 dark:border-rose-900 dark:bg-rose-950 dark:text-rose-200">
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
}
