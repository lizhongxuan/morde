import { describe, expect, it } from "vitest";
import { signInDoorNeeded } from "./gate";
import type { CloudAccount } from "./types";

const account = (over: Partial<CloudAccount>): { account: CloudAccount } => ({
  account: { base_url: "https://relay.example", signed_in: false, hint: "", channel: "email", is_model: false, required: true, ...over },
});

describe("signInDoorNeeded", () => {
  it("stands when the runtime asks for an account and nothing else could answer", () => {
    expect(signInDoorNeeded(account({}), { llm_ready: false }, false)).toBe(true);
    expect(signInDoorNeeded(account({}), null, false)).toBe(true);
  });

  it("steps aside for a model of one's own", () => {
    expect(signInDoorNeeded(account({}), { llm_ready: true }, false)).toBe(false);
  });

  it("steps aside once the person chose their own key, and stays aside", () => {
    expect(signInDoorNeeded(account({}), { llm_ready: false }, true)).toBe(false);
  });

  it("never stands for a signed-in account or a runtime that does not ask for one", () => {
    expect(signInDoorNeeded(account({ signed_in: true }), { llm_ready: false }, false)).toBe(false);
    expect(signInDoorNeeded(account({ required: false }), { llm_ready: false }, false)).toBe(false);
    expect(signInDoorNeeded(null, null, false)).toBe(false);
  });
});
