import { act, fireEvent, render, screen } from '@testing-library/react';
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
  afterEach(() => vi.useRealTimers());

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
        active_transition: null,
      }}
      controlActionLabel="接管电脑"
      onControlAction={onControlAction}
    />);

    expect(container.querySelector('.control-state')).toHaveTextContent('当前控制者：AI · 桌面只读');
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
        active_transition: {
          request_id: 'handoff-1',
          action: 'take_over',
          phase: 'draining',
          expected_control_version: 0,
        },
      }}
    />);

    expect(container.querySelector('.control-state')).toHaveTextContent('正在接管电脑，请稍候…');
    expect(screen.queryByText('当前控制者：你')).not.toBeInTheDocument();
  });
});
