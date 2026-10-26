import { aioRequest, AioError, jsonRequest } from './aio';

export type AioFile = {
  name: string;
  path: string;
  is_directory: boolean;
  size?: number | null;
  modified_time?: string;
  extension?: string | null;
};

type FileReadData = { content?: unknown };

export const MAX_UPLOAD_BYTES = 100 * 1024 * 1024;

function isAioFile(value: unknown): value is AioFile {
  if (value === null || typeof value !== 'object' || Array.isArray(value)) return false;
  const file = value as Record<string, unknown>;
  return typeof file.name === 'string' && file.name.length > 0
    && typeof file.path === 'string' && file.path.length > 0
    && typeof file.is_directory === 'boolean'
    && (file.size === undefined || file.size === null || (typeof file.size === 'number' && Number.isFinite(file.size) && file.size >= 0))
    && (file.extension === undefined || file.extension === null || typeof file.extension === 'string');
}

export async function listFiles(path: string, showHidden: boolean): Promise<AioFile[]> {
  const data = await aioRequest<unknown>(
    '/v1/file/list',
    jsonRequest({ path, recursive: false, show_hidden: showHidden, include_size: true }),
  );
  if (data === null || typeof data !== 'object' || Array.isArray(data) || !('files' in data) || !Array.isArray(data.files)) {
    throw new AioError('目录列表响应无效');
  }
  if (!data.files.every(isAioFile)) throw new AioError('目录列表响应无效');
  return data.files as AioFile[];
}

export async function readFile(file: string): Promise<string> {
  const data = await aioRequest<FileReadData>('/v1/file/read', jsonRequest({ file }));
  if (typeof data?.content !== 'string') throw new AioError('文件读取响应无效');
  return data.content;
}

export async function writeFile(file: string, content: string): Promise<void> {
  await aioRequest('/v1/file/write', jsonRequest({ file, content }));
}

export function uploadFile(
  file: File,
  path: string,
  onProgress: (percent: number) => void,
): Promise<void> {
  if (file.size > MAX_UPLOAD_BYTES) {
    return Promise.reject(new AioError('单个文件不能超过 100 MB'));
  }

  return new Promise((resolve, reject) => {
    const request = new XMLHttpRequest();
    const body = new FormData();
    body.append('file', file);
    body.append('path', path);
    request.open('POST', '/v1/file/upload');
    request.withCredentials = true;
    request.upload.onprogress = (event) => {
      if (event.lengthComputable) onProgress(Math.round((event.loaded / event.total) * 100));
    };
    request.onerror = () => reject(new AioError('上传连接失败'));
    request.onabort = () => reject(new AioError('上传已取消'));
    request.onload = () => {
      let envelope: { success?: boolean; message?: string } | null;
      try {
        envelope = JSON.parse(request.responseText) as { success?: boolean; message?: string } | null;
      } catch {
        reject(new AioError('上传响应格式无效'));
        return;
      }
      if (request.status < 200 || request.status >= 300 || envelope?.success !== true) {
        reject(new AioError(envelope?.message || `上传失败（HTTP ${request.status}）`));
        return;
      }
      resolve();
    };
    request.send(body);
  });
}

export function downloadHref(path: string): string {
  return `/v1/file/download?path=${encodeURIComponent(path)}`;
}

export async function downloadFile(path: string, filename: string): Promise<void> {
  const response = await fetch(downloadHref(path), { credentials: 'same-origin' });
  if (!response.ok) throw new AioError(`下载失败（HTTP ${response.status}）`);

  const objectUrl = URL.createObjectURL(await response.blob());
  const link = document.createElement('a');
  link.href = objectUrl;
  link.download = filename;
  link.hidden = true;
  document.body.append(link);
  try {
    link.click();
  } finally {
    link.remove();
    window.setTimeout(() => URL.revokeObjectURL(objectUrl), 1000);
  }
}
