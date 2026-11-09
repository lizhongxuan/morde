import { RefreshCw } from "lucide-react";
import { useT } from "../i18n";

/**
 * What a list shows when its first fetch failed: the runtime's sentence (translated when it is
 * one we know) and *Try again*, instead of a spinner that never stops or an empty state that lies.
 */
export function LoadError({ message, onRetry }: { message: string; onRetry: () => void }) {
  const t = useT();
  return (
    <div className="flex flex-col items-center gap-3 px-8 py-10 text-center">
      <p className="text-[14px] text-rose-600 dark:text-rose-300">{t(message) || t("Could not load.")}</p>
      <button
        type="button"
        onClick={onRetry}
        className="flex h-10 items-center gap-1.5 rounded-full bg-surface px-4 text-[13.5px] font-medium shadow-[0_1px_4px_rgba(0,0,0,0.14)] dark:border dark:border-border dark:shadow-none"
      >
        <RefreshCw size={14} /> {t("Try again")}
      </button>
    </div>
  );
}
