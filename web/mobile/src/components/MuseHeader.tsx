import type { ReactNode } from "react";
import type { Profile, Status } from "../types";
import { cx } from "../util";
import { Avatar } from "./Avatar";

/**
 * Muse's page header, the one the Android app draws on the chat and the four tabs: the face
 * centred on its pale disc, the name in a small white tag hanging off the chin with what the
 * agent is doing as the tag's second line, and a round button in each top corner. Two lines
 * are always reserved under the face so the page does not jump when a status appears.
 */
export function MuseHeader({
  profile,
  status,
  name,
  statusLine,
  statusTone = "muted",
  spinning = false,
  badge,
  onAvatar,
  onName,
  leading,
  trailing,
  under,
  size = 72,
  className,
  scrim = false,
}: {
  profile: Profile | null;
  status?: Status;
  name: string;
  statusLine?: string | null;
  /** the second line's colour: muted at rest, accent while working or waiting */
  statusTone?: "muted" | "accent";
  /** a small spinner before the status line */
  spinning?: boolean;
  /** a count on the face's shoulder (queued messages, pending approvals) */
  badge?: number;
  onAvatar?: () => void;
  onName?: () => void;
  leading?: ReactNode;
  trailing?: ReactNode;
  /** a chip under the tag (the side chat's title, the device a chat is addressed to) */
  under?: ReactNode;
  size?: number;
  className?: string;
  /** a shade over a list that scrolls under the header (the chat), instead of the canvas colour */
  scrim?: boolean;
}) {
  return (
    <header className={cx("safe-top relative shrink-0 pt-1.5 pb-1", scrim ? "header-scrim" : "bg-bg", className)}>
      <div className="flex flex-col items-center px-16">
        <button type="button" onClick={onAvatar} aria-label={name} className="relative rounded-full">
          <Avatar profile={profile} status={status} size={size} />
          {badge !== undefined && badge > 0 && (
            <span className="pointer-events-none absolute -right-0.5 top-0 flex h-[18px] min-w-[18px] items-center justify-center rounded-full border-2 border-bg bg-rose-500 px-1 text-[10.5px] font-bold text-white">
              {badge}
            </span>
          )}
        </button>
        {/* the tag rides up over the chin; its area is fixed so the bar keeps its height */}
        <div className="relative z-10 -mt-2.5 flex min-h-[44px] flex-col items-center">
          <MuseNamePill name={name} statusLine={statusLine} statusTone={statusTone} spinning={spinning} onClick={onName ?? onAvatar} />
          {under && <div className="mt-1.5 flex max-w-full items-center">{under}</div>}
        </div>
      </div>
      {leading && <div className="absolute left-4 top-1.5 safe-top">{leading}</div>}
      {trailing && <div className="absolute right-4 top-1.5 safe-top flex items-center gap-2">{trailing}</div>}
    </header>
  );
}

/** Muse's name tag: a small white tag with a faint shadow, the name in regular weight, the status as a smaller grey line inside the same tag. */
export function MuseNamePill({
  name,
  statusLine,
  statusTone = "muted",
  spinning = false,
  onClick,
}: {
  name: string;
  statusLine?: string | null;
  statusTone?: "muted" | "accent";
  spinning?: boolean;
  onClick?: () => void;
}) {
  const inner = (
    <>
      <span className="max-w-[240px] truncate text-[14px] leading-[18px] text-fg">{name}</span>
      {statusLine && (
        <span className={cx("mt-0.5 flex max-w-[240px] items-center gap-1 text-[11.5px] leading-[14px]", statusTone === "accent" ? "text-accent" : "text-muted")}>
          {spinning && <span className="h-2.5 w-2.5 shrink-0 animate-spin rounded-full border-[1.5px] border-current border-t-transparent" />}
          <span className="truncate">{statusLine}</span>
        </span>
      )}
    </>
  );
  const cls = "flex flex-col items-center rounded-[14px] bg-surface px-3 py-1 shadow-[0_1px_4px_rgba(0,0,0,0.16)] dark:border dark:border-border dark:shadow-none";
  return onClick ? (
    <button type="button" onClick={onClick} className={cls}>
      {inner}
    </button>
  ) : (
    <div className={cls}>{inner}</div>
  );
}

/** Muse's corner button: a white disc with one glyph, lifted by a soft shadow (a hairline in the dark). */
export function MuseRoundButton({
  onClick,
  label,
  children,
  dot = false,
  small = false,
  className,
  disabled = false,
  expanded,
}: {
  onClick: () => void;
  label: string;
  children: ReactNode;
  /** a small accent dot at the top right (something is waiting behind it) */
  dot?: boolean;
  /** the 40px disc of a page bar rather than the 44px one of the home header */
  small?: boolean;
  className?: string;
  disabled?: boolean;
  /** for a button that opens a menu */
  expanded?: boolean;
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      aria-label={label}
      aria-haspopup={expanded === undefined ? undefined : "menu"}
      aria-expanded={expanded}
      disabled={disabled}
      className={cx(
        "relative flex items-center justify-center rounded-full bg-surface text-fg shadow-[0_1px_4px_rgba(0,0,0,0.14)] hover:bg-surface-2 disabled:opacity-50 dark:border dark:border-border dark:shadow-none",
        small ? "h-10 w-10" : "h-11 w-11",
        className,
      )}
    >
      {children}
      {dot && <span className="absolute right-[3px] top-[3px] h-[9px] w-[9px] rounded-full border-[1.5px] border-surface bg-accent" />}
    </button>
  );
}

/** Muse's big left-aligned page title ("Goals", "Ideas"). */
export function MusePageTitle({ children, className }: { children: ReactNode; className?: string }) {
  return <h1 className={cx("px-5 pt-1.5 pb-1.5 text-[26px] font-bold leading-8 tracking-tight", className)}>{children}</h1>;
}
