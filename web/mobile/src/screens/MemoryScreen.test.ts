import { createElement } from "react";
import { act, create, type ReactTestRenderer } from "react-test-renderer";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { api } from "../api";
import { setLocaleSetting } from "../i18n";
import type { MemoryChange } from "../types";
import { MemoryScreen } from "./MemoryScreen";

vi.mock("../store", () => ({
  useStore: () => ({
    state: { profile: null, memoryVersion: 0 },
    toast: () => {},
    openThread: () => {},
    setTab: () => {},
  }),
}));

// A forget logged by the runtime: the removed line in before, nothing after.
const forgetChange: MemoryChange = {
  id: "change-1",
  at: "2026-10-21T12:00:00Z",
  action: "forget",
  before: [
    {
      id: "memory-1",
      content: "Home address is 1 Test Street",
      category: "profile",
      created_at: "2026-10-20T12:00:00Z",
      source: "user",
    },
  ],
  after: null,
  reason: "",
  restored: false,
};

let screen: ReactTestRenderer;

function renderedText(): string {
  const walk = (node: unknown): string => {
    if (typeof node === "string") return node;
    if (Array.isArray(node)) return node.map(walk).join(" ");
    if (node && typeof node === "object" && "children" in node) {
      return walk((node as { children: unknown }).children);
    }
    return "";
  };
  return walk(screen.toJSON());
}

beforeEach(() => {
  vi.spyOn(api, "memory").mockResolvedValue([]);
  vi.spyOn(api, "memoryChanges").mockResolvedValue([forgetChange]);
});

afterEach(() => {
  if (screen) act(() => screen.unmount());
  setLocaleSetting("en");
  vi.restoreAllMocks();
});

describe("MemoryScreen recent changes", () => {
  it("labels a forget change Forgot, not Updated", async () => {
    setLocaleSetting("en");
    await act(async () => {
      screen = create(createElement(MemoryScreen));
    });
    const text = renderedText();
    expect(text).toContain("Forgot");
    expect(text).not.toContain("Updated");
    expect(text).toContain("Home address is 1 Test Street");
  });

  it("labels a forget change 已忘记 in zh-CN", async () => {
    setLocaleSetting("zh-CN");
    await act(async () => {
      screen = create(createElement(MemoryScreen));
    });
    expect(renderedText()).toContain("已忘记");
  });
});
