import { Briefcase, Compass, GraduationCap, Heart, HeartHandshake, House, Palette, PiggyBank, Tag, Users } from "lucide-react";
import type { ReactNode } from "react";
import { getLocale, intlLocale, t } from "./i18n";
import type { GoalCategory } from "./types";

/**
 * What the goals screen, the status sheet and the ideas list share: Muse's life areas, the
 * cadence spec ("weekly mon 09:00") and its words, the due-date line. Pure, so the goals screen
 * stays a lazy chunk of its own and these have tests.
 */

/** Muse's life areas. Colours are Tailwind classes so the badge and the filter chip agree. */
export const CATEGORIES: Array<{ id: GoalCategory; label: string; icon: (p: { size: number }) => ReactNode; tone: string }> = [
  { id: "health", label: "Health", icon: (p) => <Heart {...p} />, tone: "bg-rose-500/12 text-rose-600 dark:text-rose-300" },
  { id: "finance", label: "Finance", icon: (p) => <PiggyBank {...p} />, tone: "bg-emerald-500/12 text-emerald-600 dark:text-emerald-300" },
  { id: "career", label: "Career", icon: (p) => <Briefcase {...p} />, tone: "bg-sky-500/12 text-sky-600 dark:text-sky-300" },
  { id: "learning", label: "Learning", icon: (p) => <GraduationCap {...p} />, tone: "bg-violet-500/12 text-violet-600 dark:text-violet-300" },
  { id: "relationships", label: "Relationships", icon: (p) => <HeartHandshake {...p} />, tone: "bg-pink-500/12 text-pink-600 dark:text-pink-300" },
  { id: "family", label: "Family", icon: (p) => <Users {...p} />, tone: "bg-amber-500/12 text-amber-600 dark:text-amber-300" },
  { id: "home", label: "Home", icon: (p) => <House {...p} />, tone: "bg-orange-500/12 text-orange-600 dark:text-orange-300" },
  { id: "travel", label: "Travel", icon: (p) => <Compass {...p} />, tone: "bg-cyan-500/12 text-cyan-600 dark:text-cyan-300" },
  { id: "creative", label: "Creative", icon: (p) => <Palette {...p} />, tone: "bg-fuchsia-500/12 text-fuchsia-600 dark:text-fuchsia-300" },
  { id: "other", label: "Other", icon: (p) => <Tag {...p} />, tone: "bg-surface-2 text-muted" },
];

export function categoryOf(id: GoalCategory) {
  return CATEGORIES.find((c) => c.id === id) ?? null;
}

export const CADENCES = [
  { id: "", label: "No reminders" },
  { id: "daily", label: "Every day" },
  { id: "weekdays", label: "Weekdays" },
  { id: "weekly", label: "Once a week" },
  { id: "monthly", label: "Once a month" },
];
export const WEEKDAYS = ["mon", "tue", "wed", "thu", "fri", "sat", "sun"];

/** "weekly mon 09:00" → parts, and back. */
export function splitCadence(spec: string): { cadence: string; anchor: string; time: string } {
  const m = /^(daily|weekdays|weekly|monthly)(?:\s+(\S+?))?(?:\s+(\d{1,2}:\d{2}))?$/.exec(spec.trim());
  if (!m) return { cadence: "", anchor: "", time: "09:00" };
  let anchor = m[2] ?? "";
  let time = m[3] ?? "09:00";
  if (anchor && /^\d{1,2}:\d{2}$/.test(anchor)) {
    time = anchor;
    anchor = "";
  }
  return { cadence: m[1], anchor, time: time.padStart(5, "0") };
}

export function joinCadence(cadence: string, anchor: string, time: string): string {
  if (!cadence) return "";
  return [cadence, anchor, time].filter(Boolean).join(" ");
}

/** "mon" → "Monday" / "星期一", in the UI language. */
export function weekdayName(short: string): string {
  const idx = WEEKDAYS.indexOf(short);
  if (idx < 0) return short;
  const d = new Date(2024, 0, 1 + idx); // 2024-01-01 was a Monday
  return d.toLocaleDateString(intlLocale(), { weekday: "long" });
}

export function describeCadence(spec: string): string {
  const { cadence, anchor, time } = splitCadence(spec);
  switch (cadence) {
    case "daily":
      return t("Daily at {time}", { time });
    case "weekdays":
      return t("Weekdays at {time}", { time });
    case "weekly":
      return anchor && WEEKDAYS.includes(anchor)
        ? t("{day}s at {time}", { day: weekdayName(anchor), time })
        : t("Weekly at {time}", { time });
    case "monthly":
      return t("Monthly on the {day} at {time}", { day: ordinal(Number(anchor || 1)), time });
    default:
      return "";
  }
}

/** "1st", "22nd", or the bare number where the language has no ordinal suffixes. */
export function ordinal(n: number): string {
  if (getLocale() !== "en") return String(n);
  const s = ["th", "st", "nd", "rd"];
  const v = n % 100;
  return `${n}${s[(v - 20) % 10] ?? s[v] ?? s[0]}`;
}

export function dueLabel(due: string, overdue: boolean): string {
  if (!due) return "";
  const d = new Date(`${due}T00:00:00`);
  const label = d.toLocaleDateString(intlLocale(), { month: "short", day: "numeric", year: d.getFullYear() === new Date().getFullYear() ? undefined : "numeric" });
  if (overdue) return t("Was due {date}", { date: label });
  const days = Math.ceil((d.getTime() - Date.now()) / 86_400_000);
  if (days <= 0) return t("Due today");
  if (days === 1) return t("Due tomorrow");
  if (days <= 14) return t("Due in {n} days", { n: days });
  return t("By {date}", { date: label });
}
