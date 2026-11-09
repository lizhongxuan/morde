import { describe, expect, it } from "vitest";
import { markFiles, matchFile, splitBlocks } from "./Markdown";

const files = ["kyoto-notes/packing-list.html", "notes/plan.md", "reports/plan.md", "budget.csv"];

describe("matchFile", () => {
  it("accepts the workspace path, with or without a leading ./", () => {
    expect(matchFile("kyoto-notes/packing-list.html", files)).toBe("kyoto-notes/packing-list.html");
    expect(matchFile("./budget.csv", files)).toBe("budget.csv");
    expect(matchFile("/budget.csv", files)).toBe("budget.csv");
  });

  it("accepts a bare file name only when it names one file", () => {
    expect(matchFile("packing-list.html", files)).toBe("kyoto-notes/packing-list.html");
    expect(matchFile("plan.md", files)).toBeNull(); // notes/plan.md or reports/plan.md?
    expect(matchFile("reports/plan.md", files)).toBe("reports/plan.md");
  });

  it("leaves ordinary code alone", () => {
    expect(matchFile("git status", files)).toBeNull();
    expect(matchFile("list.html", files)).toBeNull(); // not a suffix at a path boundary
    expect(matchFile("", files)).toBeNull();
    expect(matchFile("budget.csv", [])).toBeNull();
  });
});

describe("markFiles", () => {
  it("wraps bare mentions of the thread's files in backticks", () => {
    expect(markFiles("已完成翻译：packing-list.html（保存在工作区根目录）。", files)).toBe(
      "已完成翻译：`packing-list.html`（保存在工作区根目录）。",
    );
    expect(markFiles("Saved to kyoto-notes/packing-list.html.", files)).toBe(
      "Saved to `kyoto-notes/packing-list.html`.",
    );
    expect(markFiles("see budget.csv and reports/plan.md", files)).toBe("see `budget.csv` and `reports/plan.md`");
  });

  it("does not touch code, links, other files or ambiguous names", () => {
    expect(markFiles("run `cat budget.csv`", files)).toBe("run `cat budget.csv`");
    expect(markFiles("```\nbudget.csv\n```", files)).toBe("```\nbudget.csv\n```");
    expect(markFiles("[the budget](budget.csv)", files)).toBe("[the budget](budget.csv)");
    expect(markFiles("old-budget.csv and budget.csv.bak", files)).toBe("old-budget.csv and budget.csv.bak");
    expect(markFiles("draft/packing-list.html", files)).toBe("draft/packing-list.html");
    expect(markFiles("plan.md is either", files)).toBe("plan.md is either"); // two plan.md files
    expect(markFiles("nothing here", [])).toBe("nothing here");
  });
});

describe("splitBlocks", () => {
  it("makes one bubble per paragraph and keeps a list together", () => {
    const blocks = splitBlocks("Hello.\n\nTwo things:\n- a\n- b\n\nDone.");
    expect(blocks.map((b) => b.text)).toEqual(["Hello.", "Two things:\n- a\n- b", "Done."]);
    expect(blocks.every((b) => !b.bare)).toBe(true);
  });

  it("keeps fenced code whole, blank lines included, and marks it bare", () => {
    const blocks = splitBlocks("Run this:\n\n```sh\nls\n\npwd\n```\n\nThen stop.");
    expect(blocks).toEqual([
      { text: "Run this:", bare: false },
      { text: "```sh\nls\n\npwd\n```", bare: true },
      { text: "Then stop.", bare: false },
    ]);
  });

  it("gives tables their own frame and folds a heading into what follows", () => {
    const blocks = splitBlocks("## Plan\n\nFirst step.\n\n| a | b |\n|---|---|\n| 1 | 2 |");
    expect(blocks).toEqual([
      { text: "## Plan\n\nFirst step.", bare: false },
      { text: "| a | b |\n|---|---|\n| 1 | 2 |", bare: true },
    ]);
  });

  it("returns nothing for an empty reply", () => {
    expect(splitBlocks("  \n\n")).toEqual([]);
  });
});
