import { describe, expect, it } from "vitest";
import { clearMarks, filterPages, HIT_CLASS, hitLabel, markMatches, matches, queryWords, splitAround } from "./settings-search";

// Finding a setting in the settings: the words of a query, the pages whose title
// answers them, and the text cut around a match. The same cases the desktop
// app's tests state for its copy of the rule.

describe("queryWords", () => {
  it("a query is its words, lower case, the blanks dropped", () => {
    expect(queryWords("  Language  ")).toEqual(["language"]);
    expect(queryWords("Hot Key")).toEqual(["hot", "key"]);
    expect(queryWords("   ")).toEqual([]);
  });
});

describe("matches", () => {
  it("a text answers a query when it holds every word of it", () => {
    expect(matches("Language", "language")).toBe(true);
    expect(matches("Language", "LANGUAGE")).toBe(true);
    expect(matches("Language", "lang")).toBe(true);
    expect(matches("Language", "appearance")).toBe(false);
    // every word has to be there, not one of them
    expect(matches("Quick chat", "chat quick")).toBe(true);
    expect(matches("Quick chat", "chat voice")).toBe(false);
    // an empty query answers everything
    expect(matches("Language", "")).toBe(true);
    expect(matches("Language", "  ")).toBe(true);
  });
});

describe("splitAround", () => {
  it("the text is cut around the words that matched, and the pieces make the text again", () => {
    const parts = splitAround("Language and Appearance", "language");
    expect(parts).toEqual([
      { text: "Language", hit: true },
      { text: " and Appearance", hit: false },
    ]);
    expect(parts.map((p) => p.text).join("")).toBe("Language and Appearance");

    // every word is marked where it stands
    expect(splitAround("hot key", "hot key")).toEqual([
      { text: "hot", hit: true },
      { text: " ", hit: false },
      { text: "key", hit: true },
    ]);

    // nothing matched, the text stands as it was
    expect(splitAround("Language", "appearance")).toEqual([{ text: "Language", hit: false }]);
    expect(splitAround("", "language")).toEqual([]);
  });
});

describe("filterPages", () => {
  it("the pages whose title answers the query keep the order they came in", () => {
    const pages = [
      { id: "general", label: "General" },
      { id: "models", label: "Models" },
      { id: "language", label: "Language" },
      { id: "advanced", label: "Advanced" },
    ];
    expect(filterPages(pages, "model").map((p) => p.id)).toEqual(["models"]);
    expect(filterPages(pages, "lang").map((p) => p.id)).toEqual(["language"]);
    expect(filterPages(pages, "an").map((p) => p.id)).toEqual(["language", "advanced"]);
    // an empty query keeps them all
    expect(filterPages(pages, "").map((p) => p.id)).toEqual(["general", "models", "language", "advanced"]);
  });
});

describe("hitLabel", () => {
  it("the words of a hit are short enough to sit in a list", () => {
    expect(hitLabel({ textContent: "  Language \n  row " } as unknown as Element)).toBe("Language row");
    const long = "x".repeat(200);
    const label = hitLabel({ textContent: long } as unknown as Element);
    expect(label.length).toBe(80);
    expect(label.endsWith("…")).toBe(true);
  });
});

// The marking half, driven by the same hand-built fakes the desktop tests use:
// the mark goes on the element a matched word sits in, controls stay unmarked,
// and a new query takes the old marks off.

type FakeElement = {
  textContent: string;
  parentElement: FakeElement | null;
  classes: Set<string>;
  classList: { add: (name: string) => void; remove: (name: string) => void };
  closest: () => object | null;
  querySelectorAll?: () => FakeElement[];
};

function element(textContent: string, parentElement: FakeElement | null = null, excluded = false): FakeElement {
  const classes = new Set<string>();
  return {
    textContent,
    parentElement,
    classes,
    classList: { add: (name) => classes.add(name), remove: (name) => classes.delete(name) },
    closest: () => (excluded ? {} : null),
  };
}

function withTextNodes(nodes: Array<{ textContent: string; parentElement: FakeElement }>, run: () => void): void {
  const previousDocument = Object.getOwnPropertyDescriptor(globalThis, "document");
  const previousNodeFilter = Object.getOwnPropertyDescriptor(globalThis, "NodeFilter");
  Object.defineProperty(globalThis, "NodeFilter", { configurable: true, value: { SHOW_TEXT: 4 } });
  Object.defineProperty(globalThis, "document", {
    configurable: true,
    value: {
      createTreeWalker: () => {
        let index = 0;
        return { nextNode: () => nodes[index++] ?? null };
      },
    },
  });
  try {
    run();
  } finally {
    if (previousDocument) Object.defineProperty(globalThis, "document", previousDocument);
    else delete (globalThis as { document?: unknown }).document;
    if (previousNodeFilter) Object.defineProperty(globalThis, "NodeFilter", previousNodeFilter);
    else delete (globalThis as { NodeFilter?: unknown }).NodeFilter;
  }
}

describe("markMatches", () => {
  it("a long matching paragraph is highlighted without marking the entire settings body", () => {
    const text = "Language settings " + "explanation ".repeat(50);
    const root = element(text + "Unrelated setting");
    const paragraph = element(text, root as unknown as FakeElement);
    const unrelated = element("Unrelated setting", root as unknown as FakeElement);
    root.querySelectorAll = () => [paragraph, unrelated].filter((child) => child.classes.has(HIT_CLASS));
    withTextNodes(
      [
        { textContent: text, parentElement: paragraph },
        { textContent: unrelated.textContent, parentElement: unrelated },
      ],
      () => {
        expect(markMatches(root as unknown as HTMLElement, "language")).toEqual([paragraph]);
        expect(paragraph.classes.has(HIT_CLASS)).toBe(true);
        expect(root.classes.has(HIT_CLASS)).toBe(false);
        expect(unrelated.classes.has(HIT_CLASS)).toBe(false);
        expect(markMatches(root as unknown as HTMLElement, "unrelated")).toEqual([unrelated]);
        expect(paragraph.classes.has(HIT_CLASS)).toBe(false);
        expect(markMatches(root as unknown as HTMLElement, "")).toEqual([]);
        expect(unrelated.classes.has(HIT_CLASS)).toBe(false);
      },
    );
  });

  it("matching text nodes on one element produce one hit and excluded controls stay unmarked", () => {
    const root = element("Language language language");
    const paragraph = element("Language language", root as unknown as FakeElement);
    const control = element("language", root as unknown as FakeElement, true);
    root.querySelectorAll = () => [paragraph, control].filter((child) => child.classes.has(HIT_CLASS));
    withTextNodes(
      [
        { textContent: "Language", parentElement: paragraph },
        { textContent: "language", parentElement: paragraph },
        { textContent: "language", parentElement: control },
      ],
      () => {
        expect(markMatches(root as unknown as HTMLElement, "language")).toEqual([paragraph]);
        expect(control.classes.has(HIT_CLASS)).toBe(false);
      },
    );
  });

  it("clearMarks takes the marks off again", () => {
    const root = element("Language");
    const paragraph = element("Language", root as unknown as FakeElement);
    root.querySelectorAll = () => [paragraph].filter((child) => child.classes.has(HIT_CLASS));
    withTextNodes([{ textContent: "Language", parentElement: paragraph }], () => {
      markMatches(root as unknown as HTMLElement, "language");
      expect(paragraph.classes.has(HIT_CLASS)).toBe(true);
      clearMarks(root as unknown as HTMLElement);
      expect(paragraph.classes.has(HIT_CLASS)).toBe(false);
    });
  });
});
