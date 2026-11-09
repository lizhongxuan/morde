/**
 * The model picker of the Connections page (the Models contract, the same on the desktop
 * and the phones): a button that reads `<provider> · <model>` and opens a panel under it
 * with the fixed rows first (*Automatic*), a search field once the lists are long, then
 * one group per provider, each folded to a few rows with *Show {n} more* at its foot
 * (`model-list.ts` decides what is shown), and a trailing row when the caller has one
 * (*Other model…*). A native `<select>` can neither fold nor filter, and OpenRouter or
 * SiliconFlow list hundreds of models. Esc, a click outside and a pick close it; the rows
 * are buttons, so Tab reaches them, and the arrows walk them.
 */
import { Check, ChevronDown, Search } from "lucide-react";
import { useCallback, useEffect, useRef, useState, type KeyboardEvent as ReactKeyboardEvent, type ReactNode } from "react";
import { useT } from "../i18n";
import { hasSearch, layoutGroups, type Current, type ModelGroup, type ModelRow } from "../model-list";
import { cx } from "../util";
import { inputCls } from "./Form";

/** A row before the groups (*Automatic*) or after them (*Other model…*). */
export interface PickerHead {
  value: string;
  label: string;
}

const ROW = "flex w-full items-center gap-2 rounded-xl px-3 py-2 text-left text-[14px] hover:bg-surface-2 focus:bg-surface-2 focus:outline-none";

export function ModelPicker<R extends ModelRow>({
  label,
  text,
  heads = [],
  headValue,
  tail,
  groups,
  current,
  disabled,
  onHead,
  onPick,
  onTail,
  rowText,
}: {
  /** The field's title, for the button's accessible name. */
  label: string;
  /** What the button reads: `<provider> · <model>`, or the head row's label. */
  text: string;
  heads?: PickerHead[];
  /** The selected head row's value, when the picker stands on one. */
  headValue?: string;
  /** A last row under the groups, for a model typed by its id. */
  tail?: string;
  groups: readonly ModelGroup<R>[];
  /** The chosen model, when the picker stands on one. */
  current?: Current;
  disabled?: boolean;
  onHead?: (value: string) => void;
  onPick: (group: string, row: R) => void;
  onTail?: () => void;
  /** How a row is written; the row's name by default. */
  rowText?: (row: R) => ReactNode;
}) {
  const t = useT();
  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState("");
  const [expanded, setExpanded] = useState<Set<string>>(() => new Set());
  // focus goes back to the button after Esc or a pick, not after a click elsewhere
  const [refocus, setRefocus] = useState(false);
  const button = useRef<HTMLButtonElement>(null);
  const panel = useRef<HTMLDivElement>(null);
  const searchable = hasSearch(groups);

  const close = useCallback((back = false) => {
    setOpen(false);
    setQuery("");
    setExpanded(new Set());
    setRefocus(back);
  }, []);

  useEffect(() => {
    if (open || !refocus) return;
    setRefocus(false);
    button.current?.focus();
  }, [open, refocus]);

  // Esc and a click outside close it
  useEffect(() => {
    if (!open) return;
    const onDown = (e: PointerEvent) => {
      const target = e.target as Node;
      if (panel.current?.contains(target) || button.current?.contains(target)) return;
      close();
    };
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        e.stopPropagation();
        close(true);
      }
    };
    document.addEventListener("pointerdown", onDown, true);
    document.addEventListener("keydown", onKey, true);
    return () => {
      document.removeEventListener("pointerdown", onDown, true);
      document.removeEventListener("keydown", onKey, true);
    };
  }, [open, close]);

  // focus lands on the search field when there is one, else on the selected row, else the first
  useEffect(() => {
    if (!open || !panel.current) return;
    const first = panel.current.querySelector<HTMLElement>("input") ?? panel.current.querySelector<HTMLElement>('[aria-selected="true"]') ?? panel.current.querySelector<HTMLElement>("[data-row]");
    first?.focus();
  }, [open]);

  // the arrows walk the rows of the panel the key was pressed in (found from the event, not a ref, so the lint knows it runs on the event only)
  const walk = (e: ReactKeyboardEvent<HTMLElement>) => {
    if (e.key !== "ArrowDown" && e.key !== "ArrowUp") return;
    const rows = [...(e.currentTarget.closest('[role="listbox"]')?.querySelectorAll<HTMLElement>("[data-row]") ?? [])];
    if (!rows.length) return;
    e.preventDefault();
    const at = rows.indexOf(e.target as HTMLElement);
    const next = e.key === "ArrowDown" ? (at < 0 ? 0 : Math.min(rows.length - 1, at + 1)) : at < 0 ? rows.length - 1 : Math.max(0, at - 1);
    rows[next]?.focus();
  };

  const shown = layoutGroups(groups, { current, query, expanded });
  const searching = query.trim().length > 0;
  // plain elements, not a nested component: a re-render must not remount the rows under the keyboard
  const row = (key: string, selected: boolean, children: ReactNode, onClick: () => void, title?: string) => (
    <button key={key} type="button" role="option" aria-selected={selected} data-row="" title={title} onClick={onClick} onKeyDown={walk} className={cx(ROW, selected && "text-accent font-medium")}>
      <span className="min-w-0 flex-1 truncate">{children}</span>
      {selected && <Check size={15} className="shrink-0" />}
    </button>
  );

  return (
    <div className="relative">
      <button
        ref={button}
        type="button"
        disabled={disabled}
        aria-label={label}
        aria-haspopup="listbox"
        aria-expanded={open}
        title={text}
        onClick={() => (open ? close() : setOpen(true))}
        className={cx(inputCls, "flex items-center gap-2 text-left text-fg disabled:opacity-50")}
      >
        <span className="min-w-0 flex-1 truncate">{text}</span>
        <ChevronDown size={16} className="shrink-0 text-muted" />
      </button>
      {open && (
        <div ref={panel} role="listbox" aria-label={label} className="absolute left-0 right-0 top-full z-30 mt-1 flex max-h-[60vh] flex-col overflow-hidden rounded-2xl border border-border bg-surface p-1.5 shadow-xl">
          {heads.length > 0 && (
            <div className="mb-1 flex flex-col border-b border-border pb-1">
              {heads.map((head) =>
                row(`head:${head.value}`, headValue === head.value, head.label, () => {
                  onHead?.(head.value);
                  close(true);
                }),
              )}
            </div>
          )}
          {searchable && (
            <label className="mb-1.5 flex items-center gap-2 rounded-xl bg-surface-2 px-3 py-2 text-muted focus-within:ring-2 focus-within:ring-accent/40">
              <Search size={15} className="shrink-0" />
              <input
                type="search"
                value={query}
                onChange={(e) => setQuery(e.target.value)}
                onKeyDown={walk}
                placeholder={t("Search models")}
                aria-label={t("Search models")}
                autoComplete="off"
                spellCheck={false}
                className="min-w-0 flex-1 bg-transparent text-[14px] text-fg outline-none placeholder:text-muted"
              />
            </label>
          )}
          <div className="min-h-0 flex-1 overflow-y-auto">
            {searching && shown.length === 0 && <div className="px-3 py-2.5 text-[13.5px] text-muted">{t("No model matches")}</div>}
            {shown.map((group) => (
              <div key={group.key} role="group" aria-label={group.label} className="[&+&]:mt-1.5">
                <div className="truncate px-3 pb-0.5 pt-1.5 text-[11px] font-semibold uppercase tracking-wide text-muted">{group.label}</div>
                {group.rows.map((r) =>
                  row(
                    `${group.key}:${r.id}`,
                    current?.group === group.key && current.id === r.id,
                    rowText ? rowText(r) : r.name,
                    () => {
                      onPick(group.key, r);
                      close(true);
                    },
                    r.id,
                  ),
                )}
                {group.more > 0 && (
                  <button type="button" data-row="" onKeyDown={walk} onClick={() => setExpanded((prev) => new Set([...prev, group.key]))} className={cx(ROW, "text-[13px] text-accent")}>
                    {t("Show {n} more", { n: group.more })}
                  </button>
                )}
              </div>
            ))}
          </div>
          {tail && !searching && (
            <div className="mt-1 border-t border-border pt-1">
              {row("tail", false, tail, () => {
                onTail?.();
                close(true);
              })}
            </div>
          )}
        </div>
      )}
    </div>
  );
}
