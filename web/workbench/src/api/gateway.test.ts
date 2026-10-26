import { afterEach, describe, expect, it, vi } from 'vitest';
import { ApiError } from './client';
import { gatewayApi } from './gateway';

describe('Gateway API client', () => {
  afterEach(() => vi.unstubAllGlobals());

  it('uses typed Gateway paths with request IDs and Cookie credentials', async () => {
    const requests: Request[] = [];
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      requests.push(new Request(input, init));
      return Response.json({ id: 'h1234567', name: '本地测试主机', state: 'running', expires_at: null });
    }));

    expect((await gatewayApi.host()).id).toBe('h1234567');
    expect((await gatewayApi.renew()).expires_at).toBeNull();
    expect(requests.map(({ method, url }) => [method, new URL(url).pathname])).toEqual([
      ['GET', '/__ch/api/host'],
      ['POST', '/__ch/api/renew'],
    ]);
    for (const request of requests) {
      expect(request.headers.get('X-Request-Id')).toBeTruthy();
      expect(request.credentials).toBe('include');
    }
  });

  it('reads control state and accepts a pending 202 handoff response', async () => {
    const requests: Request[] = [];
    const status = {
      host_id: 'h1234567',
      control_state: 'taking_over',
      control_version: 1,
      active_transition: {
        request_id: 'handoff-1',
        action: 'take_over',
        phase: 'draining',
        failure_reason: '',
        expected_control_version: 0,
      },
    };
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const request = new Request(input, init);
      requests.push(request);
      return request.method === 'GET'
        ? Response.json(status)
        : Response.json({ ...status, pending_operation_ids: ['operation-1'] }, { status: 202 });
    }));

    expect((await gatewayApi.control()).control_state).toBe('taking_over');
    const request = { action: 'take_over' as const, request_id: 'handoff-1', expected_control_version: 0 };
    expect((await gatewayApi.controlAction(request)).pending_operation_ids).toEqual(['operation-1']);
    expect(requests.map(({ method, url }) => [method, new URL(url).pathname])).toEqual([
      ['GET', '/__ch/api/control'],
      ['POST', '/__ch/api/control/actions'],
    ]);
    expect(await requests[1]!.clone().json()).toEqual(request);
  });

  it('reads the host Codex installation and login readiness', async () => {
    const requests: Request[] = [];
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      requests.push(new Request(input, init));
      return Response.json({
        host_id: 'h1234567',
        runtime_generation: 2,
        runtime_instance_id: 'runtime-2',
        control_version: 4,
        checked_at: '2026-10-24T00:00:00Z',
        items: [
          { id: 'codex', installed: true, version: '1.2.3', login_status: 'not_logged_in', installable: false, install_operation: null },
          { id: 'wechat', installed: false, version: '', login_status: 'not_applicable', installable: true, install_operation: null },
        ],
      });
    }));

    expect((await gatewayApi.applications()).items[0]).toMatchObject({
      id: 'codex', installed: true, version: '1.2.3', login_status: 'not_logged_in', installable: false, install_operation: null,
    });
    expect(requests).toHaveLength(1);
    expect([requests[0]!.method, new URL(requests[0]!.url).pathname]).toEqual(['GET', '/__ch/api/applications']);
    expect(requests[0]!.headers.get('X-Request-Id')).toBeTruthy();
    expect(requests[0]!.credentials).toBe('include');
  });

  it('submits a WeChat install with the caller request ID and accepts 202', async () => {
    const requests: Request[] = [];
    const operation = {
      id: 'install-1',
      application_id: 'wechat',
      status: 'accepted',
      progress_percent: 0,
      error_code: '',
      created_at: '2026-10-24T00:00:00Z',
      updated_at: '2026-10-24T00:00:00Z',
    };
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const request = new Request(input, init);
      requests.push(request);
      return Response.json({
        host_id: 'h1234567',
        runtime_generation: 2,
        runtime_instance_id: 'runtime-2',
        control_version: 4,
        operation,
      }, { status: 202 });
    }));

    expect(await gatewayApi.installApplication('wechat', { request_id: 'install-request-1' })).toMatchObject({ operation });
    expect([requests[0]!.method, new URL(requests[0]!.url).pathname]).toEqual([
      'POST', '/__ch/api/applications/wechat/install',
    ]);
    expect(await requests[0]!.clone().json()).toEqual({ request_id: 'install-request-1' });
    expect(requests[0]!.headers.get('X-Request-Id')).toBeTruthy();
    expect(requests[0]!.credentials).toBe('include');
  });

  it('uses the generic per-application launch contract for status, launch, and stop', async () => {
    const requests: Request[] = [];
    const operation = {
      id: 'launch_00000000-0000-4000-8000-000000000001',
      application_id: 'wechat',
      status: 'starting',
      error_code: '',
      created_at: '2026-10-24T00:00:00Z',
      updated_at: '2026-10-24T00:00:00Z',
    };
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const request = new Request(input, init);
      requests.push(request);
      return Response.json({
        host_id: 'h1234567',
        runtime_generation: 2,
        runtime_instance_id: 'runtime-2',
        control_version: 4,
        operation,
      }, { status: request.method === 'GET' ? 200 : 202 });
    }));

    await gatewayApi.currentApplicationLaunch('wechat');
    await gatewayApi.launchApplication('wechat', { launch_id: operation.id });
    await gatewayApi.applicationLaunch('wechat', operation.id);
    await gatewayApi.stopApplicationLaunch('wechat', operation.id);

    expect(requests.map(({ method, url }) => [method, new URL(url).pathname])).toEqual([
      ['GET', '/__ch/api/applications/wechat/launches/current'],
      ['POST', '/__ch/api/applications/wechat/launch'],
      ['GET', `/__ch/api/applications/wechat/launches/${operation.id}`],
      ['POST', `/__ch/api/applications/wechat/launches/${operation.id}/stop`],
    ]);
    expect(await requests[1]!.clone().json()).toEqual({ launch_id: operation.id });
    for (const request of requests) {
      expect(request.headers.get('X-Request-Id')).toBeTruthy();
      expect(request.credentials).toBe('include');
    }
  });

  it('reuses the registered terminal and only requests replacement after confirmation', async () => {
    const requests: Request[] = [];
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const request = new Request(input, init);
      requests.push(request);
      return Response.json({ state: 'ready', url: '/terminal?session_id=registered' });
    }));

    expect(await gatewayApi.ensureTerminalSession()).toEqual({ state: 'ready', url: '/terminal?session_id=registered' });
    expect(await gatewayApi.newTerminalSession()).toEqual({ state: 'ready', url: '/terminal?session_id=registered' });
    expect(requests.map(({ method, url }) => [method, new URL(url).pathname])).toEqual([
      ['PUT', '/__ch/api/terminal-session'],
      ['POST', '/__ch/api/terminal-session'],
    ]);
    expect(await requests[1]!.clone().json()).toEqual({ confirm_new: true });
    for (const request of requests) expect(request.credentials).toBe('include');
  });

  it('preserves API error codes, details, status, and Retry-After', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => Response.json({
      error: { code: 'rate_limited', message: '请稍后重试', details: { retry_after: 7 } },
    }, { status: 429, headers: { 'Retry-After': '7' } })));

    await expect(gatewayApi.renew()).rejects.toMatchObject<ApiError>({
      name: 'ApiError',
      code: 'rate_limited',
      status: 429,
      retryAfter: 7,
      details: { retry_after: 7 },
      message: '请稍后重试',
    });
  });
});
