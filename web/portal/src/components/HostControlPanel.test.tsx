import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { portalApi } from '../app/api';
import { ToastNoticeCenter, ToastProvider } from '../app/toast';
import { HostControlPanel } from './HostControlPanel';

vi.mock('../app/auth', () => ({ useAuth: () => ({ user: { id: 'u1234567' } }) }));

const host = { id: 'h1234567', name: '测试电脑', control_state: 'user' as const };
type ControlStatus = Awaited<ReturnType<typeof portalApi.hostControl>>;
const expiredLease = {
  host_id: host.id, control_state: 'user' as const, control_version: 3, control_actor_user_id: '',
  control_lease_expires_at: '2026-11-20T01:00:00.000Z', control_lease_active: false,
  control_lease_held_by_me: false, active_transition: null,
};
const activeLease = {
  ...expiredLease, control_actor_user_id: 'u1234567', control_lease_active: true, control_lease_held_by_me: true,
};

function renderPanel() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(<QueryClientProvider client={client}><ToastProvider><ToastNoticeCenter autoOpenErrors overlay />
    <HostControlPanel host={host} canControl canReleaseAnyLease compact />
  </ToastProvider></QueryClientProvider>);
}

describe('HostControlPanel embedded lease recovery', () => {
  afterEach(() => { cleanup(); vi.restoreAllMocks(); });

  it('renews the current user lease in the compact panel', async () => {
    vi.spyOn(portalApi, 'hostControl').mockResolvedValue(activeLease);
    vi.spyOn(portalApi, 'renewHostControlLease').mockResolvedValue(activeLease);
    const setInterval = vi.spyOn(window, 'setInterval');
    renderPanel();

    expect(await screen.findByRole('button', { name: '归还控制权' })).toBeEnabled();
    expect(setInterval).toHaveBeenCalledWith(expect.any(Function), 10_000);
  });

  it('keeps polling server status while the control action request is pending', async () => {
    const aiStatus: ControlStatus = {
      ...expiredLease, control_state: 'ai', control_version: 2,
    };
    let statusCalls = 0;
    const status = vi.spyOn(portalApi, 'hostControl').mockImplementation(async () => {
      statusCalls += 1;
      return statusCalls === 1 ? aiStatus : activeLease;
    });
    type ControlActionResult = Awaited<ReturnType<typeof portalApi.hostControlAction>>;
    let finishAction: ((result: ControlActionResult) => void) | undefined;
    const action = vi.spyOn(portalApi, 'hostControlAction').mockReturnValue(new Promise((resolve) => {
      finishAction = resolve;
    }));
    renderPanel();

    fireEvent.click(await screen.findByRole('button', { name: '接管控制权' }));
    await waitFor(() => expect(status).toHaveBeenCalledTimes(2), { timeout: 3_000 });
    expect(action).toHaveBeenCalledOnce();

    finishAction?.({ ...activeLease, pending_operation_ids: [] });
    expect(await screen.findByRole('button', { name: '归还控制权' })).toBeEnabled();
  });

  it('lets the owner recover an expired lease by releasing it to AI', async () => {
    let status: ControlStatus = expiredLease;
    vi.spyOn(portalApi, 'hostControl').mockImplementation(async () => status);
    const action = vi.spyOn(portalApi, 'hostControlAction').mockImplementation(async (_hostID, _request) => {
      const result: ControlStatus = { host_id: host.id, control_state: 'ai', control_version: 5, control_actor_user_id: '',
        control_lease_expires_at: null, control_lease_active: false, control_lease_held_by_me: false, active_transition: null };
      status = result;
      return { ...result, pending_operation_ids: [] };
    });
    renderPanel();

    const recover = await screen.findByRole('button', { name: '恢复 AI 控制' });
    expect(recover).toBeEnabled();
    fireEvent.click(recover);

    await waitFor(() => expect(action).toHaveBeenCalledWith(host.id, {
      action: 'release', request_id: expect.any(String), expected_control_version: 3,
    }));
  });
});
