import { describe, expect, it } from "vitest";
import { COLLAPSE_AT, hasSearch, layoutGroups, matchesQuery, orderRows, rowCount, type ModelRow } from "./model-list";

const rows = (prefix: string, n: number): ModelRow[] => Array.from({ length: n }, (_, i) => ({ id: `${prefix}-${i + 1}`, name: `${prefix} ${i + 1}` }));
const ids = (list: ModelRow[]) => list.map((r) => r.id);

describe("the model picker's folded groups and search", () => {
  it("shows a short group whole, in the order it came, with no search field", () => {
    const group = { key: "zhipu", label: "Zhipu GLM", rows: rows("glm", 3) };
    const [shown] = layoutGroups([group]);
    expect(ids(shown.rows)).toEqual(["glm-1", "glm-2", "glm-3"]);
    expect(shown.more).toBe(0);
    expect(hasSearch([group])).toBe(false);
  });

  it("folds past eight rows: exactly eight stay whole, nine fold", () => {
    expect(COLLAPSE_AT).toBe(8);
    const [eight] = layoutGroups([{ key: "a", label: "A", rows: rows("a", 8) }]);
    expect([eight.rows.length, eight.more]).toEqual([8, 0]);
    const [nine] = layoutGroups([{ key: "b", label: "B", rows: rows("b", 9) }]);
    expect([nine.rows.length, nine.more]).toEqual([8, 1]);
  });

  it("orders a folded group: the catalogue default, the current choice, then the rest as listed", () => {
    const group = { key: "openrouter", label: "OpenRouter", rows: rows("or", 40), default: "or-30" };
    const [shown] = layoutGroups([group], { current: { group: "openrouter", id: "or-17" } });
    expect(ids(shown.rows)).toEqual(["or-30", "or-17", "or-1", "or-2", "or-3", "or-4", "or-5", "or-6"]);
    expect(shown.more).toBe(32);
    // a current choice in another group leaves this one alone
    const [other] = layoutGroups([group], { current: { group: "siliconflow", id: "or-17" } });
    expect(ids(other.rows)).toEqual(["or-30", "or-1", "or-2", "or-3", "or-4", "or-5", "or-6", "or-7"]);
  });

  it("skips a default the provider does not list and never shows the current twice", () => {
    expect(ids(orderRows({ key: "p", label: "P", rows: rows("p", 10), default: "p-none" })).slice(0, 3)).toEqual(["p-1", "p-2", "p-3"]);
    const ordered = orderRows({ key: "p", label: "P", rows: rows("p", 10), default: "p-4" }, { group: "p", id: "p-4" });
    expect(ids(ordered).slice(0, 3)).toEqual(["p-4", "p-1", "p-2"]);
    expect(ordered).toHaveLength(10);
  });

  it("keeps the Cloud group's recommended model first, the current choice after it", () => {
    const cloud = { key: "nanomuse_cloud", label: "nanoMuse Cloud", rows: [{ id: "q-rec", name: "Qwen", recommended: true }, ...rows("q", 12)] };
    const [shown] = layoutGroups([cloud], { current: { group: "nanomuse_cloud", id: "q-9" } });
    expect(ids(shown.rows).slice(0, 3)).toEqual(["q-rec", "q-9", "q-1"]);
    expect(shown.more).toBe(5);
  });

  it("unfolds only the expanded group", () => {
    const groups = [
      { key: "a", label: "A", rows: rows("a", 12) },
      { key: "b", label: "B", rows: rows("b", 12) },
    ];
    expect(layoutGroups(groups).map((g) => [g.rows.length, g.more])).toEqual([
      [8, 4],
      [8, 4],
    ]);
    expect(layoutGroups(groups, { expanded: new Set(["b"]) }).map((g) => [g.rows.length, g.more])).toEqual([
      [8, 4],
      [12, 0],
    ]);
  });

  it("adds the search field once the rows across all groups pass eight", () => {
    const four = { key: "a", label: "A", rows: rows("a", 4) };
    expect(hasSearch([four, { key: "b", label: "B", rows: rows("b", 4) }])).toBe(false);
    expect(hasSearch([four, { key: "b", label: "B", rows: rows("b", 5) }])).toBe(true);
    expect(rowCount([four, { key: "b", label: "B", rows: rows("b", 5) }])).toBe(9);
  });

  it("matches the id or the name, case-insensitively, as a substring", () => {
    expect(matchesQuery({ id: "qwen3-vl-plus", name: "Qwen3 VL Plus" }, "VL")).toBe(true);
    expect(matchesQuery({ id: "qwen3-vl-plus", name: "Qwen3 VL Plus" }, "plus")).toBe(true);
    expect(matchesQuery({ id: "glm-4.6v", name: "GLM 4.6V (vision)" }, "VISION")).toBe(true);
    expect(matchesQuery({ id: "glm-4.6v", name: "GLM" }, "gpt")).toBe(false);
    expect(matchesQuery({ id: "x", name: "y" }, "   ")).toBe(true);
  });

  it("shows every match in every group while a query is present, hides groups without one, and folds again when cleared", () => {
    const groups = [
      { key: "cloud", label: "nanoMuse Cloud", rows: [{ id: "qwen-vl", name: "Qwen VL", recommended: true }, { id: "glm-4.6v", name: "GLM 4.6V" }] },
      { key: "openrouter", label: "OpenRouter", rows: [...rows("qwen", 20), ...rows("gpt", 5)] },
      { key: "zhipu", label: "Zhipu GLM", rows: rows("glm", 3) },
    ];
    expect(layoutGroups(groups, { query: "qwen" }).map((g) => [g.key, g.rows.length, g.more])).toEqual([
      ["cloud", 1, 0],
      ["openrouter", 20, 0],
    ]);
    expect(layoutGroups(groups, { query: " GPT " }).map((g) => [g.key, g.rows.length])).toEqual([["openrouter", 5]]);
    expect(layoutGroups(groups, { query: "claude" })).toEqual([]);
    expect(layoutGroups(groups, { query: "" }).map((g) => [g.key, g.rows.length, g.more])).toEqual([
      ["cloud", 2, 0],
      ["openrouter", 8, 17],
      ["zhipu", 3, 0],
    ]);
  });
});
