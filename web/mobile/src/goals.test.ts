import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { CADENCES, CATEGORIES, categoryOf, describeCadence, dueLabel, joinCadence, ordinal, splitCadence } from "./goals";
import { setLocaleSetting } from "./i18n";
import zhCN from "./i18n/zh-CN";

describe("the cadence spec", () => {
  beforeEach(() => setLocaleSetting("en"));

  it("splits what the runtime writes and joins it back", () => {
    expect(splitCadence("weekly mon 09:00")).toEqual({ cadence: "weekly", anchor: "mon", time: "09:00" });
    expect(splitCadence("daily 7:30")).toEqual({ cadence: "daily", anchor: "", time: "07:30" });
    expect(splitCadence("monthly 15 18:00")).toEqual({ cadence: "monthly", anchor: "15", time: "18:00" });
    expect(splitCadence("weekdays")).toEqual({ cadence: "weekdays", anchor: "", time: "09:00" });
    expect(splitCadence("")).toEqual({ cadence: "", anchor: "", time: "09:00" });
    expect(splitCadence("every other day")).toEqual({ cadence: "", anchor: "", time: "09:00" });
    expect(joinCadence("weekly", "mon", "09:00")).toBe("weekly mon 09:00");
    expect(joinCadence("daily", "", "07:30")).toBe("daily 07:30");
    expect(joinCadence("", "mon", "09:00")).toBe("");
  });

  it("describes a cadence in the app language", () => {
    expect(describeCadence("daily 08:00")).toBe("Daily at 08:00");
    expect(describeCadence("weekdays 09:00")).toBe("Weekdays at 09:00");
    expect(describeCadence("weekly mon 09:00")).toBe("Mondays at 09:00");
    expect(describeCadence("weekly 09:00")).toBe("Weekly at 09:00");
    expect(describeCadence("monthly 22 09:00")).toBe("Monthly on the 22nd at 09:00");
    expect(describeCadence("")).toBe("");
    setLocaleSetting("zh-CN");
    try {
      expect(describeCadence("daily 08:00")).toBe("每天 08:00");
      expect(describeCadence("weekly mon 09:00")).toBe("每星期一 09:00");
      // Chinese has no ordinal suffixes: the bare day number
      expect(describeCadence("monthly 22 09:00")).toBe("每月 22 日 09:00");
    } finally {
      setLocaleSetting("en");
    }
  });

  it("writes English ordinals", () => {
    expect([1, 2, 3, 4, 11, 12, 13, 21, 22, 23, 31].map(ordinal)).toEqual(["1st", "2nd", "3rd", "4th", "11th", "12th", "13th", "21st", "22nd", "23rd", "31st"]);
  });
});

describe("dueLabel", () => {
  beforeEach(() => {
    setLocaleSetting("en");
    vi.useFakeTimers();
    vi.setSystemTime(new Date(2026, 8, 22, 12, 0, 0));
  });
  afterEach(() => vi.useRealTimers());

  it("counts the days, then names the date", () => {
    expect(dueLabel("", false)).toBe("");
    expect(dueLabel("2026-09-22", false)).toBe("Due today");
    expect(dueLabel("2026-09-23", false)).toBe("Due tomorrow");
    expect(dueLabel("2026-09-30", false)).toBe("Due in 8 days");
    expect(dueLabel("2026-10-30", false)).toMatch(/^By Oct 30$/);
    expect(dueLabel("2027-01-05", false)).toMatch(/^By Jan 5, 2027$/);
    expect(dueLabel("2026-09-01", true)).toMatch(/^Was due Sep 1$/);
  });
});

describe("the life areas", () => {
  it("has one entry per category and finds them by id", () => {
    expect(new Set(CATEGORIES.map((c) => c.id)).size).toBe(CATEGORIES.length);
    expect(categoryOf("health")?.label).toBe("Health");
    expect(categoryOf("other")?.label).toBe("Other");
  });

  it("has every label in Chinese (the labels reach t() from a table, which the i18n scan cannot see)", () => {
    for (const { label } of [...CATEGORIES, ...CADENCES]) expect(zhCN[label], label).toBeTruthy();
  });
});
