import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { describe, expect, it } from "vitest";
import { ApiError } from "./client";
import { ERROR_MESSAGES, errorMessage, isTransientNetworkError, modelCheckErrorMessage, modelConnectionSaveErrorMessage } from "./errors";

const contractFixture = JSON.parse(readFileSync(
  resolve(process.cwd(), "../../server/openapi/fixtures/morde-client-v1.json"), "utf8",
)) as {
  error_codes: string[];
  errors: { status: number; client_message: string; retry_after?: string; body: { error: { code: string; message: string } } }[];
};

describe("portal error messages", () => {
  it("covers every API error code in the contract", () => {
    expect(Object.keys(ERROR_MESSAGES).sort()).toEqual(contractFixture.error_codes.slice().sort());
  });

  it.each(contractFixture.errors)("maps the $body.error.code response fixture", (fixture) => {
    const error = new ApiError({ code: fixture.body.error.code, status: fixture.status,
      message: fixture.body.error.message, retryAfter: fixture.retry_after ? Number(fixture.retry_after) : undefined });
    expect(errorMessage(error)).toBe(fixture.client_message);
  });

  it("keeps the server message for an unknown future code", () => {
    expect(errorMessage(new ApiError({ code: "new_code", status: 400, message: "具体错误" }))).toBe("具体错误");
  });

  it("hides parser internals when an API returns non-JSON content", () => {
    expect(errorMessage(new SyntaxError(`Unexpected token '<', "<!doctype "... is not valid JSON`)))
      .toBe("服务器响应格式异常，请稍后重试");
  });

  it("shows a readable message when the browser cannot reach the API", () => {
    expect(errorMessage(new TypeError("Failed to fetch"))).toBe("网络连接失败，请检查网络后重试");
  });

  it("classifies transport, parser, and server failures as transient without hiding client errors", () => {
    expect(isTransientNetworkError(new TypeError("Failed to fetch"))).toBe(true);
    expect(isTransientNetworkError(new SyntaxError("invalid JSON"))).toBe(true);
    expect(isTransientNetworkError(new ApiError({ code: "internal", status: 503, message: "unavailable" }))).toBe(true);
    expect(isTransientNetworkError(new ApiError({ code: "forbidden", status: 403, message: "forbidden" }))).toBe(false);
    expect(isTransientNetworkError(new Error("local validation failed"))).toBe(false);
  });

  it("distinguishes model budget and connection errors from host errors", () => {
    expect(modelCheckErrorMessage(new ApiError({ code: "quota_exceeded", status: 409, message: "quota exceeded" })))
      .toBe("模型调用额度不足或尚未配置，请在管理后台检查账号的模型预算。");
    expect(modelCheckErrorMessage(new ApiError({ code: "upstream_error", status: 502, message: "upstream error", details: { reason: "transport" } })))
      .toBe("无法连接模型服务，请检查网络或代理设置。");
  });

  it.each([
    ["base_url", "服务地址无效或未获服务端允许"],
    ["api_key", "API 密钥不能为空"],
    ["name", "显示名称和模型名"],
    ["model_connection", "上下文窗口"],
    ["body", "客户端与服务端的版本"],
  ])("explains a model save error for %s without exposing the server message", (field, expected) => {
    const error = new ApiError({ code: "invalid_argument", status: 400, message: "private-test-secret", details: { field } });
    expect(modelConnectionSaveErrorMessage(error)).toContain(expected);
    expect(modelConnectionSaveErrorMessage(error)).not.toContain("private-test-secret");
  });
});
