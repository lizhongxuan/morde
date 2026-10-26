import { ApiError } from "./client";

export const ERROR_MESSAGES: Record<string, string> = {
  invalid_argument: "请检查填写内容",
  unauthenticated: "登录已失效，请重新登录",
  forbidden: "没有执行此操作的权限",
  not_found: "请求的内容不存在",
  quota_exceeded: "已达到主机配额上限",
  capacity_exceeded: "当前资源容量不足",
  workspace_busy: "工作目录正在被其他主机使用",
  invalid_state: "当前状态不允许此操作",
  unsupported: "当前运行规格不支持此操作",
  request_too_large: "提交内容超过大小限制",
  rate_limited: "操作过于频繁，请稍后重试",
  internal: "服务暂时不可用，请稍后重试",
  upstream_error: "主机服务暂时不可用",
  host_not_ready: "主机尚未就绪，请稍后重试",
  upstream_timeout: "主机响应超时，请稍后重试",
};

export function errorMessage(error: unknown): string {
  if (error instanceof SyntaxError) return "服务器响应格式异常，请稍后重试";
  if (!(error instanceof ApiError)) return error instanceof Error ? error.message : "操作失败，请重试";
  const message = ERROR_MESSAGES[error.code] || error.message || "操作失败，请重试";
  return error.retryAfter && error.retryAfter > 0 ? `${message}（${error.retryAfter} 秒后可重试）` : message;
}
