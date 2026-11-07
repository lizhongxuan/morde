import { createHash, webcrypto } from 'node:crypto';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { needsMessageUpload, uploadMessage } from './message-upload';

const serviceID = '00000000-0000-4000-8000-000000000001';

afterEach(() => vi.unstubAllGlobals());

describe('resumable message upload', () => {
  it('retries missing UTF-8 chunks using the original identity and verifies the full receipt', async () => {
    vi.stubGlobal('crypto', webcrypto);
    const content = '中文a'.repeat(9000);
    const bytes = new TextEncoder().encode(content);
    const received = new Map<number, Uint8Array>();
    const requests: { path: string; method: string; headers: Headers; body?: Record<string, unknown> }[] = [];
    let interrupted = false;
    const receipt = { id: 'm1', conversation_id: 'c1', sequence: 1, role: 'user', content,
      client_message_id: 'message-1', delivery_status: 'pending', created_at: '2026-11-01T00:00:00Z' };
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const request = input instanceof Request ? new Request(input, init) : new Request(new URL(String(input), window.location.origin), init);
      const path = new URL(request.url).pathname;
      const method = request.method;
      const body = request.body ? await request.clone().json() as Record<string, unknown> : undefined;
      requests.push({ path, method, headers: request.headers, body });
      if (method === 'GET') return Response.json({ client_message_id: 'message-1', state: 'uploading', total_bytes: bytes.length,
        total_chunks: Math.ceil(bytes.length / 16384), received_sequences: [...received.keys()] });
      const sequence = /\/chunks\/(\d+)$/.exec(path);
      if (sequence) {
        if (sequence[1] === '1' && !interrupted) { interrupted = true; throw new Error('offline'); }
        received.set(Number(sequence[1]), Uint8Array.from(atob(String(body?.content_base64)), (char) => char.charCodeAt(0)));
      }
      if (path.endsWith('/commit')) return Response.json({ message: receipt }, { status: 202 });
      if (sequence) return new Response(null, { status: 204 });
      return Response.json({});
    }));
    const request = { client_message_id: 'message-1', content, model_id: 'p1', previous_client_message_id: 'previous' };
    expect(needsMessageUpload(content)).toBe(true);
    await expect(uploadMessage({ conversationID: 'c1' }, request, serviceID, 'u1', () => {})).rejects.toThrow('offline');
    expect(received.has(0)).toBe(true);
    const result = await uploadMessage({ conversationID: 'c1' }, request, serviceID, 'u1', () => {});
    expect(result).toMatchObject({ client_message_id: 'message-1', content });
    expect(requests.filter((item) => item.path.endsWith('/chunks/0'))).toHaveLength(1);
    const restored = Buffer.concat([...received.entries()].sort(([left], [right]) => left - right).map(([, chunk]) => Buffer.from(chunk)));
    expect(restored.equals(Buffer.from(bytes))).toBe(true);
    expect([...received.values()].every((chunk) => chunk.length <= 16384)).toBe(true);
    expect(requests.every((item) => item.headers.get('X-Morde-Service-Instance-ID') === serviceID)).toBe(true);
    expect(requests.every((item) => item.headers.get('X-Morde-Actor-ID') === 'u1')).toBe(true);
    const initialization = requests.find((item) => item.method === 'PUT' && item.path.endsWith('/message-1'))!;
    expect(initialization.headers.get('Idempotency-Key')).toBe('message-1');
    expect(initialization.body).toMatchObject({ total_bytes: bytes.length, total_chunks: Math.ceil(bytes.length / 16384),
      model_id: 'p1', previous_client_message_id: 'previous', sha256: createHash('sha256').update(bytes).digest('hex') });
    expect(initialization.body).not.toHaveProperty('content');
    for (const item of requests.filter((candidate) => /\/chunks\/\d+$/.test(candidate.path))) {
      expect(item.body?.sha256).toBe(createHash('sha256').update(Buffer.from(String(item.body?.content_base64), 'base64')).digest('hex'));
    }
    expect(requests.at(-1)?.headers.get('Idempotency-Key')).toBe('message-1');
  });

  it('retains oversized text and refuses network access above the local limit', async () => {
    const fetch = vi.fn();
    vi.stubGlobal('fetch', fetch);
    await expect(uploadMessage({ projectID: 'p', conversationID: 'c' }, { client_message_id: 'message-2', content: 'x'.repeat(8 * 1024 * 1024 + 1) }, serviceID, 'u1', () => {}))
      .rejects.toThrow('消息超过 8 MiB');
    expect(fetch).not.toHaveBeenCalled();
  });

  it('rejects mismatched upload progress before sending or committing any chunk', async () => {
    vi.stubGlobal('crypto', webcrypto);
    const fetch = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => (input instanceof Request ? input.method : init?.method) === 'PUT'
      ? Response.json({}) : Response.json({ client_message_id: 'different', state: 'uploading', total_bytes: 1, total_chunks: 1, received_sequences: [] }));
    vi.stubGlobal('fetch', fetch);
    await expect(uploadMessage({ projectID: 'p', conversationID: 'c' }, { client_message_id: 'message-3', content: '中'.repeat(20000) }, serviceID, 'u1', () => {}))
      .rejects.toThrow('长消息上传进度无效');
    expect(fetch).toHaveBeenCalledTimes(2);
    expect(new URL((fetch.mock.calls[0]?.[0] as Request).url).pathname).toBe('/api/projects/p/conversations/c/message-uploads/message-3');
  });
});
