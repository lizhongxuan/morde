import { describe, expect, it } from "vitest";
import { FENCE_NAMING, stripFences, stripNamingFence } from "./fences";

const BLOCK = '```nanomuse-naming\n{"addressGiven": true, "userAddress": "Lin"}\n```';

describe("stripFences", () => {
  it("removes a complete block and the blank line it left", () => {
    expect(stripNamingFence(`Nice to meet you, Lin.\n\n${BLOCK}\n`)).toBe("Nice to meet you, Lin.");
    expect(stripNamingFence(`${BLOCK}\nHello there.`)).toBe("\nHello there.");
  });

  it("leaves text without the tag alone, other fences included", () => {
    const code = "Here:\n```python\nprint(1)\n```\n";
    expect(stripNamingFence(code)).toBe(code);
    expect(stripFences("plain", FENCE_NAMING)).toBe("plain");
  });

  it("cuts from an opener that never closed, whole or still streaming", () => {
    expect(stripNamingFence('Sure.\n```nanomuse-naming\n{"agentName": "Ju')).toBe("Sure.");
    expect(stripNamingFence("Sure.\n```nanomuse-naming", true)).toBe("Sure.");
    expect(stripNamingFence("Sure.\n```nanomuse-na", true)).toBe("Sure.");
    expect(stripNamingFence("Sure.\n``", true)).toBe("Sure.\n``");
    expect(stripNamingFence("Sure.\n```", true)).toBe("Sure.");
  });

  it("holds back a trailing ``` only while streaming; a complete reply keeps its code fences", () => {
    expect(stripNamingFence("Sure.\n```", false)).toBe("Sure.\n```");
    // the closing fence of a code block is hidden for the moment it could still be an opener
    expect(stripNamingFence("Run\n```sh\nls\n```", true)).toBe("Run\n```sh\nls");
    expect(stripNamingFence("Run\n```sh\nls\n```\n", false)).toBe("Run\n```sh\nls\n```\n");
    expect(stripNamingFence("Run\n```sh\nls\n```\nDone.", true)).toBe("Run\n```sh\nls\n```\nDone.");
  });
});
