import { afterEach, describe, expect, it, vi } from 'vitest';
import { ApiError, consumeServerSentEvents, requestFetch, unwrapApiResponse } from '../api/client';
import { enterHostInNewTab, portalApi } from './api';
import { projectApi } from './project-api';
import { createMessageVariant, messageVariantContentTooLong } from './message-variants';

describe('typed Portal API client', () => {
  afterEach(() => { vi.restoreAllMocks(); vi.unstubAllGlobals(); });

  it('sends a W3C trace context and exposes the server trace ID for diagnostics', async () => {
    let request: Request | undefined;
    const received: { request_id?: string | null; trace_id?: string; status?: number }[] = [];
    const onTrace = (event: Event) => received.push((event as CustomEvent).detail);
    window.addEventListener('morde:api-trace', onTrace);
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      request = new Request(new URL(typeof input === 'string' ? input : input.toString(), window.location.origin), init);
      return new Response('{}', { headers: { 'X-Trace-Id': 'a'.repeat(32) } });
    }));

    try {
      await requestFetch('/api/test', { headers: { 'X-Request-Id': 'existing-request-id' } });
    } finally {
      window.removeEventListener('morde:api-trace', onTrace);
    }

    const traceparent = request!.headers.get('traceparent') ?? '';
    expect(traceparent).toMatch(/^00-[0-9a-f]{32}-[0-9a-f]{16}-01$/);
    const [, traceID, spanID] = traceparent.split('-');
    expect(traceID).not.toBe('0'.repeat(32));
    expect(spanID).not.toBe('0'.repeat(16));
    expect(request!.headers.get('X-Request-Id')).toBe('existing-request-id');
    expect(received).toEqual([{ request_id: 'existing-request-id', trace_id: 'a'.repeat(32), status: 200 }]);
  });

  it('creates a new request and trace ID for each retried request', async () => {
    const requests: Request[] = [];
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      requests.push(input instanceof Request ? new Request(input, init) : new Request(new URL(String(input), window.location.origin), init));
      return Response.json({ ok: true });
    }));

    await requestFetch('/api/test');
    await requestFetch('/api/test');

    const requestIDs = requests.map((request) => request.headers.get('x-request-id'));
    const traceparents = requests.map((request) => request.headers.get('traceparent'));
    expect(requestIDs.every(Boolean)).toBe(true);
    expect(new Set(requestIDs).size).toBe(2);
    expect(traceparents.every((value) => value && /^00-[0-9a-f]{32}-[0-9a-f]{16}-01$/.test(value))).toBe(true);
    expect(new Set(traceparents).size).toBe(2);
  });

  it('retains response trace IDs on typed API errors', () => {
    let thrown: unknown;
    try {
      unwrapApiResponse({ error: { error: { code: 'internal', message: 'unavailable' } }, response: new Response('{}', {
        status: 500, headers: { 'X-Request-Id': '123e4567-e89b-42d3-a456-426614174000', 'X-Trace-Id': 'd'.repeat(32) },
      }) });
    } catch (error) { thrown = error; }
    expect(thrown).toBeInstanceOf(ApiError);
    expect((thrown as ApiError).details).toMatchObject({
      request_id: '123e4567-e89b-42d3-a456-426614174000', trace_id: 'd'.repeat(32),
    });
  });

  it('reads streamed task events while tracing and cancelling the request', async () => {
    let request: Request | undefined;
    const received: { request_id?: string | null; trace_id?: string; status?: number }[] = [];
    const onTrace = (event: Event) => received.push((event as CustomEvent).detail);
    window.addEventListener('morde:api-trace', onTrace);
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      request = new Request(new URL(typeof input === 'string' ? input : input.toString(), window.location.origin), init);
      return new Response('event: task.message\r\ndata: {"part":\r\ndata: "one"}\r\n\r\n', {
        headers: { 'Content-Type': 'text/event-stream', 'X-Trace-Id': 'c'.repeat(32) },
      });
    }));
    const events: { event: string; data: string; id: string }[] = [];
    const controller = new AbortController();
    try {
      const response = await requestFetch('/api/tasks/q1234567/events?after=2', {
        headers: { accept: 'text/event-stream' }, signal: controller.signal,
      });
      await consumeServerSentEvents(response, (event) => events.push(event), controller.signal);
    } finally {
      window.removeEventListener('morde:api-trace', onTrace);
    }

    expect(request!.headers.get('accept')).toBe('text/event-stream');
    expect(request!.headers.get('x-request-id')).toBeTruthy();
    expect(request!.headers.get('traceparent')).toMatch(/^00-[0-9a-f]{32}-[0-9a-f]{16}-01$/);
    expect(events).toEqual([{ event: 'task.message', data: '{"part":\n"one"}', id: '' }]);
    expect(received[0]).toMatchObject({ request_id: request!.headers.get('x-request-id'), trace_id: 'c'.repeat(32), status: 200 });
  });

  it('cancels a pending streamed response when the owning request is cancelled', async () => {
    let streamCancelled = false;
    const stream = new ReadableStream<Uint8Array>({ cancel: () => { streamCancelled = true; } });
    const controller = new AbortController();
    const consuming = consumeServerSentEvents(new Response(stream), () => undefined, controller.signal);
    controller.abort();
    await consuming;
    expect(streamCancelled).toBe(true);
  });

  it('passes trace headers through fetch cancellation without marking it as a network outage', async () => {
    const controller = new AbortController();
    const events: string[] = [];
    const onNetworkError = () => events.push('network-error');
    window.addEventListener('morde:api-network-error', onNetworkError);
    let request: Request | undefined;
    vi.stubGlobal('fetch', vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      request = new Request(new URL(typeof input === 'string' ? input : input.toString(), window.location.origin), init);
      return new Promise<Response>((_resolve, reject) => {
        init?.signal?.addEventListener('abort', () => reject(new TypeError('aborted')), { once: true });
      });
    }));
    try {
      const pending = requestFetch('/api/tasks/q1234567/events', { signal: controller.signal });
      controller.abort();
      await expect(pending).rejects.toThrow('aborted');
    } finally {
      window.removeEventListener('morde:api-network-error', onNetworkError);
    }
    expect(request!.headers.get('x-request-id')).toBeTruthy();
    expect(request!.headers.get('traceparent')).toMatch(/^00-[0-9a-f]{32}-[0-9a-f]{16}-01$/);
    expect(events).toEqual([]);
  });

  it('builds a same-origin URL for a Task artifact download', () => {
    expect(portalApi.taskArtifactContentURL('q1234567', 'a7654321'))
      .toBe('/api/tasks/q1234567/artifacts/a7654321/content');
  });

  it.each(['image/png', 'audio/ogg', 'video/mp4'])('loads only the authorized Task media preview bytes (%s)', async (mediaType) => {
    const calls: { input: RequestInfo | URL; init?: RequestInit }[] = [];
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      calls.push({ input, init });
      return new Response('safe preview bytes', { headers: { 'Content-Type': mediaType, 'Cache-Control': 'private, no-store' } });
    }));

    const preview = await portalApi.taskMediaPreview('q1234567', 'o1234567');

    expect(preview).toBeInstanceOf(Blob);
    expect(preview.type).toBe(mediaType);
    expect(await preview.text()).toBe('safe preview bytes');
    expect(calls).toHaveLength(1);
    const call = calls.at(0)!;
    expect(call.input).toBe('/api/tasks/q1234567/media/o1234567/preview');
    expect(call.init?.credentials).toBe('include');
    expect(call.init?.redirect).toBe('manual');
  });

  it.each(['image/svg+xml', 'image/png; charset=utf-8', 'text/html'])('rejects unsafe media preview MIME without following a provider URL (%s)', async (mediaType) => {
    const calls: { input: RequestInfo | URL; init?: RequestInit }[] = [];
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      calls.push({ input, init });
      return new Response('https://provider.invalid/private-media', {
        headers: { 'Content-Type': mediaType, Location: 'https://provider.invalid/private-media' },
      });
    }));

    await expect(portalApi.taskMediaPreview('q1234567', 'o1234567')).rejects.toThrow('无法安全显示');
    expect(calls).toHaveLength(1);
    const call = calls.at(0)!;
    expect(String(call.input)).toBe('/api/tasks/q1234567/media/o1234567/preview');
    expect(String(call.input)).not.toContain('provider.invalid');
    expect(call.init?.redirect).toBe('manual');
  });

  it('fails closed on a media preview redirect without requesting its destination', async () => {
    const calls: { input: RequestInfo | URL; init?: RequestInit }[] = [];
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      calls.push({ input, init });
      return new Response('provider bytes must not be read', {
        status: 302, headers: { Location: 'https://provider.invalid/private-media' },
      });
    }));

    await expect(portalApi.taskMediaPreview('q1234567', 'o1234567')).rejects.toThrow('已拒绝显示');
    expect(calls).toHaveLength(1);
    expect(calls[0]!.input).toBe('/api/tasks/q1234567/media/o1234567/preview');
    expect(calls[0]!.init?.redirect).toBe('manual');
  });

  it('normalizes empty non-ready main-conversation IDs from the API contract', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => Response.json({ status: 'selection_required', conversation_id: '', version: 0 })));

    await expect(portalApi.mainConversation()).resolves.toEqual({ status: 'selection_required', conversation_id: null, version: 0 });
  });

  it('signals API transport failures and recovery to the client shell', async () => {
    const events: string[] = [];
    const onError = () => events.push('error');
    const onRecovered = () => events.push('recovered');
    window.addEventListener('morde:api-network-error', onError);
    window.addEventListener('morde:api-network-recovered', onRecovered);
    vi.stubGlobal('fetch', vi.fn().mockRejectedValueOnce(new TypeError('Failed to fetch')).mockResolvedValueOnce(Response.json({ ok: true })));

    try {
      await expect(requestFetch('/api/me')).rejects.toThrow('Failed to fetch');
      await expect(requestFetch('/api/me')).resolves.toMatchObject({ status: 200 });
      expect(events).toEqual(['error', 'recovered']);
    } finally {
      window.removeEventListener('morde:api-network-error', onError);
      window.removeEventListener('morde:api-network-recovered', onRecovered);
    }
  });

  it('keeps the client shell in a connection state for failed server responses', async () => {
    const events: string[] = [];
    const onError = () => events.push('error');
    const onRecovered = () => events.push('recovered');
    window.addEventListener('morde:api-network-error', onError);
    window.addEventListener('morde:api-network-recovered', onRecovered);
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(Response.json({ error: { code: 'internal' } }, { status: 503 })));

    try {
      await requestFetch('/api/me');
      expect(events).toEqual(['error']);
    } finally {
      window.removeEventListener('morde:api-network-error', onError);
      window.removeEventListener('morde:api-network-recovered', onRecovered);
    }
  });

  it('reads owner-scoped memory history using the signed cursor and fixed memory route', async () => {
    let request: Request | undefined;
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      request = new Request(input, init);
      return Response.json({ items: [], next_cursor: null });
    }));

    await portalApi.personalMemoryHistory('global', 'signed-history-cursor', 25);

    expect([request!.method, new URL(request!.url).pathname, new URL(request!.url).search]).toEqual([
      'GET', '/api/personal-data/memory/global/history', '?limit=25&cursor=signed-history-cursor',
    ]);
  });

  it('sends only the username and password during registration', async () => {
    let request: Request | undefined;
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      request = new Request(input, init);
      return Response.json({ user: { id: 'u1234567', name: 'new-user', role: 'user', status: 'pending' } }, { status: 201 });
    }));

    await portalApi.register('new-user', 'secret-pass');

    expect([request!.method, new URL(request!.url).pathname]).toEqual(['POST', '/api/auth/register']);
    expect(await request!.json()).toEqual({ name: 'new-user', password: 'secret-pass' });
  });

  it('saves the default model budget with its expected version', async () => {
    let request: Request | undefined;
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      request = new Request(input, init);
      return Response.json({ configured: true, token_limit: 500_000, max_concurrent: 8, version: 1 });
    }));

    await portalApi.updateAdminModelBudgetDefault({ expected_version: 0, token_limit: 500_000, max_concurrent: 8 });

    expect([request!.method, new URL(request!.url).pathname]).toEqual(['PUT', '/api/admin/settings/model-budget-default']);
    expect(await request!.json()).toEqual({ expected_version: 0, token_limit: 500_000, max_concurrent: 8 });
  });

  it('sets or clears the platform default model with the settings CAS version', async () => {
    let request: Request | undefined;
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      request = new Request(input, init);
      return Response.json({ configured: false, model_id: null, version: 2 });
    }));

    await portalApi.updateAdminModelDefault({ expected_version: 1, model_id: '' });

    expect([request!.method, new URL(request!.url).pathname]).toEqual(['PUT', '/api/admin/settings/model-default']);
    expect(await request!.json()).toEqual({ expected_version: 1, model_id: '' });
  });

  it('reads and updates the server-owned main conversation with stable idempotency headers', async () => {
    const requests: Request[] = [];
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const request = new Request(new URL(typeof input === 'string' ? input : input.toString(), window.location.origin), init);
      requests.push(request);
      return Response.json({ status: 'ready', conversation_id: 'c1234567', version: requests.length === 1 ? 1 : 2 });
    }));

    await portalApi.mainConversation();
    await portalApi.ensureMainConversation('main-ensure-001');
    await portalApi.setMainConversation('c1234567', 1, 'main-set-0001');

    expect(requests.map((request) => [request.method, new URL(request.url).pathname])).toEqual([
      ['GET', '/api/me/main-conversation'],
      ['POST', '/api/me/main-conversation/ensure'],
      ['PUT', '/api/me/main-conversation'],
    ]);
    expect(requests[1]?.headers.get('Idempotency-Key')).toBe('main-ensure-001');
    expect(requests[2]?.headers.get('Idempotency-Key')).toBe('main-set-0001');
    expect(await requests[2]!.json()).toEqual({ conversation_id: 'c1234567', expected_version: 1 });
    expect(requests.every((request) => request.credentials === 'include' && request.headers.has('X-Request-Id'))).toBe(true);
  });

  it('rejects an invalid main-conversation response instead of trusting its shape', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => Response.json({ status: 'ready', conversation_id: null, version: 1 })));
    await expect(portalApi.mainConversation()).rejects.toThrow('主会话响应格式无效');
  });

  it('fences new conversation creation to the stored account and service identities', async () => {
    let request: Request | undefined;
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      request = new Request(input, init);
      return Response.json({ id: 'c1' }, { status: 201 });
    }));
    await portalApi.createConversation({}, 'conversation-create-01', 'service-id', 'actor-id');
    expect(request?.headers.get('Idempotency-Key')).toBe('conversation-create-01');
    expect(request?.headers.get('X-Morde-Service-Instance-ID')).toBe('service-id');
    expect(request?.headers.get('X-Morde-Actor-ID')).toBe('actor-id');
  });

  it('fences personal-data deletion and upload cancellation to the stored account and service identities', async () => {
    const requests: Request[] = [];
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const request = input instanceof Request
        ? new Request(input, init)
        : new Request(new URL(typeof input === 'string' ? input : input.toString(), window.location.origin), init);
      requests.push(request);
      return request.method === 'DELETE' && new URL(request.url).pathname.endsWith('/uploads/pu_12345678')
        ? new Response(null, { status: 204 })
        : Response.json({ result_revision: 4, account_change_sequence: 18 });
    }));

    await portalApi.deletePersonalObject('profile', 'default', 3, 'personal-delete-001', 'instance-a', 'u1234567');
    await portalApi.cancelPersonalAttachmentUpload('pu_12345678', 'instance-a', 'u1234567');

    expect(requests.map((request) => [request.method, new URL(request.url).pathname])).toEqual([
      ['DELETE', '/api/personal-data/profile/default'],
      ['DELETE', '/api/personal-data/uploads/pu_12345678'],
    ]);
    expect(requests[0]?.headers.get('Idempotency-Key')).toBe('personal-delete-001');
    expect(await requests[0]!.json()).toEqual({ base_revision: 3 });
    expect(requests.every((request) => request.headers.get('X-Morde-Service-Instance-ID') === 'instance-a'
      && request.headers.get('X-Morde-Actor-ID') === 'u1234567')).toBe(true);
  });

  it('rejects partially supplied personal-data identity before sending a request', async () => {
    const fetch = vi.fn();
    vi.stubGlobal('fetch', fetch);

    await expect(portalApi.deletePersonalObject('profile', 'default', 0, 'personal-delete-002', 'instance-a'))
      .rejects.toThrow('个人数据操作必须同时提供服务实例和账号身份。');
    await expect(portalApi.cancelPersonalAttachmentUpload('pu_12345678', undefined, 'u1234567'))
      .rejects.toThrow('个人数据操作必须同时提供服务实例和账号身份。');
    expect(fetch).not.toHaveBeenCalled();
  });

  it('accepts empty 202 message-processing retries with the correct personal or project scope', async () => {
    const requests: Request[] = [];
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      requests.push(new Request(input, init));
      return new Response(null, { status: 202 });
    }));
    await portalApi.retryConversationMessageProcessing('c1234567', 'm1234567', 'retry-key-001', 'instance-a', 'u1234567');
    await projectApi.retryProjectConversationMessageProcessing('j1234567', 'c1234567', 'm1234567', 'retry-key-002', 'instance-a', 'u1234567');
    expect(requests.map((request) => [request.method, new URL(request.url).pathname])).toEqual([
      ['POST', '/api/conversations/c1234567/messages/m1234567/retry-processing'],
      ['POST', '/api/projects/j1234567/conversations/c1234567/messages/m1234567/retry-processing'],
    ]);
    expect(requests.map((request) => request.headers.get('Idempotency-Key'))).toEqual(['retry-key-001', 'retry-key-002']);
    expect(requests.every((request) => request.headers.get('X-Morde-Actor-ID') === 'u1234567'
      && request.headers.get('X-Morde-Service-Instance-ID') === 'instance-a')).toBe(true);
  });

  it('cancels one personal conversation message with its session and actor scope', async () => {
    let request: Request | undefined;
    const cancelledMessage = { id: 'm1234567', conversation_id: 'c1234567', role: 'user', content: '继续', sequence: 2 };
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      request = new Request(new URL(typeof input === 'string' ? input : input.toString(), window.location.origin), init);
      return Response.json({ message: cancelledMessage, replayed: true }, { status: 202 });
    }));

    await expect(portalApi.cancelConversationMessage('c1234567', 'm1234567', 'instance-a', 'u1234567'))
      .resolves.toMatchObject({ message: cancelledMessage, replayed: true });
    expect(request?.method).toBe('POST');
    expect(new URL(request!.url).pathname).toBe('/api/conversations/c1234567/messages/m1234567/cancel');
    expect(request?.body).toBeNull();
    expect(request?.headers.get('X-Morde-Service-Instance-ID')).toBe('instance-a');
    expect(request?.headers.get('X-Morde-Actor-ID')).toBe('u1234567');
  });

  it('does not send a conversation cancellation without both identity headers', async () => {
    const fetch = vi.fn();
    vi.stubGlobal('fetch', fetch);

    await expect(portalApi.cancelConversationMessage('c1234567', 'm1234567', '', 'u1234567'))
      .rejects.toThrow('服务或账号身份尚未确认。');
    await expect(portalApi.cancelConversationMessage('c1234567', 'm1234567', 'instance-a', ''))
      .rejects.toThrow('服务或账号身份尚未确认。');
    expect(fetch).not.toHaveBeenCalled();
  });

  it('cancels a project conversation message with project, conversation, service, and actor scope', async () => {
    const requests: Request[] = [];
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      requests.push(input instanceof Request ? new Request(input, init) : new Request(new URL(String(input), window.location.origin), init));
      return Response.json({ message: { id: 'm1234567', conversation_id: 'c7654321' }, replayed: false }, { status: 202 });
    }));

    await expect(projectApi.cancelProjectConversationMessage('j1234567', 'c7654321', 'm1234567', 'instance-a', 'u1234567'))
      .resolves.toMatchObject({ message: { id: 'm1234567', conversation_id: 'c7654321' }, replayed: false });
    expect(new URL(requests[0]!.url).pathname).toBe('/api/projects/j1234567/conversations/c7654321/messages/m1234567/cancel');
    expect(requests[0]!.method).toBe('POST');
    expect(requests[0]!.body).toBeNull();
    expect(requests[0]!.headers.get('X-Morde-Service-Instance-ID')).toBe('instance-a');
    expect(requests[0]!.headers.get('X-Morde-Actor-ID')).toBe('u1234567');
  });

  it('rejects a cancellation response for another conversation message', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => Response.json({
      message: { id: 'm7654321', conversation_id: 'c7654321' }, replayed: false,
    }, { status: 202 })));

    await expect(portalApi.cancelConversationMessage('c1234567', 'm1234567', 'instance-a', 'u1234567'))
      .rejects.toThrow('取消消息响应与当前会话不匹配。');
  });

  it('reads personal and project retry eligibility with their exact message and actor scopes', async () => {
    const requests: Request[] = [];
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      requests.push(input instanceof Request ? new Request(input, init) : new Request(new URL(String(input), window.location.origin), init));
      return Response.json({ retry_eligible: false, retry_reason: 'reconciliation_required' });
    }));

    await expect(portalApi.conversationMessageRetryEligibility('c1234567', 'm1234567', 'instance-a', 'u1234567'))
      .resolves.toEqual({ retry_eligible: false, retry_reason: 'reconciliation_required' });
    await expect(projectApi.projectConversationMessageRetryEligibility('j1234567', 'c7654321', 'm7654321', 'instance-a', 'u1234567'))
      .resolves.toEqual({ retry_eligible: false, retry_reason: 'reconciliation_required' });

    expect(requests.map((request) => [request.method, new URL(request.url).pathname])).toEqual([
      ['GET', '/api/conversations/c1234567/messages/m1234567/retry-eligibility'],
      ['GET', '/api/projects/j1234567/conversations/c7654321/messages/m7654321/retry-eligibility'],
    ]);
    expect(requests.every((request) => request.headers.get('X-Morde-Service-Instance-ID') === 'instance-a'
      && request.headers.get('X-Morde-Actor-ID') === 'u1234567')).toBe(true);
  });

  it('posts personal and project variants with the expected-version CAS and scoped idempotency headers', async () => {
    const requests: Request[] = [];
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const request = input instanceof Request ? new Request(input, init) : new Request(new URL(String(input), window.location.origin), init);
      requests.push(request);
      const body = await request.clone().json() as { action: string };
      return Response.json({ operation_id: `op-${requests.length}`, variant_group_id: 'm1234567', variant_version: 2,
        action: body.action, state: 'accepted', user_message_id: 'm7654321' }, { status: 202 });
    }));

    await createMessageVariant({ conversationID: 'c1234567', messageID: 'm1234567', action: 'edit_resend', expectedVersion: 1,
      content: '改写消息', idempotencyKey: 'variant-personal-01', serviceInstanceID: 'instance-a', actorID: 'u1234567' });
    await createMessageVariant({ projectID: 'j1234567', conversationID: 'c7654321', messageID: 'm7654321', action: 'regenerate_text', expectedVersion: 2,
      idempotencyKey: 'variant-project-01', serviceInstanceID: 'instance-a', actorID: 'u1234567' });

    expect(requests.map((request) => [request.method, new URL(request.url).pathname])).toEqual([
      ['POST', '/api/conversations/c1234567/messages/m1234567/variants'],
      ['POST', '/api/projects/j1234567/conversations/c7654321/messages/m7654321/variants'],
    ]);
    expect(requests.map((request) => request.headers.get('Idempotency-Key'))).toEqual(['variant-personal-01', 'variant-project-01']);
    expect(requests.every((request) => request.headers.get('X-Morde-Service-Instance-ID') === 'instance-a'
      && request.headers.get('X-Morde-Actor-ID') === 'u1234567')).toBe(true);
    expect(await requests[0]!.clone().json()).toEqual({ action: 'edit_resend', expected_version: 1, content: '改写消息' });
    expect(await requests[1]!.clone().json()).toEqual({ action: 'regenerate_text', expected_version: 2 });
  });

  it('limits edited variants by Unicode scalar count before sending', async () => {
    const fetch = vi.fn();
    vi.stubGlobal('fetch', fetch);
    expect(messageVariantContentTooLong('🙂'.repeat(32_768))).toBe(false);
    expect(messageVariantContentTooLong('🙂'.repeat(32_769))).toBe(true);
    await expect(createMessageVariant({ conversationID: 'c1234567', messageID: 'm1234567', action: 'edit_resend', expectedVersion: 1,
      content: '🙂'.repeat(32_769), idempotencyKey: 'variant-too-long-01', serviceInstanceID: 'instance-a', actorID: 'u1234567' }))
      .rejects.toThrow('编辑后的消息不能超过 32768 个字符。');
    expect(fetch).not.toHaveBeenCalled();
  });

  it('requests server Workspace diffs and restores selected paths with an idempotency key', async () => {
    const requests: Request[] = [];
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      requests.push(input instanceof Request
        ? new Request(input, init)
        : new Request(new URL(typeof input === 'string' ? input : input.toString(), window.location.origin), init));
      return Response.json({ workspace_id: 'w1234567', target_revision: 1, expected_current_revision: 2, revision: 3, replayed: false });
    }));

    await portalApi.workspaceDiff('w1234567', 1, 2);
    await portalApi.restoreWorkspacePaths('w1234567', {
      target_revision: 1, expected_current_revision: 2, paths: ['README.md'],
    }, 'workspace-restore-key-01');

    expect(requests.map((request) => [request.method, new URL(request.url).pathname, new URL(request.url).search])).toEqual([
      ['GET', '/api/workspaces/w1234567/diff', '?from_revision=1&to_revision=2'],
      ['POST', '/api/workspaces/w1234567/restore', ''],
    ]);
    expect(requests[1]?.headers.get('Idempotency-Key')).toBe('workspace-restore-key-01');
    expect(await requests[1]!.json()).toEqual({ target_revision: 1, expected_current_revision: 2, paths: ['README.md'] });
  });

  it('opens a visible same-origin Host handoff page in a new tab', async () => {
    const tab = { opener: window as Window | null };
    const open = vi.spyOn(window, 'open').mockReturnValue(tab as unknown as Window);
    const enter = vi.spyOn(portalApi, 'enterHost');
    await enterHostInNewTab('h1234567');
    expect(open).toHaveBeenCalledWith(`${window.location.origin}/host-entry/h1234567`, '_blank');
    expect(tab.opener).toBeNull();
    expect(enter).not.toHaveBeenCalled();
  });

  it('opens a shared terminal through the same-origin Host handoff page', () => {
    const tab = { opener: window as Window | null };
    const open = vi.spyOn(window, 'open').mockReturnValue(tab as unknown as Window);
    const ticket = vi.spyOn(portalApi, 'issueObservationTicket');
    enterHostInNewTab('h1234567', 'terminal');
    expect(open).toHaveBeenCalledWith(`${window.location.origin}/host-entry/h1234567?target=terminal`, '_blank');
    expect(tab.opener).toBeNull();
    expect(ticket).not.toHaveBeenCalled();
  });

  it('opens a shared workspace through the same-origin Host handoff page', () => {
    const tab = { opener: window as Window | null };
    const open = vi.spyOn(window, 'open').mockReturnValue(tab as unknown as Window);
    enterHostInNewTab('h1234567', 'workspace');
    expect(open).toHaveBeenCalledWith(`${window.location.origin}/host-entry/h1234567?target=workspace`, '_blank');
    expect(tab.opener).toBeNull();
  });

  it('requests a one-time conversation-bound Host embed ticket without putting credentials in the URL', async () => {
    let request: Request | undefined;
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      request = new Request(input, init);
      return Response.json({
        exchange_url: 'https://h1234567.hosts.example.test/__ch/embed-auth',
        ticket: 'A'.repeat(43), embed_nonce: 'B'.repeat(32), targets: ['browser', 'workspace'],
        expires_at: '2026-11-12T03:00:00.000Z',
      }, { status: 201 });
    }));

    const result = await portalApi.issueHostEmbedTicket('h1234567', {
      conversation_id: 'c1234567', target: 'browser', targets: ['browser', 'workspace'],
    });

    expect([request!.method, new URL(request!.url).pathname, new URL(request!.url).search]).toEqual([
      'POST', '/api/hosts/h1234567/embed-tickets', '',
    ]);
    expect(await request!.json()).toEqual({ conversation_id: 'c1234567', target: 'browser', targets: ['browser', 'workspace'] });
    expect(result.embed_nonce).toBe('B'.repeat(32));
  });

  it('sends schema-typed requests with request IDs and Cookie credentials', async () => {
    const requests: Request[] = [];
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      requests.push(new Request(input, init));
      return Response.json({
        id: 'h1234567', name: '本地测试主机', owner_id: 'u1234567', template_id: 'std-2c4g',
        isolation: 'runsc', state: 'creating', state_reason: '', url: 'https://h1234567.localhost',
        workspaces: [],
        resources: { cpu_milli: 2000, mem_mib: 4096, cpu_limit_milli: 2000, mem_limit_mib: 4096 },
        expires_at: null, last_active_at: null, created_at: '2026-10-20T00:00:00Z',
      }, { status: 201 });
    }));

    expect((await portalApi.createHost({}, 'host-create-key-001')).id).toBe('h1234567');
    const request = requests[0]!;
    expect([request.method, new URL(request.url).pathname]).toEqual(['POST', '/api/hosts']);
    expect(request.headers.get('X-Request-Id')).toBeTruthy();
    expect(request.headers.get('Content-Type')).toContain('application/json');
    expect(request.headers.get('Idempotency-Key')).toBe('host-create-key-001');
    expect(request.credentials).toBe('include');
    expect(await request.json()).toEqual({});
  });

  it('sends a stable idempotency key for a metered model connection check', async () => {
    const requests: Request[] = [];
    const responses = [
      { text: true, tools: false, vision: false, text_stream: false },
      { text: false, tools: true, vision: false, text_stream: false },
    ];
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      requests.push(new Request(input, init));
      return Response.json(responses[requests.length - 1]);
    }));

    await expect(portalApi.checkModelConnection('p1234567', 'model-probe-key-001')).resolves.toEqual({
      text: true, tools: true, vision: false, text_stream: false,
    });

    expect(requests.map((request) => new URL(request.url).pathname)).toEqual(['/api/model-connections/p1234567/check', '/api/model-connections/p1234567/check']);
    expect(requests[0]!.headers.get('Idempotency-Key')).toBe('model-probe-key-001');
    expect(requests[1]!.headers.get('Idempotency-Key')).not.toBe(requests[0]!.headers.get('Idempotency-Key'));
    expect(await requests[0]!.json()).toEqual({ capability: 'text' });
    expect(await requests[1]!.json()).toEqual({ capability: 'tools' });
  });

  it('keeps text capability verified when the tool probe fails', async () => {
    const requests: Request[] = [];
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      requests.push(new Request(input, init));
      if (requests.length === 1) return Response.json({ text: true, tools: false, vision: false, text_stream: false });
      return Response.json({ error: { code: 'upstream_error', message: 'tool probe failed' } }, { status: 502 });
    }));

    await expect(portalApi.checkModelConnection('p1234567', 'model-probe-key-001')).resolves.toEqual({
      text: true, tools: false, vision: false, text_stream: false,
    });
    expect(requests).toHaveLength(2);
    expect(await requests[0]!.json()).toEqual({ capability: 'text' });
    expect(await requests[1]!.json()).toEqual({ capability: 'tools' });
    expect(requests[1]!.headers.get('Idempotency-Key')).not.toBe(requests[0]!.headers.get('Idempotency-Key'));
  });

  it('checks streaming separately with its own idempotency key', async () => {
    let request: Request | undefined;
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      request = new Request(input, init);
      return Response.json({ text: true, tools: false, vision: false, text_stream: true });
    }));
    await portalApi.checkModelTextStreaming('p1234567', 'stream-probe-key-001');
    expect(request?.headers.get('Idempotency-Key')).toBe('stream-probe-key-001');
    expect(await request?.json()).toEqual({ capability: 'text_stream' });
  });

  it('fences every chat attachment upload step and association to the same service', async () => {
    const requests: Request[] = [];
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const request = input instanceof Request ? new Request(input, init)
        : new Request(new URL(String(input), window.location.origin), init);
      requests.push(request);
      const path = new URL(request.url).pathname;
      if (path.endsWith('/attachments/client-attachment-1')) return new Response(null, { status: 204 });
      if (path.endsWith('/commit')) return Response.json({ attachment_id: 'pa_test', result_revision: 2 });
      if (path.includes('/chunks/')) return Response.json({ upload_id: 'pu_test', sequence: 0, replay: false });
      return Response.json({ upload_id: 'pu_test', state: 'active', chunk_count: 1, received_sequences: [], replay: false });
    }));
    const serviceID = '00000000-0000-4000-8000-000000000001';
    await portalApi.replacePersonalObject('library_item', 'chat-test', 0, { name: 'brief.txt', kind: 'artifact' }, 'attachment-object-1', 'u1', serviceID);
    await portalApi.createPersonalAttachmentUpload({ object_id: 'chat-test', base_revision: 1, size_bytes: 5,
      sha256: 'a'.repeat(64), media_type: 'text/plain' }, 'attachment-upload-1', serviceID, 'u1');
    await portalApi.putPersonalAttachmentChunk('pu_test', 0, new Uint8Array([1, 2]).buffer, 'b'.repeat(64), serviceID, 'u1');
    await portalApi.commitPersonalAttachment('pu_test', 'attachment-commit-1', serviceID, 'u1');
    await portalApi.associateConversationAttachment('c1', 'm1', 'client-attachment-1', 'pa_test', serviceID, 'u1');
    expect(requests).toHaveLength(5);
    expect(requests.every((request) => request.headers.get('X-Morde-Service-Instance-ID') === serviceID)).toBe(true);
    expect(requests.every((request) => request.headers.get('X-Morde-Actor-ID') === 'u1')).toBe(true);
    expect(requests[0]?.headers.get('Idempotency-Key')).toBe('attachment-object-1');
    expect(requests[1]?.headers.get('Idempotency-Key')).toBe('attachment-upload-1');
    expect(requests[3]?.headers.get('Idempotency-Key')).toBe('attachment-commit-1');
    expect(await requests[4]?.json()).toEqual({ attachment_id: 'pa_test' });
  });

  it('sends caller-supplied idempotency keys for approval and clarification actions', async () => {
    const requests: Request[] = [];
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      requests.push(new Request(input, init));
      return Response.json({ task_id: 'q1234567', action: 'respond', status: 'queued', unsettled_operation_ids: [] }, { status: 202 });
    }));

    await portalApi.respondToTaskApproval('q1234567', 'k1234567', { version: 4, decision: 'approve' }, 'approval-retry-key');
    await portalApi.answerTaskInput('q1234567', {
      clarification_id: 'k7654321', clarification_version: 2, answer: 'Use the reports directory.',
    }, 'clarification-retry-key');

    expect(requests.map((request) => [new URL(request.url).pathname, request.headers.get('Idempotency-Key')])).toEqual([
      ['/api/tasks/q1234567/approvals/k1234567/responses', 'approval-retry-key'],
      ['/api/tasks/q1234567/inputs', 'clarification-retry-key'],
    ]);
    expect(await requests[0]!.json()).toEqual({ version: 4, decision: 'approve' });
    expect(await requests[1]!.json()).toEqual({ clarification_id: 'k7654321', clarification_version: 2,
      answer: 'Use the reports directory.' });
  });

  it('answers a structured clarification through the versioned route with its idempotency key', async () => {
    let request: Request | undefined;
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      request = new Request(input, init);
      return Response.json({ task_id: 'q1234567', action: 'answer_clarification', status: 'queued', unsettled_operation_ids: [] }, { status: 202 });
    }));
    const body = { version: 4, answers: [
      { question_id: 'output_format', option_ids: ['markdown'], text: '', skipped: false },
      { question_id: 'extra_notes', option_ids: [], text: '', skipped: true },
    ] } satisfies import('../api/schema').components['schemas']['TaskStructuredInputRequest'];

    await portalApi.answerStructuredTaskClarification('q1234567', 'k7654321', body, 'structured-clarification-key');

    expect(new URL(request!.url).pathname).toBe('/api/v2/tasks/q1234567/clarifications/k7654321/responses');
    expect(request!.headers.get('Idempotency-Key')).toBe('structured-clarification-key');
    expect(await request!.json()).toEqual(body);
  });

  it('retries one persisted Task step with only its current revision and attempt', async () => {
    let request: Request | undefined;
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      request = new Request(input, init);
      return Response.json({ task_id: 'q1234567', source_step_id: 's1234567', attempt_step_id: 's7654321',
        attempt: 2, status: 'queued', replayed: false }, { status: 202 });
    }));

    await portalApi.retryTaskStep('q1234567', 's1234567', { expected_task_revision: 4, expected_attempt: 1 }, 'task-step-retry-key');

    expect(request?.method).toBe('POST');
    expect(new URL(request!.url).pathname).toBe('/api/tasks/q1234567/steps/s1234567/retries');
    expect(request?.headers.get('Idempotency-Key')).toBe('task-step-retry-key');
    expect(await request?.json()).toEqual({ expected_task_revision: 4, expected_attempt: 1 });
  });

  it('sends a version-scoped cancellation action with the supplied retry key', async () => {
    let request: Request | undefined;
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      request = new Request(input, init);
      return Response.json({ task_id: 'q1234567', action: 'cancel', status: 'cancelling', unsettled_operation_ids: ['op1234567'] }, { status: 202 });
    }));

    const response = await portalApi.taskAction('q1234567', {
      action: 'cancel', expected_status: 'running', expected_current_revision: 3, expected_desired_revision: 4,
    }, 'task-stop-retry-key');

    expect(response.status).toBe('cancelling');
    expect(new URL(request!.url).pathname).toBe('/api/tasks/q1234567/actions');
    expect(request!.headers.get('Idempotency-Key')).toBe('task-stop-retry-key');
    expect(await request!.json()).toEqual({ action: 'cancel', expected_status: 'running',
      expected_current_revision: 3, expected_desired_revision: 4 });
  });

  it('reads Host control status and sends version-scoped handoff actions', async () => {
    const requests: Request[] = [];
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const request = new Request(input, init);
      requests.push(request);
      if (request.method === 'GET') return Response.json({ host_id: 'h1234567', control_state: 'ai', control_version: 3,
        control_actor_user_id: '', control_lease_expires_at: null, control_lease_active: false, control_lease_held_by_me: false, active_transition: null });
      return Response.json({ host_id: 'h1234567', control_state: 'user', control_version: 5,
        control_actor_user_id: 'u1234567', control_lease_expires_at: '2026-11-02T00:00:30Z', control_lease_active: true,
        control_lease_held_by_me: true, active_transition: null, pending_operation_ids: [] });
    }));

    await expect(portalApi.hostControl('h1234567')).resolves.toMatchObject({ control_state: 'ai', control_version: 3 });
    await expect(portalApi.hostControlAction('h1234567', {
      action: 'take_over', request_id: 'host-takeover-retry-01', expected_control_version: 3,
    })).resolves.toMatchObject({ control_state: 'user', control_version: 5 });

    expect(requests.map((request) => [request.method, new URL(request.url).pathname])).toEqual([
      ['GET', '/api/hosts/h1234567/control'],
      ['POST', '/api/hosts/h1234567/control/actions'],
    ]);
    expect(await requests[1]!.json()).toEqual({ action: 'take_over', request_id: 'host-takeover-retry-01', expected_control_version: 3 });
  });

  it('renews the current Host control lease with its expected control version', async () => {
    let request: Request | undefined;
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      request = new Request(input, init);
      return Response.json({ host_id: 'h1234567', control_state: 'user', control_version: 7,
        control_actor_user_id: 'u1234567', control_lease_expires_at: '2026-11-02T00:00:30Z',
        control_lease_active: true, control_lease_held_by_me: true, active_transition: null });
    }));

    await expect(portalApi.renewHostControlLease('h1234567', 7)).resolves.toMatchObject({ control_lease_held_by_me: true });
    expect([request!.method, new URL(request!.url).pathname, await request!.json()]).toEqual([
      'POST', '/api/hosts/h1234567/control/lease/renew', { expected_control_version: 7 },
    ]);
  });

  it('reads the account Runtime separately from Host state', async () => {
    let request: Request | undefined;
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      request = new Request(input, init);
      return Response.json({ owner_id: 'u1234567', state: 'enabled', epoch: 3, pending_task_count: 1,
        runtime: { state: 'running', runtime_id: 'rt1234567', runtime_generation: 2, max_concurrent_tasks: 4, active_task_count: 1 }, updated_at: '2026-10-25T00:00:00Z' });
    }));

    await expect(portalApi.execution()).resolves.toMatchObject({ state: 'enabled', runtime: { state: 'running' } });
    expect([request!.method, new URL(request!.url).pathname]).toEqual(['GET', '/api/execution']);
  });

  it('requests owner-scoped Site and release pages with bounded cursors', async () => {
    const requests: Request[] = [];
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      requests.push(new Request(input, init));
      return Response.json({ items: [], next_cursor: null });
    }));

    await portalApi.sites('site-cursor');
    await portalApi.siteReleases('site_0123456789abcdef', 'release-cursor');

    expect(requests.map((request) => [request.method, new URL(request.url).pathname, new URL(request.url).search])).toEqual([
      ['GET', '/api/sites', '?limit=100&cursor=site-cursor'],
      ['GET', '/api/sites/site_0123456789abcdef/releases', '?limit=100&cursor=release-cursor'],
    ]);
    expect(requests.every((request) => request.credentials === 'include')).toBe(true);
  });

  it('creates a private Site snapshot using a server-pinned Workspace digest', async () => {
    const requests: Request[] = [];
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const request = new Request(input, init);
      requests.push(request);
      const path = new URL(request.url).pathname;
      if (path === '/api/sites') return Response.json({ id: 'site_0123456789abcdef' }, { status: 201 });
      if (path.endsWith('/materialize')) return Response.json({ id: 'rel_0123456789abcdef', state: 'candidate' });
      return Response.json({ id: 'rel_0123456789abcdef', manifest_sha256: 'a'.repeat(64), state: 'candidate' }, { status: 201 });
    }));

    await portalApi.createSite({ host_id: 'h1234567', workspace_id: 'w1234567' }, 'site-create-key-001');
    await portalApi.createSiteCandidate('site_0123456789abcdef', { workspace_revision: 4, service_port: 3000 }, 'release-create-key-01');
    await portalApi.materializeSiteRelease('site_0123456789abcdef', 'rel_0123456789abcdef');

    expect(requests.map((request) => [request.method, new URL(request.url).pathname, request.headers.get('Idempotency-Key')])).toEqual([
      ['POST', '/api/sites', 'site-create-key-001'],
      ['POST', '/api/sites/site_0123456789abcdef/releases', 'release-create-key-01'],
      ['POST', '/api/sites/site_0123456789abcdef/releases/rel_0123456789abcdef/materialize', null],
    ]);
    expect(await requests[0]!.json()).toEqual({ host_id: 'h1234567', workspace_id: 'w1234567' });
    expect(await requests[1]!.json()).toEqual({ workspace_revision: 4, service_port: 3000 });
    expect(requests.every((request) => request.credentials === 'include' && request.headers.has('X-Request-Id'))).toBe(true);
  });

  it('uses the Host sharing endpoints with the read-only desktop permission', async () => {
    const requests: Request[] = [];
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const request = new Request(input, init);
      requests.push(request);
      if (request.method === 'DELETE') return new Response(null, { status: 204 });
      if (new URL(request.url).pathname === '/api/shared-hosts') return Response.json({ items: [{ id: 'h1234567', name: '共享电脑', state: 'running', url: 'https://h1234567.hosts.test', permissions: ['desktop:view'] }] });
      if (new URL(request.url).pathname.endsWith('/observation-ticket')) return Response.json({ url: `https://h1234567.hosts.test/__ch/auth?t=one-time${new URL(request.url).search ? '&next=%2F__ch%2Fterminal%2F' : ''}`, expires_at: '2026-10-25T00:01:00Z' });
      if (request.method === 'POST') return Response.json({ user_id: 'u7654321', permissions: ['desktop:view'], created_at: '2026-10-25T00:00:00Z' }, { status: 201 });
      return Response.json({ items: [{ user_id: 'u7654321', permissions: ['desktop:view'], created_at: '2026-10-25T00:00:00Z' }] });
    }));

    await expect(portalApi.sharedHosts()).resolves.toMatchObject({ items: [{ id: 'h1234567' }] });
    await expect(portalApi.hostMembers('h1234567')).resolves.toMatchObject({ items: [{ user_id: 'u7654321' }] });
    await expect(portalApi.inviteHostMember('h1234567', { user_id: 'u7654321', permissions: ['desktop:view'] })).resolves.toMatchObject({ user_id: 'u7654321' });
    await portalApi.revokeHostMember('h1234567', 'u7654321');
    await expect(portalApi.issueObservationTicket('h1234567')).resolves.toMatchObject({ url: 'https://h1234567.hosts.test/__ch/auth?t=one-time' });
    await expect(portalApi.issueObservationTicket('h1234567', 'terminal')).resolves.toMatchObject({ url: 'https://h1234567.hosts.test/__ch/auth?t=one-time&next=%2F__ch%2Fterminal%2F' });

    expect(requests.map((request) => [request.method, new URL(request.url).pathname, new URL(request.url).search])).toEqual([
      ['GET', '/api/shared-hosts', ''],
      ['GET', '/api/hosts/h1234567/members', ''],
      ['POST', '/api/hosts/h1234567/members', ''],
      ['DELETE', '/api/hosts/h1234567/members/u7654321', ''],
      ['POST', '/api/hosts/h1234567/observation-ticket', ''],
      ['POST', '/api/hosts/h1234567/observation-ticket', '?target=terminal'],
    ]);
    expect(await requests[2]!.json()).toEqual({ user_id: 'u7654321', permissions: ['desktop:view'] });
    expect(requests.every((request) => request.credentials === 'include' && request.headers.has('X-Request-Id'))).toBe(true);
  });

  it('issues a Task-bound short-lived Workbench observation ticket through the Portal session', async () => {
    let request: Request | undefined;
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      request = new Request(new URL(String(input), window.location.origin), init);
      return Response.json({ ticket: 'opaque-one-time-ticket-123', task_id: 'q1234567', host_id: 'h1234567',
        expires_at: new Date(Date.now() + 30_000).toISOString() });
    }));

    await expect(portalApi.taskWorkbenchObservationTicket('q1234567')).resolves.toMatchObject({
      task_id: 'q1234567', host_id: 'h1234567', ticket: 'opaque-one-time-ticket-123',
    });
    expect([request!.method, new URL(request!.url).pathname, request!.credentials]).toEqual([
      'POST', '/api/tasks/q1234567/workbench-observation-ticket', 'include',
    ]);
    expect(request!.headers.has('X-Request-Id')).toBe(true);
    expect(await request!.clone().text()).toBe('');
  });

  it('reuses a host idempotency key after a lost create response', async () => {
    const keys: string[] = [];
    let attempts = 0;
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const request = new Request(input, init);
      keys.push(request.headers.get('Idempotency-Key') ?? '');
      if (attempts++ === 0) throw new TypeError('主机已创建，但响应丢失');
      return Response.json({ id: 'h1234567' }, { status: 201 });
    }));

    await expect(portalApi.createHost({}, 'host-create-retry-001')).rejects.toThrow('主机已创建，但响应丢失');
    await expect(portalApi.createHost({}, 'host-create-retry-001')).resolves.toMatchObject({ id: 'h1234567' });
    expect(keys).toEqual(['host-create-retry-001', 'host-create-retry-001']);
  });

  it('confirms possible process termination when releasing a host', async () => {
    let request: Request | undefined;
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      request = new Request(input, init);
      return Response.json({ id: 'h1234567', state: 'stopping' }, { status: 202 });
    }));

    await portalApi.releaseHost('h1234567', true);

    expect(request?.method).toBe('POST');
    expect(new URL(request!.url).pathname).toBe('/api/hosts/h1234567/release');
    expect(await request!.json()).toEqual({ confirm_process_termination: true });
  });

  it('does not infer process-termination confirmation when the caller declines it', async () => {
    let request: Request | undefined;
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      request = new Request(input, init);
      return Response.json({ id: 'h1234567', state: 'stopping' }, { status: 202 });
    }));

    await portalApi.releaseHost('h1234567', false);

    expect(await request!.json()).toEqual({ confirm_process_termination: false });
  });

  it('uses durable message acceptance and standalone Task endpoints with stable retry keys', async () => {
    const requests: Request[] = [];
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const request = new Request(input, init);
      requests.push(request);
      if (request.method === 'POST' && new URL(request.url).pathname === '/api/conversations/c1234567/messages') {
        return Response.json({ message: { id: 'm1234567', conversation_id: 'c1234567', sequence: 1, role: 'user',
          content: '继续', client_message_id: 'task-message-key-001', routing_status: 'pending', delivery_status: 'pending',
          created_at: '2026-11-07T00:00:00Z' }, replayed: false }, { status: 202 });
      }
      return Response.json({}, { status: request.method === 'POST' && new URL(request.url).pathname === '/api/conversations/c1234567/messages' ? 202 : 200 });
    }));

    await portalApi.capabilities();
    await portalApi.conversations();
    await portalApi.conversations(undefined, 5, 'planning notes');
    const serviceID = '00000000-0000-4000-8000-000000000001';
    await portalApi.conversationMessages('c1234567', undefined, 'task-message-key-001', serviceID, 'u1234567');
    await portalApi.submitTask({ model_id: 'p1234567', objective: '你好' }, 'task-submit-key-001');
    await portalApi.acceptConversationMessage('c1234567', {
      client_message_id: 'task-message-key-001', model_id: 'p1234567', content: '继续',
    }, 'task-message-key-001', serviceID, 'u1234567');
    await portalApi.task('q1234567');
    await portalApi.answerTaskInput('q1234567', { clarification_id: 'k1234567', clarification_version: 2, answer: '使用默认目录' }, 'task-input-key-001');
    await portalApi.respondToTaskApproval('q1234567', 'k7654321', { version: 3, decision: 'approve' }, 'task-approval-key-001');

    expect(requests.map((request) => [request.method, new URL(request.url).pathname, new URL(request.url).search])).toEqual([
      ['GET', '/api/capabilities', ''],
      ['GET', '/api/conversations', '?limit=100'],
      ['GET', '/api/conversations', '?limit=5&q=planning%20notes'],
      ['GET', '/api/conversations/c1234567/messages', '?limit=100&client_message_id=task-message-key-001'],
      ['POST', '/api/tasks', ''],
      ['POST', '/api/conversations/c1234567/messages', ''],
      ['GET', '/api/tasks/q1234567', ''],
      ['POST', '/api/tasks/q1234567/inputs', ''],
      ['POST', '/api/tasks/q1234567/approvals/k7654321/responses', ''],
    ]);
    expect(requests[4]?.headers.get('Idempotency-Key')).toBe('task-submit-key-001');
    expect(requests[5]?.headers.get('Idempotency-Key')).toBe('task-message-key-001');
    expect(requests[3]?.headers.get('X-Morde-Service-Instance-ID')).toBe(serviceID);
    expect(requests[5]?.headers.get('X-Morde-Service-Instance-ID')).toBe(serviceID);
    expect(requests[3]?.headers.get('X-Morde-Actor-ID')).toBe('u1234567');
    expect(requests[5]?.headers.get('X-Morde-Actor-ID')).toBe('u1234567');
    expect(requests[7]?.headers.get('Idempotency-Key')).toBe('task-input-key-001');
    expect(requests[8]?.headers.get('Idempotency-Key')).toBe('task-approval-key-001');
    expect(await requests[4]?.json()).toEqual({ model_id: 'p1234567', objective: '你好' });
    expect(await requests[5]?.json()).toEqual({ client_message_id: 'task-message-key-001', model_id: 'p1234567', content: '继续' });
    expect(requests.every((request) => request.credentials === 'include')).toBe(true);
  });

  it('loads every page of active tasks for a conversation', async () => {
    const requests: string[] = [];
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = new URL((input instanceof Request ? input : new Request(input, init)).url);
      requests.push(`${url.pathname}${url.search}`);
      return Response.json(url.searchParams.has('cursor')
        ? { items: [{ id: 'q7654321' }], next_cursor: null }
        : { items: [{ id: 'q1234567' }], next_cursor: 'next-page' });
    }));
    const page = await portalApi.conversationTasks('c1234567');
    expect(page.items.map((item) => item.id)).toEqual(['q1234567', 'q7654321']);
    expect(requests).toEqual(['/api/conversations/c1234567/tasks?limit=100', '/api/conversations/c1234567/tasks?limit=100&cursor=next-page']);
  });

  it('loads recent tasks for the selected conversation without changing the active default', async () => {
    const requests: string[] = [];
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const request = new Request(input, init);
      const url = new URL(request.url);
      requests.push(`${url.pathname}${url.search}`);
      if (url.searchParams.get('view') === 'recent' && !url.searchParams.has('cursor')) {
        return Response.json({ items: [{ id: 'q1234567' }], next_cursor: 'recent-next', pending_count: 0 });
      }
      if (url.searchParams.get('view') === 'recent') {
        return Response.json({ items: [{ id: 'q7654321' }], next_cursor: null, pending_count: 0 });
      }
      return Response.json({ items: [], next_cursor: null, pending_count: 0 });
    }));

    const recent = await portalApi.conversationTasks('c1234567', { view: 'recent' });
    await portalApi.conversationTasks('c1234567');

    expect(recent.items.map((item) => item.id)).toEqual(['q1234567', 'q7654321']);
    expect(requests).toEqual([
      '/api/conversations/c1234567/tasks?limit=100&view=recent',
      '/api/conversations/c1234567/tasks?limit=100&view=recent&cursor=recent-next',
      '/api/conversations/c1234567/tasks?limit=100',
    ]);
  });

  it('loads a page of approval snapshots scoped to the selected conversation', async () => {
    let request: Request | undefined;
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      request = new Request(input, init);
      return Response.json({ items: [], next_cursor: 'approval-next', pending_count: 2 });
    }));

    const page = await portalApi.taskApprovals({ conversationID: 'c1234567', cursor: 'approval-cursor' });

    expect(page.pending_count).toBe(2);
    expect(new URL(request!.url).pathname).toBe('/api/me/task-approvals');
    expect(new URL(request!.url).search).toBe('?limit=100&conversation_id=c1234567&cursor=approval-cursor');
  });

  it('loads user-wide tasks filtered by status without a conversation scope', async () => {
    let request: Request | undefined;
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      request = new Request(input, init);
      return Response.json({ items: [], next_cursor: null });
    }));

    await portalApi.tasks({ status: 'waiting_user' });

    expect(new URL(request!.url).pathname).toBe('/api/tasks');
    expect(new URL(request!.url).search).toBe('?limit=100&status=waiting_user');
  });

  it('requires explicit confirmation before deleting host-owned storage', async () => {
    const requests: Request[] = [];
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const request = new Request(input, init);
      requests.push(request);
      return Response.json({
        id: 'h1234567', name: '测试电脑', owner_id: 'u1234567', template_id: 'std', isolation: 'container', state: 'deleting', state_reason: '', runtime_generation: 1, control_version: 1, control_state: 'ai', url: '', workspaces: [],
        resources: { cpu_milli: 500, mem_mib: 1024, cpu_limit_milli: 1000, mem_limit_mib: 2048 }, expires_at: null, last_active_at: null, created_at: '',
      });
    }));

    await portalApi.deleteHost('h1234567', true);

    expect(requests).toHaveLength(1);
    expect(requests[0]?.method).toBe('DELETE');
    expect(new URL(requests[0]!.url).pathname).toBe('/api/hosts/h1234567');
    expect(new URL(requests[0]!.url).search).toBe('?confirm_delete_workspace=true');
  });

  it('retries Host workspace sync with the observed binding version and preserves stale-version errors', async () => {
    const requests: Request[] = [];
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const request = new Request(new URL(String(input), window.location.origin), init);
      requests.push(request);
      if (requests.length === 1) return Response.json({ sync_state: 'healthy', sync_conflicts: [] });
      return Response.json({ error: { code: 'workspace_conflict', message: '工作区绑定版本已变化' } }, { status: 409 });
    }));

    await expect(portalApi.retryHostWorkspaceSync('h1234567', 'w1234567', 4)).resolves.toBeUndefined();
    await expect(portalApi.retryHostWorkspaceSync('h1234567', 'w1234567', 3)).rejects.toMatchObject<ApiError>({
      name: 'ApiError', code: 'workspace_conflict', message: '工作区绑定版本已变化', status: 409,
    });

    expect(requests.map((request) => [request.method, new URL(request.url).pathname])).toEqual([
      ['POST', '/api/hosts/h1234567/workspace-bindings/w1234567/retry-sync'],
      ['POST', '/api/hosts/h1234567/workspace-bindings/w1234567/retry-sync'],
    ]);
    expect(await requests[0]!.json()).toEqual({ expected_binding_version: 4 });
    expect(requests[0]!.headers.get('X-Request-Id')).toBeTruthy();
  });

  it('reads workspace device sync summaries from the owner-only endpoint', async () => {
    let request: Request | undefined;
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      request = new Request(input, init);
      return Response.json({ items: [] });
    }));

    await expect(portalApi.workspaceDeviceSyncProgress('w1234567')).resolves.toEqual({ items: [] });
    expect(request?.method).toBe('GET');
    expect(new URL(request!.url).pathname).toBe('/api/workspaces/w1234567/sync-progress');
  });

  it('unwraps the nested ErrorResponse and Retry-After header', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => Response.json({
      error: { code: 'rate_limited', message: '请稍后重试', details: { retry_after: 7 } },
    }, { status: 429, headers: { 'Retry-After': '7' } })));

    await expect(portalApi.hosts()).rejects.toMatchObject<ApiError>({
      name: 'ApiError', code: 'rate_limited', message: '请稍后重试', status: 429,
      retryAfter: 7, details: { retry_after: 7 },
    });
  });

  it('accepts successful no-content responses', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => new Response(null, { status: 204 })));
    await expect(portalApi.logout()).resolves.toBeUndefined();
  });

  it('begins MCP OAuth through the authenticated Portal endpoint without putting client secrets in the URL', async () => {
    let request: Request | undefined;
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      request = input instanceof Request
        ? new Request(input, init)
        : new Request(new URL(typeof input === 'string' ? input : input.toString(), window.location.origin), init);
      return Response.json({ kind: 'authorize', authorization_url: 'https://auth.example.test/authorize?state=opaque', redirect_uri: 'https://morde.example.test/api/mcp/oauth/callback' });
    }));

    await portalApi.beginMCPConnectorOAuth('k1234567', { expected_revision: 4, client_id: 'local-client', client_secret: 'local-client-secret' });

    expect([request!.method, new URL(request!.url).pathname, new URL(request!.url).search]).toEqual(['POST', '/api/mcp/connectors/k1234567/oauth/authorize', '']);
    expect(await request!.json()).toEqual({ expected_revision: 4, client_id: 'local-client', client_secret: 'local-client-secret' });
    expect(request!.credentials).toBe('include');
  });
});
