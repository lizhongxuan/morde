import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, cleanup, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { portalApi } from '../app/api';
import { ToastProvider } from '../app/toast';
import { NewHost } from './NewHost';

vi.mock('../app/auth', () => ({ useAuth: () => ({ user: { id: 'u1234567' } }) }));

describe('NewHost workspace loading', () => {
  afterEach(() => { cleanup(); localStorage.clear(); vi.restoreAllMocks(); });

  it('retries workspace loading and submits selected workspace IDs', async () => {
    let rejectInitial!: (error: Error) => void;
    const initialRequest = new Promise<Awaited<ReturnType<typeof portalApi.workspaces>>>((_, reject) => { rejectInitial = reject; });
    const workspaces = vi.spyOn(portalApi, 'workspaces')
      .mockReturnValueOnce(initialRequest)
      .mockResolvedValue({ items: [{
        id: 'w1234567', name: '待挂载目录', revision: 3, used_bytes: 2048, quota_bytes: 1024 * 1024, state: 'ready', created_at: '', updated_at: '',
      }] });
    vi.spyOn(portalApi, 'templates').mockResolvedValue({ items: [{
      id: 'std', name: '标准规格', description: '', isolation: 'container',
      resources: { cpu_milli: 500, mem_mib: 1024, cpu_limit_milli: 1000, mem_limit_mib: 2048 },
      workspace_size_mib: 1024, ttl_seconds: 3600, idle_timeout_seconds: 0, on_idle: 'disabled', keep_workspace_default: true,
    }] });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const createHost = vi.spyOn(portalApi, 'createHost').mockResolvedValue({
      id: 'h1234567', name: '测试主机', owner_id: 'u1234567', template_id: 'std', isolation: 'container', state: 'creating', state_reason: '', runtime_generation: 1, control_version: 0, control_state: 'ai', url: '', workspaces: [],
      resources: { cpu_milli: 500, mem_mib: 1024, cpu_limit_milli: 1000, mem_limit_mib: 2048 }, expires_at: null, last_active_at: null, created_at: '',
    });
    const user = userEvent.setup();

    render(<QueryClientProvider client={client}><ToastProvider><MemoryRouter><NewHost /></MemoryRouter></ToastProvider></QueryClientProvider>);

    expect(await screen.findByRole('status')).toHaveTextContent('正在加载工作目录');
    expect(screen.queryByText('目前没有可用的工作区。')).not.toBeInTheDocument();
    await act(async () => { rejectInitial(new Error('工作目录服务不可用')); });
    expect(await screen.findByRole('alert')).toHaveTextContent('工作目录服务不可用');
    expect(screen.queryByText('目前没有可挂载的工作目录。')).not.toBeInTheDocument();
    const existingWorkspace = screen.getByRole('radio', { name: '绑定已有工作区' });
    expect(existingWorkspace).toBeDisabled();

    await user.click(screen.getByRole('button', { name: '重试' }));
    await waitFor(() => expect(existingWorkspace).toBeEnabled());
    await user.click(existingWorkspace);

    const selectedWorkspace = screen.getByRole('checkbox', { name: /待挂载目录/ });
    await user.click(selectedWorkspace);
    await user.click(screen.getByRole('button', { name: '创建主机' }));
    await waitFor(() => expect(createHost).toHaveBeenCalledWith({ template_id: 'std', workspace_ids: ['w1234567'] }, expect.stringMatching(/^[A-Za-z0-9._~-]{8,128}$/)));
    expect(workspaces).toHaveBeenCalledTimes(3);
  });

  it('shows the empty workspace message only after a successful empty response', async () => {
    vi.spyOn(portalApi, 'workspaces').mockResolvedValue({ items: [] });
    vi.spyOn(portalApi, 'templates').mockResolvedValue({ items: [{
      id: 'std', name: '标准规格', description: '', isolation: 'container',
      resources: { cpu_milli: 500, mem_mib: 1024, cpu_limit_milli: 1000, mem_limit_mib: 2048 },
      workspace_size_mib: 1024, ttl_seconds: 3600, idle_timeout_seconds: 0, on_idle: 'disabled', keep_workspace_default: true,
    }] });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });

    render(<QueryClientProvider client={client}><ToastProvider><MemoryRouter><NewHost /></MemoryRouter></ToastProvider></QueryClientProvider>);

    await userEvent.setup().click(await screen.findByRole('radio', { name: '绑定已有工作区' }));
    expect(await screen.findByText('目前没有可用的工作区。')).toBeInTheDocument();
  });

  it('creates an independent workspace before creating a host', async () => {
    vi.spyOn(portalApi, 'workspaces').mockResolvedValue({ items: [] });
    vi.spyOn(portalApi, 'templates').mockResolvedValue({ items: [{
      id: 'std', name: '标准规格', description: '', isolation: 'container',
      resources: { cpu_milli: 500, mem_mib: 1024, cpu_limit_milli: 1000, mem_limit_mib: 2048 },
      workspace_size_mib: 1024, ttl_seconds: 3600, idle_timeout_seconds: 0, on_idle: 'disabled', keep_workspace_default: true,
    }] });
    const createWorkspace = vi.spyOn(portalApi, 'createWorkspace').mockResolvedValue({ id: 'w7654321', name: '新工作区', revision: 0, used_bytes: 0, quota_bytes: 1024, state: 'ready', created_at: '', updated_at: '' });
    const createHost = vi.spyOn(portalApi, 'createHost').mockResolvedValue({
      id: 'h1234567', name: '测试主机', owner_id: 'u1234567', template_id: 'std', isolation: 'container', state: 'creating', state_reason: '', runtime_generation: 1, control_version: 0, control_state: 'ai', url: '', workspaces: [],
      resources: { cpu_milli: 500, mem_mib: 1024, cpu_limit_milli: 1000, mem_limit_mib: 2048 }, expires_at: null, last_active_at: null, created_at: '',
    });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const user = userEvent.setup();

    render(<QueryClientProvider client={client}><ToastProvider><MemoryRouter><NewHost /></MemoryRouter></ToastProvider></QueryClientProvider>);

    await user.click(await screen.findByRole('button', { name: '创建主机' }));
    await waitFor(() => expect(createWorkspace).toHaveBeenCalledWith({ name: '项目工作区' }, expect.any(String)));
    expect(createHost).toHaveBeenCalledWith({ template_id: 'std', workspace_ids: ['w7654321'] }, expect.stringMatching(/^[A-Za-z0-9._~-]{8,128}$/));
  });

  it('reuses accepted workspace and host creations after their responses are lost', async () => {
    vi.spyOn(portalApi, 'workspaces').mockResolvedValue({ items: [] });
    vi.spyOn(portalApi, 'templates').mockResolvedValue({ items: [{
      id: 'std', name: '标准规格', description: '', isolation: 'container',
      resources: { cpu_milli: 500, mem_mib: 1024, cpu_limit_milli: 1000, mem_limit_mib: 2048 },
      workspace_size_mib: 1024, ttl_seconds: 3600, idle_timeout_seconds: 0, on_idle: 'disabled', keep_workspace_default: true,
    }] });
    const accepted = new Map<string, Awaited<ReturnType<typeof portalApi.createWorkspace>>>();
    let workspaceAttempts = 0;
    const createWorkspace = vi.spyOn(portalApi, 'createWorkspace').mockImplementation(async (_body, key) => {
      const idempotencyKey = key!;
      let result = accepted.get(idempotencyKey);
      if (!result) {
        result = { id: 'w7654321', name: '项目工作区', revision: 0, used_bytes: 0, quota_bytes: 1024, state: 'ready', created_at: '', updated_at: '' };
        accepted.set(idempotencyKey, result);
      }
      if (workspaceAttempts++ === 0) throw new TypeError('服务器已创建工作区，但响应丢失');
      return result;
    });
    const acceptedHost: Awaited<ReturnType<typeof portalApi.createHost>> = {
        id: 'h1234567', name: '测试主机', owner_id: 'u1234567', template_id: 'std', isolation: 'container', state: 'creating', state_reason: '', runtime_generation: 1, control_version: 0, control_state: 'ai', url: '', workspaces: [],
        resources: { cpu_milli: 500, mem_mib: 1024, cpu_limit_milli: 1000, mem_limit_mib: 2048 }, expires_at: null, last_active_at: null, created_at: '',
    };
    const hostKeys: string[] = [];
    let hostAttempts = 0;
    const createHost = vi.spyOn(portalApi, 'createHost').mockImplementation(async (_body, key) => {
      hostKeys.push(key!);
      if (hostAttempts++ === 0) throw new TypeError('主机已创建，但响应丢失');
      return acceptedHost;
    });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const user = userEvent.setup();

    render(<QueryClientProvider client={client}><ToastProvider><MemoryRouter><NewHost /></MemoryRouter></ToastProvider></QueryClientProvider>);

    const createButton = await screen.findByRole('button', { name: '创建主机' });
    await user.click(createButton);
    expect(await screen.findByText('服务器已创建工作区，但响应丢失')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: '继续上次创建' }));
    expect(await screen.findByText('主机已创建，但响应丢失')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: '继续上次创建' }));

    await waitFor(() => expect(createHost).toHaveBeenLastCalledWith({ template_id: 'std', workspace_ids: ['w7654321'] }, expect.any(String)));
    expect(createWorkspace).toHaveBeenCalledTimes(2);
    expect(createWorkspace.mock.calls[1]?.[1]).toBe(createWorkspace.mock.calls[0]?.[1]);
    expect(createHost).toHaveBeenCalledTimes(2);
    expect(hostKeys[1]).toBe(hostKeys[0]);
    expect(accepted.size).toBe(1);
  });

  it('restores the same create request after leaving and reopening the page', async () => {
    vi.spyOn(portalApi, 'workspaces').mockResolvedValue({ items: [] });
    vi.spyOn(portalApi, 'templates').mockResolvedValue({ items: [{
      id: 'std', name: '标准规格', description: '', isolation: 'container',
      resources: { cpu_milli: 500, mem_mib: 1024, cpu_limit_milli: 1000, mem_limit_mib: 2048 },
      workspace_size_mib: 1024, ttl_seconds: 3600, idle_timeout_seconds: 0, on_idle: 'disabled', keep_workspace_default: true,
    }] });
    const createWorkspace = vi.spyOn(portalApi, 'createWorkspace').mockResolvedValue({ id: 'w7654321', name: '项目工作区', revision: 0, used_bytes: 0, quota_bytes: 1024, state: 'ready', created_at: '', updated_at: '' });
    const createHost = vi.spyOn(portalApi, 'createHost')
      .mockRejectedValueOnce(new TypeError('响应丢失'))
      .mockResolvedValue({
        id: 'h1234567', name: '测试主机', owner_id: 'u1234567', template_id: 'std', isolation: 'container', state: 'creating', state_reason: '', runtime_generation: 1, control_version: 0, control_state: 'ai', url: '', workspaces: [],
        resources: { cpu_milli: 500, mem_mib: 1024, cpu_limit_milli: 1000, mem_limit_mib: 2048 }, expires_at: null, last_active_at: null, created_at: '',
      });
    const renderPage = () => {
      const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
      return render(<QueryClientProvider client={client}><ToastProvider><MemoryRouter><NewHost /></MemoryRouter></ToastProvider></QueryClientProvider>);
    };
    const user = userEvent.setup();

    const firstPage = renderPage();
    await user.click(await screen.findByRole('button', { name: '创建主机' }));
    expect(await screen.findByText('响应丢失')).toBeInTheDocument();
    const record = JSON.parse(localStorage.getItem('morde.pending-host-create.u1234567') ?? 'null') as { workspaceID: string; workspaceIdempotencyKey: string; hostIdempotencyKey: string };
    expect(record).toMatchObject({ workspaceID: 'w7654321' });
    firstPage.unmount();

    renderPage();
    expect(await screen.findByRole('status')).toHaveTextContent('上次创建结果尚未确认');
    await user.click(screen.getByRole('button', { name: '继续上次创建' }));
    await waitFor(() => expect(createHost).toHaveBeenCalledTimes(2));
    expect(createWorkspace).toHaveBeenCalledTimes(1);
    expect(createHost.mock.calls[1]?.[1]).toBe(record.hostIdempotencyKey);
    expect(localStorage.getItem('morde.pending-host-create.u1234567')).toBeNull();
  });

  it('blocks a new host when the saved recovery record is invalid until the user verifies the host list', async () => {
    localStorage.setItem('morde.pending-host-create.u1234567', '{"version":0}');
    vi.spyOn(portalApi, 'workspaces').mockResolvedValue({ items: [] });
    vi.spyOn(portalApi, 'templates').mockResolvedValue({ items: [{
      id: 'std', name: '标准规格', description: '', isolation: 'container',
      resources: { cpu_milli: 500, mem_mib: 1024, cpu_limit_milli: 1000, mem_limit_mib: 2048 },
      workspace_size_mib: 1024, ttl_seconds: 3600, idle_timeout_seconds: 0, on_idle: 'disabled', keep_workspace_default: true,
    }] });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const user = userEvent.setup();

    render(<QueryClientProvider client={client}><ToastProvider><MemoryRouter><NewHost /></MemoryRouter></ToastProvider></QueryClientProvider>);

    expect(await screen.findByRole('alert')).toHaveTextContent('无法读取本机上次创建记录');
    const submit = screen.getByRole('button', { name: '创建主机' });
    expect(submit).toBeDisabled();
    await user.click(screen.getByRole('button', { name: '我已核实，清除记录' }));
    expect(localStorage.getItem('morde.pending-host-create.u1234567')).toBeNull();
    expect(submit).toBeEnabled();
  });
});
