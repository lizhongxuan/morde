import { ApiError } from '../api/client';

export type AioEnvelope<T> = {
  success: boolean;
  message: string;
  data: T;
};

export class AioError extends Error {
  constructor(message: string, readonly status?: number, readonly networkFailure = false, readonly responseParseFailure = false) {
    super(message);
    this.name = 'AioError';
  }
}

export function isTransientNetworkError(error: unknown): boolean {
  if (error instanceof SyntaxError) return true;
  if (error instanceof TypeError && /failed to fetch|networkerror|load failed|network request failed|fetch failed|connection lost/i.test(error.message)) return true;
  if (error instanceof ApiError) return error.status >= 500;
  return error instanceof AioError && (error.networkFailure || (error.status !== undefined && error.status >= 500) ||
    error.responseParseFailure && error.status !== undefined && error.status >= 200 && error.status < 300);
}

export async function aioRequest<T>(path: string, init?: RequestInit): Promise<T> {
  const response = await fetch(path, {
    credentials: 'same-origin',
    ...init,
  });
  let envelope: AioEnvelope<T>;
  try {
    envelope = (await response.json()) as AioEnvelope<T>;
  } catch {
    throw new AioError(`AIO 返回了无效响应（HTTP ${response.status}）`, response.status, false, true);
  }
  if (!response.ok || envelope?.success !== true) {
    throw new AioError(envelope?.message || `请求失败（HTTP ${response.status}）`, response.status);
  }
  return envelope.data;
}

export function jsonRequest(body: unknown): RequestInit {
  return {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  };
}

export type ShellResult = {
  status: 'running' | 'completed' | 'no_change_timeout' | 'hard_timeout' | 'terminated';
  exit_code?: number;
  output?: string;
};

export async function runShell(command: string, cwd: string): Promise<void> {
  const result = await aioRequest<ShellResult>(
    '/v1/shell/exec',
    jsonRequest({ command, exec_dir: cwd, timeout: 30 }),
  );
  if (result.status !== 'completed') {
    throw new AioError(result.output || `命令未完成（状态 ${result.status}）`);
  }
  if (result.exit_code !== 0) {
    throw new AioError(result.output || `命令执行失败（退出码 ${result.exit_code ?? '未知'}）`);
  }
}
