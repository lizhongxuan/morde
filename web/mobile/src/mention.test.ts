import { describe, expect, it } from "vitest";
import { mentionPrefix, mentionSuggestions, mentionTarget } from "./mention";
import type { HubDevice } from "./types";

const devices: HubDevice[] = [
  { id: "me", name: "Desk", kind: "computer", online: true, this: true },
  { id: "tab", name: "Chrome", kind: "web", online: true },
  { id: "phone-1", name: "Pixel 8", kind: "phone", online: true },
  { id: "mac-1", name: "Mac mini", kind: "computer", online: false },
  { id: "mac-2", name: "MacBook", kind: "computer", online: true },
];

describe("mention", () => {
  it("knows when a mention is being typed", () => {
    expect(mentionPrefix("@")).toBe("");
    expect(mentionPrefix("@Pi")).toBe("Pi");
    expect(mentionPrefix("@Pixel 8 open")).toBeNull();
    expect(mentionPrefix("hello @Pi")).toBeNull();
  });

  it("offers the other devices whose names start with what was typed, online first", () => {
    expect(mentionSuggestions("@", devices).map((d) => d.id)).toEqual(["mac-2", "phone-1", "mac-1"]);
    expect(mentionSuggestions("@mac", devices).map((d) => d.id)).toEqual(["mac-2", "mac-1"]);
    expect(mentionSuggestions("@pix", devices).map((d) => d.id)).toEqual(["phone-1"]);
    expect(mentionSuggestions("@desk", devices)).toEqual([]);
    expect(mentionSuggestions("plain text", devices)).toEqual([]);
  });

  it("names the device a finished mention goes to, like the runtime does", () => {
    expect(mentionTarget("@Pixel 8 open the calendar", devices)?.id).toBe("phone-1");
    expect(mentionTarget("  @mac mini, list ~/Downloads", devices)?.id).toBe("mac-1");
    expect(mentionTarget("@pix what's on screen", devices)?.id).toBe("phone-1");
    // "mac" starts two names: the one online is meant
    expect(mentionTarget("@mac hello", devices)?.id).toBe("mac-2");
    expect(mentionTarget("@Desk hi", devices)).toBeNull();
    expect(mentionTarget("@Chrome hi", devices)).toBeNull();
    expect(mentionTarget("@nobody hi", devices)).toBeNull();
    expect(mentionTarget("hello @Pixel 8", devices)).toBeNull();
  });
});
