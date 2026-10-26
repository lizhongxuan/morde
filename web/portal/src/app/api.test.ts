import { afterEach, describe, expect, it, vi } from 'vitest';
import { ApiError } from '../api/client';
import { enterHostInNewTab, portalApi } from './api';

describe('typed Portal API client', () => {
  afterEach(() => { vi.restoreAllMocks(); vi.unstubAllGlobals(); });

  it('opens the one-time Host entry without changing its destination', async () => {
    const tab = { opener: null as Window | null, location: { href: '' }, close: vi.fn() };
    vi.spyOn(window, 'open').mockReturnValue(tab as unknown as Window);
    vi.spyOn(portalApi, 'enterHost').mockResolvedValue({ url: 'https://h1234567.hosts.test/__ch/auth?t=one-time', expires_at: '2026-10-25T00:01:00Z' });
    await enterHostInNewTab('h1234567');
    const ticket = new URL(tab.location.href);
    expect(ticket.searchParams.get('t')).toBe('one-time');
    expect(ticket.pathname).toBe('/__ch/auth');
    expect(ticket.searchParams.has('next')).toBe(false);
    expect(tab.close).not.toHaveBeenCalled();
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

    expect((await portalApi.createHost({ template_id: 'std-2c4g', workspace_ids: [] }, 'host-create-key-001')).id).toBe('h1234567');
    const request = requests[0]!;
    expect([request.method, new URL(request.url).pathname]).toEqual(['POST', '/api/hosts']);
    expect(request.headers.get('X-Request-Id')).toBeTruthy();
    expect(request.headers.get('Content-Type')).toContain('application/json');
    expect(request.headers.get('Idempotency-Key')).toBe('host-create-key-001');
    expect(request.credentials).toBe('include');
    expect(await request.json()).toEqual({ template_id: 'std-2c4g', workspace_ids: [] });
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

  it('reuses a host idempotency key after a lost create response', async () => {
    const keys: string[] = [];
    let attempts = 0;
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const request = new Request(input, init);
      keys.push(request.headers.get('Idempotency-Key') ?? '');
      if (attempts++ === 0) throw new TypeError('主机已创建，但响应丢失');
      return Response.json({ id: 'h1234567' }, { status: 201 });
    }));

    await expect(portalApi.createHost({ template_id: 'std-2c4g', workspace_ids: [] }, 'host-create-retry-001')).rejects.toThrow('主机已创建，但响应丢失');
    await expect(portalApi.createHost({ template_id: 'std-2c4g', workspace_ids: [] }, 'host-create-retry-001')).resolves.toMatchObject({ id: 'h1234567' });
    expect(keys).toEqual(['host-create-retry-001', 'host-create-retry-001']);
  });

  it('attaches a supplied retry key or generates a key for task actions', async () => {
    const requests: Request[] = [];
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      requests.push(new Request(input, init));
      return Response.json({ task_id: 'q1234567', action: 'pause', status: 'pausing', unsettled_operation_ids: [] }, { status: 202 });
    }));

    const body = { action: 'pause', expected_status: 'running' } as const;
    await portalApi.taskAction('q1234567', body, 'task-action-retry-001');
    await portalApi.taskAction('q1234567', body);

    expect(requests[0]?.method).toBe('POST');
    expect(new URL(requests[0]!.url).pathname).toBe('/api/tasks/q1234567/actions');
    expect(requests[0]?.headers.get('Idempotency-Key')).toBe('task-action-retry-001');
    const idempotencyKey = requests[1]?.headers.get('Idempotency-Key');
    expect(typeof idempotencyKey).toBe('string');
    expect(idempotencyKey as string).toMatch(/^[A-Za-z0-9._~-]{8,128}$/);
    expect(await requests[0]!.json()).toEqual(body);
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

  it('uses caller-provided idempotency keys for task submission and conversation messages', async () => {
    const requests: Request[] = [];
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      requests.push(new Request(input, init));
      return Response.json({});
    }));

    await portalApi.submitTask({ host_id: 'h1234567', model_id: 'model-a', objective: '检查项目' }, 'task-retry-key-001');
    await portalApi.appendConversationMessage('c1234567', {
      host_id: 'h1234567', model_id: 'model-a', content: '继续检查',
    }, 'message-retry-key-001');
    await portalApi.submitTask({ host_id: 'h1234567', model_id: 'model-a', objective: '默认任务键' });
    await portalApi.appendConversationMessage('c1234567', {
      host_id: 'h1234567', model_id: 'model-a', content: '默认消息键',
    });

    const keys = requests.map((request) => request.headers.get('Idempotency-Key'));
    expect(keys.slice(0, 2)).toEqual(['task-retry-key-001', 'message-retry-key-001']);
    expect(keys[2]).toMatch(/^[A-Za-z0-9._~-]{8,128}$/);
    expect(keys[3]).toMatch(/^[A-Za-z0-9._~-]{8,128}$/);
  });

  it('creates a standalone workspace with an idempotency key and deletes hosts without legacy query flags', async () => {
    const requests: Request[] = [];
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const request = new Request(input, init);
      requests.push(request);
      if (new URL(request.url).pathname === '/api/workspaces') {
        return Response.json({ id: 'w1234567', name: '项目工作区', revision: 0, used_bytes: 0, quota_bytes: 4096, state: 'ready', created_at: '', updated_at: '' }, { status: 201 });
      }
      return Response.json({
        id: 'h1234567', name: '测试电脑', owner_id: 'u1234567', template_id: 'std', isolation: 'container', state: 'deleting', state_reason: '', runtime_generation: 1, control_version: 1, control_state: 'ai', url: '', workspaces: [],
        resources: { cpu_milli: 500, mem_mib: 1024, cpu_limit_milli: 1000, mem_limit_mib: 2048 }, expires_at: null, last_active_at: null, created_at: '',
      });
    }));

    await portalApi.createWorkspace({ name: '项目工作区' });
    await portalApi.deleteHost('h1234567');

    const [workspaceRequest, hostDeleteRequest] = requests;
    expect(workspaceRequest?.method).toBe('POST');
    expect(workspaceRequest?.headers.get('Idempotency-Key')).toMatch(/^[A-Za-z0-9._~-]{8,128}$/);
    expect(await workspaceRequest?.json()).toEqual({ name: '项目工作区' });
    expect(hostDeleteRequest?.method).toBe('DELETE');
    expect(new URL(hostDeleteRequest!.url).pathname).toBe('/api/hosts/h1234567');
    expect(new URL(hostDeleteRequest!.url).search).toBe('');
  });

  it('replays an accepted workspace creation after the response is lost', async () => {
    const accepted = new Map<string, { id: string; name: string; revision: number; used_bytes: number; quota_bytes: number; state: string; created_at: string; updated_at: string }>();
    const keys: string[] = [];
    let attempts = 0;
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const request = new Request(input, init);
      const key = request.headers.get('Idempotency-Key')!;
      keys.push(key);
      let workspace = accepted.get(key);
      if (!workspace) {
        workspace = { id: 'w1234567', name: '项目工作区', revision: 0, used_bytes: 0, quota_bytes: 4096, state: 'ready', created_at: '', updated_at: '' };
        accepted.set(key, workspace);
      }
      if (attempts++ === 0) throw new TypeError('服务器已接受请求，但响应丢失');
      return Response.json(workspace, { status: 201 });
    }));

    await expect(portalApi.createWorkspace({ name: '项目工作区' }, 'workspace-retry-key-001')).rejects.toThrow('服务器已接受请求，但响应丢失');
    await expect(portalApi.createWorkspace({ name: '项目工作区' }, 'workspace-retry-key-001')).resolves.toMatchObject({ id: 'w1234567' });

    expect(keys).toEqual(['workspace-retry-key-001', 'workspace-retry-key-001']);
    expect(accepted.size).toBe(1);
  });

  it('unbinds a workspace with the observed binding version, writer confirmation, and idempotency key', async () => {
    let request: Request | undefined;
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      request = new Request(input, init);
      return Response.json({
        workspace: { id: 'w1234567', name: '项目工作区', revision: 2, used_bytes: 1, state: 'ready' },
        directory: 'projects/w1234567', state: 'unbinding', binding_version: 3, desired_revision: 2, applied_revision: 2,
      }, { status: 202 });
    }));

    const response = await portalApi.unbindWorkspaceFromHost('h1234567', 'w1234567', {
      expected_binding_version: 2, writers_stopped: true,
    });

    expect(response.state).toBe('unbinding');
    expect(request?.method).toBe('DELETE');
    expect(new URL(request!.url).pathname).toBe('/api/hosts/h1234567/workspace-bindings/w1234567');
    expect(request!.headers.get('Idempotency-Key')).toMatch(/^[A-Za-z0-9._~-]{8,128}$/);
    expect(await request!.json()).toEqual({ expected_binding_version: 2, writers_stopped: true });
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
});
