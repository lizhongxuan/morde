import { ChevronRight, ExternalLink } from "lucide-react";
import type { ReactNode } from "react";
import { cx } from "../util";

/**
 * Muse's list pieces, as the Android app draws them (ui/muse/MuseChrome.kt): a white card
 * with 16px corners on the grey canvas, rows of a bare glyph in ink, a label, a grey value
 * and a chevron, hairline dividers inset past the glyph, and the small grey captions set
 * under a card.
 */
export function MuseCard({ children, className }: { children: ReactNode; className?: string }) {
  return <div className={cx("mx-4 overflow-hidden rounded-2xl bg-surface dark:border dark:border-border", className)}>{children}</div>;
}

/** The hairline between two rows, inset past the glyph (52px) or to the card's padding (16px). */
export function MuseDivider({ inset = 52 }: { inset?: 16 | 52 }) {
  return <div className={cx("border-t border-border/60", inset === 52 ? "ml-[52px]" : "ml-4")} />;
}

export function MuseRow({
  icon,
  label,
  value,
  onClick,
  external = false,
  trailing,
  chevron = true,
}: {
  icon?: ReactNode;
  label: string;
  value?: string;
  onClick?: () => void;
  /** opens a page outside the app */
  external?: boolean;
  /** replaces the chevron (a switch, a dot) */
  trailing?: ReactNode;
  chevron?: boolean;
}) {
  const body = (
    <>
      {icon && <span className="shrink-0 text-fg">{icon}</span>}
      <span className={cx("min-w-0 truncate text-[16px] leading-[21px]", value ? "max-w-[60%] shrink-0" : "flex-1")}>{label}</span>
      {value && <span className="min-w-0 flex-1 truncate pl-3 text-right text-[14px] leading-[18px] text-muted">{value}</span>}
      {trailing ?? (chevron ? external ? <ExternalLink size={16} className="shrink-0 text-muted/70" /> : <ChevronRight size={18} className="shrink-0 text-muted/70" /> : null)}
    </>
  );
  const cls = "flex min-h-[54px] w-full items-center gap-3.5 px-4 py-2 text-left";
  return onClick ? (
    <button type="button" onClick={onClick} className={cx(cls, "hover:bg-surface-2/60")}>
      {body}
    </button>
  ) : (
    <div className={cls}>{body}</div>
  );
}

/** A row whose trailing piece is a switch; the label is the switch's label. */
export function MuseSwitchRow({
  icon,
  label,
  value,
  checked,
  disabled = false,
  onChange,
}: {
  icon?: ReactNode;
  label: string;
  value?: string;
  checked: boolean;
  disabled?: boolean;
  onChange: (v: boolean) => void;
}) {
  // the whole row is the switch (one control for a screen reader); the knob is decoration
  return (
    <button
      type="button"
      role="switch"
      aria-checked={checked}
      disabled={disabled}
      onClick={() => onChange(!checked)}
      className={cx("flex min-h-[54px] w-full items-center gap-3.5 px-4 py-2 text-left", disabled ? "opacity-60" : "hover:bg-surface-2/60")}
    >
      {icon && <span className="shrink-0 text-fg">{icon}</span>}
      <span className={cx("min-w-0 truncate text-[16px] leading-[21px]", value ? "max-w-[60%] shrink-0" : "flex-1")}>{label}</span>
      {value && <span className="min-w-0 flex-1 truncate pl-3 text-right text-[14px] leading-[18px] text-muted">{value}</span>}
      <MuseSwitch checked={checked} />
    </button>
  );
}

/** The knob alone, decorative; put it inside a control that carries the switch role. */
export function MuseSwitch({ checked }: { checked: boolean }) {
  return (
    <span aria-hidden className={cx("relative h-7 w-12 shrink-0 rounded-full transition", checked ? "bg-accent" : "bg-surface-2 border border-border")}>
      <span className={cx("absolute top-0.5 h-6 w-6 rounded-full bg-white shadow transition", checked ? "left-[22px]" : "left-0.5")} />
    </span>
  );
}

/** The small grey caption under (or inside) a card. */
export function MuseCaption({ children, className }: { children: ReactNode; className?: string }) {
  return <p className={cx("px-8 py-2 text-[13px] leading-[18px] text-muted", className)}>{children}</p>;
}

/** The small grey label over a card. */
export function MuseSectionLabel({ children }: { children: ReactNode }) {
  return <p className="px-8 pb-2 pt-[18px] text-[13px] leading-[18px] text-muted">{children}</p>;
}
