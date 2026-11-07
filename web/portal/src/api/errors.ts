import { ApiError } from "./client";

export const ERROR_MESSAGES: Record<string, string> = {
  invalid_argument: "请检查填写内容",
  unauthenticated: "登录已失效，请重新登录",
  forbidden: "没有执行此操作的权限",
  account_pending: "账号仍在审核中，暂不可登录",
  account_rejected: "账号未获批准，请联系管理员",
  account_disabled: "账号已停用，请联系管理员",
  not_found: "请求的内容不存在",
  conflict: "当前状态已变化，请刷新后核对",
  quota_exceeded: "额度不足或尚未配置，请检查相关额度",
  capacity_exceeded: "当前资源容量不足",
  workspace_busy: "工作目录正在被其他主机使用",
  workspace_conflict: "工作区发生冲突，请刷新后核对",
  workspace_unavailable: "工作区文件暂时不可用，请稍后重试",
  transfer_expired: "文件传输已过期，请重新发起",
  invalid_state: "当前状态不允许此操作",
  idempotency_conflict: "该请求与已受理操作不一致，请核对任务状态",
  event_cursor_expired: "任务事件已过期，正在刷新最新状态",
  unsupported: "当前运行规格不支持此操作",
  request_too_large: "提交内容超过大小限制",
  result_too_large: "任务结果超过可接收大小限制",
  rate_limited: "操作过于频繁，请稍后重试",
  internal: "服务暂时不可用，请稍后重试",
  upstream_error: "主机服务暂时不可用",
  host_not_ready: "主机尚未就绪，请稍后重试",
  upstream_timeout: "主机响应超时，请稍后重试",
};

export function errorMessage(error: unknown): string {
  if (error instanceof SyntaxError) return "服务器响应格式异常，请稍后重试";
  if (error instanceof TypeError && /failed to fetch|networkerror|load failed|network request failed/i.test(error.message)) {
    return "网络连接失败，请检查网络后重试";
  }
  if (!(error instanceof ApiError)) return error instanceof Error ? error.message : "操作失败，请重试";
  const message = ERROR_MESSAGES[error.code] || error.message || "操作失败，请重试";
  return error.retryAfter && error.retryAfter > 0 ? `${message}（${error.retryAfter} 秒后可重试）` : message;
}

export function modelCheckErrorMessage(error: unknown): string {
  if (error instanceof ApiError) {
    if (error.code === "quota_exceeded") return "模型调用额度不足或尚未配置，请在管理后台检查账号的模型预算。";
    if (error.code === "upstream_timeout") return "连接模型服务超时，请检查网络后重试。";
    if (error.code === "upstream_error") {
      switch (error.details?.reason) {
        case "transport": return "无法连接模型服务，请检查网络或代理设置。";
        case "authentication_rejected": return "模型服务拒绝了 API 密钥，请检查密钥是否有效。";
        case "access_rejected": return "模型服务拒绝访问，请检查账号和模型权限。";
        case "quota_exhausted": return "模型服务商的额度已用完，请检查服务商账号。";
        default: return "模型服务检查失败，请检查服务地址和模型配置。";
      }
    }
  }
  return errorMessage(error);
}
