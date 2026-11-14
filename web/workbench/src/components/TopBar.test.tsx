import { act, cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import TopBar from './TopBar';

const controlProps = {
  control: null,
  controlLoading: false,
  controlError: '',
  controlActionError: '',
  controlActionLabel: '',
  controlActionPending: false,
  onControlAction: vi.fn(),
  onRetryControl: vi.fn(),
};

describe('TopBar countdown', () => {
  afterEach(() => {
    cleanup();
    Reflect.deleteProperty(window, 'morde');
    vi.useRealTimers();
  });

  it('provides a safe return-to-computer-list action only in the standalone Host window', () => {
    const returnToPortalHosts = vi.fn(() => Promise.resolve({ returned: true }));
    Object.defineProperty(window, 'morde', { configurable: true, value: { returnToPortalHosts } });
    const props = { host: null, renewing: false, onRenew: vi.fn(), ...controlProps };
    const { rerender } = render(<TopBar {...props} />);

    fireEvent.click(screen.getByRole('button', { name: '返回云端电脑' }));
    expect(returnToPortalHosts).toHaveBeenCalledOnce();

    rerender(<TopBar {...props} embedded />);
    expect(screen.queryByRole('button', { name: '返回云端电脑' })).not.toBeInTheDocument();
  });

  it('shows transient errors in the overlay without widening the header', () => {
    const { container } = render(<TopBar
      host={null}
      renewing={false}
      onRenew={vi.fn()}
      {...controlProps}
      notice="远程桌面连接失败"
      noticeKind="error"
    />);

    const message = screen.getByRole('alert');
    expect(message).toHaveTextContent('远程桌面连接失败');
    expect(message).toHaveClass('workbench-floating-error');
    expect(message.closest('.top-bar')).toBeNull();
    expect(container.querySelector('.top-bar')?.textContent).not.toContain('远程桌面连接失败');
  });

  it('shows success notices in the overlay without widening the header', () => {
    const { container } = render(<TopBar
      host={null}
      renewing={false}
      onRenew={vi.fn()}
      {...controlProps}
      notice="主机已续期"
      noticeKind="success"
    />);

    const message = screen.getByText('主机已续期').closest('[role="status"]');
    if (!message) throw new Error('成功提示不在固定提示层中');
    expect(message).toHaveTextContent('主机已续期');
    expect(message).toHaveClass('workbench-floating-success');
    expect(message.closest('.top-bar')).toBeNull();
    expect(container.querySelector('.top-bar')?.textContent).not.toContain('主机已续期');
    fireEvent.click(screen.getByRole('button', { name: '关闭状态提示' }));
    expect(screen.queryByText('主机已续期')).not.toBeInTheDocument();
  });

  it('updates the remaining time once per second and marks expired hosts', async () => {
    vi.useFakeTimers();
    const now = new Date('2026-10-19T00:00:00.000Z');
    vi.setSystemTime(now);

    render(<TopBar
      host={{ id: 'h1234567', name: '本地测试主机', portal_origin: 'http://127.0.0.1:5183', state: 'running', expires_at: new Date(now.getTime() + 2000).toISOString(), workspaces: [] }}
      renewing={false}
      onRenew={vi.fn()}
      {...controlProps}
    />);

    expect(screen.getByText('剩余 00:00:02')).toBeInTheDocument();
    await act(async () => { await vi.advanceTimersByTimeAsync(1000); });
    expect(screen.getByText('剩余 00:00:01')).toBeInTheDocument();
    await act(async () => { await vi.advanceTimersByTimeAsync(1000); });
    expect(screen.getByText('已到期')).toBeInTheDocument();
  });

  it('shows an unknown expiry for hosts without an expiry time', () => {
    render(<TopBar
      host={{ id: 'h1234567', name: '本地测试主机', portal_origin: 'http://127.0.0.1:5183', state: 'running', expires_at: null, workspaces: [] }}
      renewing={false}
      onRenew={vi.fn()}
      {...controlProps}
    />);

    expect(screen.getByText('到期时间未知')).toBeInTheDocument();
  });

  it('shows the AI read-only state and exposes its handoff action', () => {
    const onControlAction = vi.fn();
    const { container } = render(<TopBar
      host={null}
      renewing={false}
      onRenew={vi.fn()}
      {...controlProps}
      control={{
        host_id: 'h1234567',
        control_state: 'ai',
        control_version: 0,
        control_actor_user_id: '',
        control_lease_expires_at: null,
        control_lease_active: false,
        control_lease_held_by_me: false,
        active_transition: null,
      }}
      controlActionLabel="接管电脑"
      onControlAction={onControlAction}
    />);

    expect(container.querySelector('.control-state-full')).toHaveTextContent('当前控制者：AI · 桌面只读');
    expect(container.querySelector('.control-state-compact')).toHaveTextContent('AI 控制中 · 只读');
    expect(container.querySelector('.desktop-sharing-notice')).toBeNull();
    const takeOver = screen.getByRole('button', { name: '接管电脑' });
    expect(takeOver).toHaveClass('button');
    expect(takeOver).not.toHaveClass('button-secondary');
    fireEvent.click(takeOver);
    expect(onControlAction).toHaveBeenCalledOnce();
  });

  it('shows waiting for an unconfirmed control transition', () => {
    const { container } = render(<TopBar
      host={null}
      renewing={false}
      onRenew={vi.fn()}
      {...controlProps}
      control={{
        host_id: 'h1234567',
        control_state: 'taking_over',
        control_version: 1,
        control_actor_user_id: '',
        control_lease_expires_at: null,
        control_lease_active: false,
        control_lease_held_by_me: false,
        active_transition: {
          request_id: 'handoff-1',
          action: 'take_over',
          phase: 'draining',
          failure_reason: '',
          expected_control_version: 0,
          blocked_control_version: 1,
          target_control_version: 2,
          created_at: '',
          updated_at: '',
        },
      }}
    />);

    expect(container.querySelector('.control-state-full')).toHaveTextContent('正在接管电脑，请稍候…');
    expect(container.querySelector('.control-state-compact')).toHaveTextContent('接管中');
    expect(screen.queryByText('当前控制者：用户身份未提供')).not.toBeInTheDocument();
  });

  it('shows the other operator and lease countdown when the server returns them', () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date('2026-10-19T00:00:00.000Z'));
    render(<TopBar
      host={null}
      renewing={false}
      onRenew={vi.fn()}
      {...controlProps}
      control={{
        host_id: 'h1234567',
        control_state: 'user',
        control_version: 8,
        control_actor_user_id: 'u7654321',
        control_lease_expires_at: '2026-10-19T00:00:08.000Z',
        control_lease_active: true,
        control_lease_held_by_me: false,
        active_transition: null,
      }}
    />);

    expect(screen.getByText('当前操作账号：u7654321')).toBeInTheDocument();
    expect(screen.getByText('操作租约有效 · 剩余 00:00:08')).toBeInTheDocument();
  });

  it('marks verbose host details and renewal for mobile hiding while keeping control recovery actions', () => {
    const onControlAction = vi.fn();
    const onRetryControl = vi.fn();
    const { container } = render(<TopBar
      host={{ id: 'h1234567', name: '本地测试主机', portal_origin: 'http://127.0.0.1:5183', state: 'running', expires_at: '2026-10-19T01:00:00.000Z', workspaces: [] }}
      renewing={false}
      onRenew={vi.fn()}
      {...controlProps}
      control={{
        host_id: 'h1234567',
        control_state: 'user',
        control_version: 8,
        control_actor_user_id: 'u7654321',
        control_lease_expires_at: '2026-10-19T00:05:00.000Z',
        control_lease_active: true,
        control_lease_held_by_me: true,
        active_transition: null,
      }}
      controlError="Synthetic control read failure"
      controlActionError="Synthetic control action failure"
      controlActionLabel="交还 AI"
      onControlAction={onControlAction}
      onRetryControl={onRetryControl}
    />);

    for (const selector of [
      '.host-expiry',
      '.control-lease',
      '.host-renew-button',
      '.control-state-full',
      '.control-state-compact',
    ]) {
      expect(container.querySelector(selector), `${selector} remains available for the mobile CSS rule`).not.toBeNull();
    }
    expect(container.querySelector('.host-state')).toHaveTextContent('运行中');
    expect(container.querySelector('.control-state-full')).toHaveTextContent('控制状态暂时不可用');
    expect(container.querySelector('.control-state-compact')).toHaveTextContent('控制不可用');
    expect(screen.getByRole('alert')).toHaveTextContent('Synthetic control action failure');

    fireEvent.click(screen.getByRole('button', { name: '重试读取' }));
    fireEvent.click(screen.getByRole('button', { name: '交还 AI' }));
    expect(onRetryControl).toHaveBeenCalledOnce();
    expect(onControlAction).toHaveBeenCalledOnce();
  });

  it('keeps the compact control action while omitting host status labels in the embedded header', () => {
    const { container } = render(<TopBar
      embedded
      host={{ id: 'h1234567', name: '本地测试主机', portal_origin: 'https://portal.example.test', state: 'running', expires_at: '2026-10-19T01:00:00.000Z', workspaces: [] }}
      renewing={false}
      onRenew={vi.fn()}
      {...controlProps}
      control={{ host_id: 'h1234567', control_state: 'ai', control_version: 1, control_actor_user_id: '',
        control_lease_expires_at: null, control_lease_active: false, control_lease_held_by_me: false, active_transition: null }}
      controlActionLabel="接管电脑"
    />);

    expect(container.querySelector('.top-bar')).toHaveClass('top-bar-embedded');
    expect(container.querySelector('.host-name')).toBeNull();
    expect(container.querySelector('.host-state')).toBeNull();
    expect(container.querySelector('.host-renew-button')).toBeNull();
    expect(container.querySelector('.top-actions button')).toHaveTextContent('接管电脑');
  });

  it('marks an expired control lease read-only', () => {
    render(<TopBar
      host={null}
      renewing={false}
      onRenew={vi.fn()}
      {...controlProps}
      control={{ host_id: 'h1234567', control_state: 'user', control_version: 8, control_actor_user_id: '',
        control_lease_expires_at: null, control_lease_active: false, control_lease_held_by_me: false, active_transition: null }}
    />);

    expect(screen.getByText('操作租约已失效 · 桌面只读')).toBeInTheDocument();
    expect(screen.getByText('当前没有有效操作租约')).toBeInTheDocument();
  });
});
