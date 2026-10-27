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
  active_transition: null,
};

function mockWorkbenchApi() {
  vi.spyOn(gatewayApi, 'host').mockResolvedValue(host);
  vi.spyOn(gatewayApi, 'control').mockResolvedValue(control);
}

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
    expect(screen.getByRole('button', { name: '文件' })).toBeVisible();
    expect(screen.getByRole('button', { name: 'VS Code' })).toBeDisabled();
    fireEvent.click(screen.getByRole('button', { name: '文件' }));
    expect(screen.getByText('请先接管电脑后使用文件。')).toBeVisible();
    expect(screen.queryByLabelText('当前路径')).not.toBeInTheDocument();
    expect(screen.queryByText(/Codex|微信|QQ|飞书/)).not.toBeInTheDocument();
    await waitFor(() => expect(gatewayApi.host).toHaveBeenCalledOnce());
    appApi.forEach((call) => expect(call).not.toHaveBeenCalled());
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
});

describe('Workbench terminal sessions', () => {
  beforeEach(mockWorkbenchApi);
  afterEach(() => {
    cleanup();
    vi.restoreAllMocks();
    vi.useRealTimers();
  });

  it('reuses the registered session and confirms before replacing it', async () => {
    vi.spyOn(gatewayApi, 'control').mockResolvedValue({ ...control, control_state: 'user' });
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
    vi.spyOn(gatewayApi, 'control').mockResolvedValue({ ...control, control_state: 'user' });
    vi.spyOn(gatewayApi, 'ensureTerminalSession').mockResolvedValue({ state: 'unknown', url: '' });
    const createNew = vi.spyOn(gatewayApi, 'newTerminalSession');
    vi.spyOn(window, 'confirm').mockReturnValue(false);
    render(<Workbench />);

    fireEvent.click(screen.getByRole('button', { name: '终端' }));
    expect(await screen.findByText(/原终端的创建结果不确定/)).toBeInTheDocument();
    expect(createNew).not.toHaveBeenCalled();
  });

  it('explains an interrupted PTY may be unavailable and does not replace it automatically', async () => {
    vi.spyOn(gatewayApi, 'control').mockResolvedValue({ ...control, control_state: 'user' });
    vi.spyOn(gatewayApi, 'ensureTerminalSession').mockResolvedValue({ state: 'interrupted', url: '' });
    const createNew = vi.spyOn(gatewayApi, 'newTerminalSession');
    render(<Workbench />);

    fireEvent.click(screen.getByRole('button', { name: '终端' }));
    expect(await screen.findByText(/原终端会话已中断或无法恢复/)).toBeInTheDocument();
    expect(createNew).not.toHaveBeenCalled();
  });

  it('waits for an in-flight terminal creation before connecting', async () => {
    vi.useFakeTimers();
    vi.spyOn(gatewayApi, 'control').mockResolvedValue({ ...control, control_state: 'user' });
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
