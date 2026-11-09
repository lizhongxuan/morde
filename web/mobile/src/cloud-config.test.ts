import { describe, expect, it } from "vitest";
import { passwordMinLength } from "./cloud-config";

describe("passwordMinLength", () => {
  it.each([6, 8, 12])("uses a relay minimum of %i", (minimum) => {
    expect(passwordMinLength({ password_min_len: minimum })).toBe(minimum);
  });

  it("falls back to eight for an older relay", () => {
    expect(passwordMinLength({})).toBe(8);
  });
});
