import { describe, expect, it } from "vitest";
import { ApiError } from "./client";
import { ERROR_MESSAGES, errorMessage } from "./errors";

describe("portal error messages", () => {
  it("covers every API error code in the contract", () => {
    expect(Object.keys(ERROR_MESSAGES).sort()).toEqual([
      "capacity_exceeded", "forbidden", "host_not_ready", "internal", "invalid_argument", "invalid_state",
      "not_found", "rate_limited", "request_too_large", "unauthenticated", "unsupported", "upstream_error",
      "upstream_timeout", "workspace_busy", "quota_exceeded",
    ].sort());
  });

  it("uses the mapped Chinese message and Retry-After hint", () => {
    const error = new ApiError({ code: "rate_limited", status: 429, message: "", retryAfter: 12 });
    expect(errorMessage(error)).toBe("操作过于频繁，请稍后重试（12 秒后可重试）");
  });

  it("keeps the server message for an unknown future code", () => {
    expect(errorMessage(new ApiError({ code: "new_code", status: 400, message: "具体错误" }))).toBe("具体错误");
  });

  it("hides parser internals when an API returns non-JSON content", () => {
    expect(errorMessage(new SyntaxError(`Unexpected token '<', "<!doctype "... is not valid JSON`)))
      .toBe("服务器响应格式异常，请稍后重试");
  });
});
