import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ApiError } from '../api/client';
import { gatewayApi, type GatewayControl, type GatewayHost } from '../api/gateway';
import Workbench from './Workbench';

const host: GatewayHost = {
  id: 'h1234567',
  name: '测试主机',
  portal_origin: window.location.origin,
  state: 'running',
  expires_at: null,
  workspaces: [],
};
const control: GatewayControl = {
  host_id: host.id,
  control_state: 'ai',
  control_version: 0,
  control_actor_user_id: '',
  control_lease_expires_at: null,
  control_lease_active: false,
  control_lease_held_by_me: false,
  active_transition: null,
};
const userControl: GatewayControl = {
  ...control,
  control_state: 'user',
  control_version: 1,
  control_actor_user_id: 'u1234567',
  control_lease_expires_at: '2026-11-02T00:00:30.000Z',
  control_lease_active: true,
  control_lease_held_by_me: true,
};

function mockWorkbenchApi() {
  vi.spyOn(gatewayApi, 'host').mockResolvedValue(host);
  vi.spyOn(gatewayApi, 'control').mockResolvedValue(control);
  vi.spyOn(gatewayApi, 'workspaces').mockResolvedValue({ items: [] });
}

afterEach(() => window.history.replaceState({}, '', '/'));

describe('Workbench MVP navigation', () => {
  beforeEach(mockWorkbenchApi);
  afterEach(() => {
    cleanup();
    vi.restoreAllMocks();
    vi.useRealTimers();
    window.history.replaceState({}, '', '/');
  });

  it('shows only the supported Host tools and does not query or operate desktop apps', async () => {
    const appApi = [
      vi.spyOn(gatewayApi, 'applications'),
      vi.spyOn(gatewayApi, 'currentApplicationLaunch'),
      vi.spyOn(gatewayApi, 'applicationLaunch'),
      vi.spyOn(gatewayApi, 'installApplication'),
      vi.spyOn(gatewayApi, 'launchApplication'),
      vi.spyOn(gatewayApi, 'stopApplicationLaunch'),
    ];
    render(<Workbench />);

    expect(await screen.findByRole('button', { name: '桌面' })).toBeVisible();
    expect(screen.getByRole('button', { name: '终端' })).toBeVisible();
    expect(screen.getByRole('button', { name: 'Workspace' })).toBeVisible();
    expect(screen.getByRole('button', { name: '主机文件' })).toBeVisible();
    expect(screen.getByRole('button', { name: 'VS Code' })).toBeDisabled();
    fireEvent.click(screen.getByRole('button', { name: '主机文件' }));
    expect(screen.getByText('请先接管电脑后使用文件。')).toBeVisible();
    expect(screen.queryByLabelText('当前路径')).not.toBeInTheDocument();
    expect(screen.queryByText(/Codex|微信|QQ|飞书/)).not.toBeInTheDocument();
    await waitFor(() => expect(gatewayApi.host).toHaveBeenCalledOnce());
    fireEvent.click(screen.getByRole('button', { name: 'Workspace' }));
    expect(await screen.findByText('此电脑没有可访问的工作区。')).toBeVisible();
    appApi.forEach((call) => expect(call).not.toHaveBeenCalled());
  });

  it('opens a Workspace-only session without querying owner-only Host or control APIs', async () => {
    window.history.replaceState({}, '', '/__ch/wb/?tab=workspace');
    render(<Workbench />);

    expect(await screen.findByRole('button', { name: 'Workspace' })).toBeVisible();
    expect(screen.queryByRole('button', { name: '桌面' })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: '终端' })).not.toBeInTheDocument();
    expect(await screen.findByText('此电脑没有可访问的工作区。')).toBeVisible();
    expect(gatewayApi.host).not.toHaveBeenCalled();
    expect(gatewayApi.control).not.toHaveBeenCalled();
    expect(gatewayApi.workspaces).toHaveBeenCalledOnce();
  });

  it('renders Task observation without loading Host controls or Workspace APIs', async () => {
    const taskID = 't1234567';
    const taskObservation = vi.spyOn(gatewayApi, 'taskObservation').mockResolvedValue({
      task_id: taskID, host_id: host.id, status: 'running', event_sequence: 8, updated_at: '2026-11-03T12:00:00Z',
      artifacts: [{ id: 'a1234567', label: 'report', media_type: 'text/plain', workspace_id: 'w1234567', revision: 2,
        size_bytes: 15, sha256: 'a'.repeat(64), created_at: '2026-11-03T12:00:00Z' }],
    });
    const eventSources: Array<{ url: string; close: ReturnType<typeof vi.fn> }> = [];
    vi.stubGlobal('EventSource', class extends EventTarget {
      url: string;
      close = vi.fn();
      constructor(url: string) { super(); this.url = url; eventSources.push(this); }
    });
    window.history.replaceState({}, '', `/__ch/wb/?task_id=${taskID}`);
    render(<Workbench />);

    expect(await screen.findByText(`Task ${taskID}`)).toBeVisible();
    expect(screen.getByText('report')).toBeVisible();
    await waitFor(() => expect(eventSources).toHaveLength(1));
    expect(new URL(eventSources[0]!.url).searchParams.get('after')).toBe('8');
    expect(taskObservation).toHaveBeenCalledWith(taskID);
    expect(gatewayApi.host).not.toHaveBeenCalled();
    expect(gatewayApi.control).not.toHaveBeenCalled();
    expect(gatewayApi.workspaces).not.toHaveBeenCalled();
    expect(screen.queryByRole('button', { name: /暂停|继续|停止任务|接管电脑/ })).not.toBeInTheDocument();
    expect(screen.queryByRole('link', { name: /下载|打开/ })).not.toBeInTheDocument();
  });
});

describe('Workbench control conflicts', () => {
  beforeEach(mockWorkbenchApi);
  afterEach(() => {
    cleanup();
    vi.restoreAllMocks();
    vi.useRealTimers();
  });

  it('drops a stale takeover request after a 409 and retries with the refreshed version', async () => {
    const refreshed = { ...control, control_version: 1 };
    vi.spyOn(gatewayApi, 'control').mockResolvedValueOnce(control).mockResolvedValueOnce(refreshed).mockResolvedValue(refreshed);
    const action = vi.spyOn(gatewayApi, 'controlAction')
      .mockRejectedValueOnce(new ApiError({ message: '控制状态已变化，请刷新后重试', code: 'conflict', status: 409 }))
      .mockResolvedValue({ ...refreshed, control_state: 'taking_over', control_version: 2 });
    render(<Workbench />);

    fireEvent.click(await screen.findByRole('button', { name: '接管电脑' }));
    await waitFor(() => expect(screen.getByRole('button', { name: '接管电脑' })).toBeVisible());
    fireEvent.click(screen.getByRole('button', { name: '接管电脑' }));
    await waitFor(() => expect(action).toHaveBeenCalledTimes(2));
    expect(action.mock.calls.map(([request]) => request.expected_control_version)).toEqual([0, 1]);
  });

  it('fails closed when a control refresh fails after the user took over', async () => {
    vi.useFakeTimers();
    vi.spyOn(gatewayApi, 'control')
      .mockResolvedValueOnce(userControl)
      .mockRejectedValueOnce(new Error('connection lost'))
      .mockResolvedValue(userControl);
    render(<Workbench />);

    await act(async () => { await Promise.resolve(); await Promise.resolve(); });
    expect(screen.getByText('你持有电脑操作权')).toBeVisible();
    expect(screen.getByTitle('远程桌面').getAttribute('src')).toContain('view_only=false');

    await act(async () => { await vi.advanceTimersByTimeAsync(3000); });
    expect(screen.getByText('控制状态暂时不可用')).toBeVisible();
    expect(screen.getByRole('button', { name: '重试读取' })).toBeVisible();
    expect(screen.queryByRole('button', { name: '交回 AI' })).not.toBeInTheDocument();
    expect(screen.getByTitle('远程桌面').getAttribute('src')).toContain('view_only=true');

    fireEvent.click(screen.getByRole('button', { name: '主机文件' }));
    expect(screen.getByRole('alert')).toHaveTextContent('控制状态暂不可用：connection lost');
    expect(screen.queryByLabelText('当前路径')).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole('button', { name: '重试读取' }));
    await act(async () => { await Promise.resolve(); await Promise.resolve(); });
    expect(screen.getByText('你持有电脑操作权')).toBeVisible();
    expect(screen.getByRole('button', { name: '交回 AI' })).toBeVisible();
    expect(screen.getByTitle('远程桌面').getAttribute('src')).toContain('view_only=false');
  });

  it('keeps an observer-only session read-only when the service denies control access', async () => {
    vi.spyOn(gatewayApi, 'control').mockRejectedValue(new ApiError({
      message: '当前账号只有桌面观察权限', code: 'forbidden', status: 403,
    }));
    const controlAction = vi.spyOn(gatewayApi, 'controlAction');
    render(<Workbench />);

    expect(await screen.findByText('控制状态暂时不可用')).toBeVisible();
    expect(screen.getByTitle('远程桌面').getAttribute('src')).toContain('view_only=true');
    expect(screen.queryByRole('button', { name: '接管电脑' })).not.toBeInTheDocument();
    expect(controlAction).not.toHaveBeenCalled();
  });

  it('keeps desktop, terminal, and VS Code read-only when another member holds the lease', async () => {
    vi.spyOn(gatewayApi, 'control').mockResolvedValue({
      ...userControl, control_actor_user_id: 'u8765432', control_lease_held_by_me: false,
    });
    const controlAction = vi.spyOn(gatewayApi, 'controlAction');
    const terminal = vi.spyOn(gatewayApi, 'ensureTerminalSession');
    render(<Workbench />);

    expect(await screen.findByText('当前操作账号：u8765432')).toBeVisible();
    expect(screen.getByTitle('远程桌面').getAttribute('src')).toContain('view_only=true');
    fireEvent.click(screen.getByRole('button', { name: '终端' }));
    expect(screen.getByText('请先接管电脑后使用终端。')).toBeVisible();
    expect(screen.getByRole('button', { name: 'VS Code' })).toBeDisabled();
    expect(screen.queryByRole('button', { name: '交回 AI' })).not.toBeInTheDocument();
    expect(controlAction).not.toHaveBeenCalled();
    expect(terminal).not.toHaveBeenCalled();
  });

  it('renews the current user lease while the Workbench stays open', async () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date('2026-11-02T00:00:00.000Z'));
    vi.spyOn(gatewayApi, 'control').mockResolvedValue(userControl);
    const renewed = { ...userControl, control_lease_expires_at: '2026-11-02T00:00:40.000Z' };
    const renewLease = vi.spyOn(gatewayApi, 'renewControlLease').mockResolvedValue(renewed);
    render(<Workbench />);

    await act(async () => { await Promise.resolve(); await Promise.resolve(); });
    await act(async () => { await vi.advanceTimersByTimeAsync(10_000); });

    expect(renewLease).toHaveBeenCalledWith(userControl.control_version);
    expect(screen.getByText('你持有电脑操作权')).toBeVisible();
  });
});

describe('Workbench terminal sessions', () => {
  beforeEach(mockWorkbenchApi);
  afterEach(() => {
    cleanup();
    vi.restoreAllMocks();
    vi.useRealTimers();
  });

  it('reuses the registered session and confirms before replacing it', async () => {
    vi.spyOn(gatewayApi, 'control').mockResolvedValue(userControl);
    const ensure = vi.spyOn(gatewayApi, 'ensureTerminalSession').mockResolvedValue({
      state: 'ready', url: '/terminal?session_id=registered',
    });
    const createNew = vi.spyOn(gatewayApi, 'newTerminalSession').mockResolvedValue({
      state: 'ready', url: '/terminal?session_id=replacement',
    });
    const confirm = vi.spyOn(window, 'confirm').mockReturnValue(false);
    render(<Workbench />);

    fireEvent.click(screen.getByRole('button', { name: '终端' }));
    const frame = await screen.findByTitle('主机终端');
    expect(frame.getAttribute('src')).toBe('/terminal?session_id=registered');
    expect(ensure).toHaveBeenCalledTimes(1);

    fireEvent.click(screen.getByRole('button', { name: '新建终端' }));
    expect(confirm).toHaveBeenCalledWith(expect.stringContaining('原终端中的命令'));
    expect(createNew).not.toHaveBeenCalled();

    confirm.mockReturnValue(true);
    fireEvent.click(screen.getByRole('button', { name: '新建终端' }));
    await waitFor(() => expect(screen.getByTitle('主机终端').getAttribute('src')).toBe('/terminal?session_id=replacement'));
    expect(createNew).toHaveBeenCalledTimes(1);
  });

  it('does not create another PTY automatically when the prior result is unknown', async () => {
    vi.spyOn(gatewayApi, 'control').mockResolvedValue(userControl);
    vi.spyOn(gatewayApi, 'ensureTerminalSession').mockResolvedValue({ state: 'unknown', url: '' });
    const createNew = vi.spyOn(gatewayApi, 'newTerminalSession');
    vi.spyOn(window, 'confirm').mockReturnValue(false);
    render(<Workbench />);

    fireEvent.click(screen.getByRole('button', { name: '终端' }));
    expect(await screen.findByText(/原终端的创建结果不确定/)).toBeInTheDocument();
    expect(createNew).not.toHaveBeenCalled();
  });

  it('explains an interrupted PTY may be unavailable and does not replace it automatically', async () => {
    vi.spyOn(gatewayApi, 'control').mockResolvedValue(userControl);
    vi.spyOn(gatewayApi, 'ensureTerminalSession').mockResolvedValue({ state: 'interrupted', url: '' });
    const createNew = vi.spyOn(gatewayApi, 'newTerminalSession');
    render(<Workbench />);

    fireEvent.click(screen.getByRole('button', { name: '终端' }));
    expect(await screen.findByText(/原终端会话已中断或无法恢复/)).toBeInTheDocument();
    expect(createNew).not.toHaveBeenCalled();
  });

  it('waits for an in-flight terminal creation before connecting', async () => {
    vi.useFakeTimers();
    vi.spyOn(gatewayApi, 'control').mockResolvedValue(userControl);
    const ensure = vi.spyOn(gatewayApi, 'ensureTerminalSession')
      .mockResolvedValueOnce({ state: 'creating', url: '' })
      .mockResolvedValueOnce({ state: 'ready', url: '/terminal?session_id=created' });
    const createNew = vi.spyOn(gatewayApi, 'newTerminalSession');
    render(<Workbench />);

    fireEvent.click(screen.getByRole('button', { name: '终端' }));
    await act(async () => {
      await Promise.resolve();
      await Promise.resolve();
      await Promise.resolve();
    });
    expect(screen.getByText(/终端正在创建/)).toBeInTheDocument();
    expect(createNew).not.toHaveBeenCalled();

    await act(async () => {
      await vi.advanceTimersByTimeAsync(2000);
    });
    expect(screen.getByTitle('主机终端').getAttribute('src')).toBe('/terminal?session_id=created');
    expect(ensure).toHaveBeenCalledTimes(2);
  });
});
