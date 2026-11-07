import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, cleanup, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { MemoryRouter } from 'react-router';
import { portalApi } from '../app/api';
import { ToastProvider } from '../app/toast';
import { AccountSettings } from './AccountSettings';

vi.mock('../app/auth', () => ({ useAuth: () => ({ user: { id: 'u1', name: 'Alice', role: 'user' } }) }));
const personalSyncStore = vi.hoisted(() => ({
  getEnabled: vi.fn(() => true),
  setEnabled: vi.fn(),
  subscribeEnabled: vi.fn((_userID: string, listener: (enabled: boolean) => void) => { listener(true); return vi.fn(); }),
  subscribeStatus: vi.fn((_userID: string, listener: (status: { enabled: boolean; pendingCount: number; conflictCount: number }) => void) => {
    listener({ enabled: true, pendingCount: 2, conflictCount: 1 });
    return vi.fn();
  }),
}));
vi.mock('./personal-data-store', () => ({
  getPersonalDataSyncEnabled: personalSyncStore.getEnabled,
  setPersonalDataSyncEnabled: personalSyncStore.setEnabled,
  subscribePersonalDataSyncEnabled: personalSyncStore.subscribeEnabled,
  subscribePersonalDataSyncStatus: personalSyncStore.subscribeStatus,
}));

type CapturePage = Awaited<ReturnType<typeof portalApi.myRecordCaptureStatuses>>;
type CaptureStatus = CapturePage['items'][number];
type CaptureResult = { page: CapturePage } | { error: Error } | { promise: Promise<CapturePage> };
const emptyCapturePage: CapturePage = { items: [], next_before: '' };

function captureStatus(id: string, state: CaptureStatus['state'], overrides: Partial<CaptureStatus> = {}): CaptureStatus {
  return {
    id,
    task_id: 't1234567',
    purpose: 'full_record',
    kind: 'turn',
    state,
    failure_code: '',
    captured_at: '2026-11-02T00:00:00Z',
    expires_at: '2026-12-02T00:00:00Z',
    ...overrides,
  };
}

describe('AccountSettings', () => {
  afterEach(() => { cleanup(); vi.restoreAllMocks(); });

  function renderSettings(
    records: Awaited<ReturnType<typeof portalApi.myRecords>> = { items: [], next_before: '' },
    captureResults: CaptureResult[] = [{ page: emptyCapturePage }],
  ) {
    vi.spyOn(portalApi, 'personalDataQuota').mockResolvedValue({ owner_id: 'u1', used_bytes: 10, reserved_bytes: 2,
      quota_bytes: 1000, max_attachment_bytes: 500, remaining_bytes: 988, version: 1 });
    vi.spyOn(portalApi, 'myRecords').mockResolvedValue(records);
    const captureStatuses = vi.spyOn(portalApi, 'myRecordCaptureStatuses');
    const results = captureResults.length ? captureResults : [{ page: emptyCapturePage }];
    const setResult = (result: CaptureResult, once: boolean) => {
      if ('page' in result) return once ? captureStatuses.mockResolvedValueOnce(result.page) : captureStatuses.mockResolvedValue(result.page);
      if ('error' in result) return once ? captureStatuses.mockRejectedValueOnce(result.error) : captureStatuses.mockRejectedValue(result.error);
      return once ? captureStatuses.mockReturnValueOnce(result.promise) : captureStatuses.mockReturnValue(result.promise);
    };
    results.slice(0, -1).forEach((result) => { setResult(result, true); });
    setResult(results[results.length - 1]!, false);
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
    const view = render(<QueryClientProvider client={queryClient}><ToastProvider><MemoryRouter><AccountSettings /></MemoryRouter></ToastProvider></QueryClientProvider>);
    return { ...view, captureStatuses };
  }

  it('shows the signed-in account, links, and default-off record consent controls', async () => {
    vi.spyOn(portalApi, 'recordConsents').mockResolvedValue({ items: [
      { owner_id: 'u1', purpose: 'full_record', enabled: false, version: 0 },
      { owner_id: 'u1', purpose: 'training_contribution', enabled: false, version: 0 },
    ] });
    renderSettings();

    expect(screen.getByRole('heading', { level: 1, name: '账户设置' })).toBeInTheDocument();
    expect(screen.getByRole('heading', { level: 3, name: 'Alice' })).toBeInTheDocument();
    expect(screen.getByText('普通用户')).toBeInTheDocument();
    expect(screen.getByRole('link', { name: '修改密码' })).toHaveAttribute('href', '/settings/password');
    expect(screen.getByRole('link', { name: '管理客户端' })).toHaveAttribute('href', '/clients');
    expect(screen.queryByRole('heading', { name: '我的邀请码' })).not.toBeInTheDocument();
    expect(await screen.findByRole('checkbox', { name: '允许保存完整会话记录' })).not.toBeChecked();
    expect(screen.getByRole('checkbox', { name: '允许保存训练样本贡献' })).not.toBeChecked();
    expect(screen.getByRole('checkbox', { name: '同步个人数据到当前账号' })).toBeChecked();
    expect(screen.getByText(/待同步 2 项 · 冲突 1 项/)).toBeInTheDocument();
    expect(screen.getByRole('link', { name: '个人数据' })).toHaveAttribute('href', '/personal');
  });

  it('updates one independent consent and reloads its server state', async () => {
    const user = userEvent.setup();
    vi.spyOn(portalApi, 'recordConsents')
      .mockResolvedValueOnce({ items: [
        { owner_id: 'u1', purpose: 'full_record', enabled: false, version: 0 },
        { owner_id: 'u1', purpose: 'training_contribution', enabled: false, version: 0 },
      ] })
      .mockResolvedValueOnce({ items: [
        { owner_id: 'u1', purpose: 'full_record', enabled: true, version: 1 },
        { owner_id: 'u1', purpose: 'training_contribution', enabled: false, version: 0 },
      ] });
    const setConsent = vi.spyOn(portalApi, 'setRecordConsent').mockResolvedValue({ owner_id: 'u1', purpose: 'full_record', enabled: true, version: 1 });
    renderSettings();

    await user.click(await screen.findByRole('checkbox', { name: '允许保存完整会话记录' }));
    await waitFor(() => expect(setConsent.mock.calls[0]?.[0]).toEqual({ purpose: 'full_record', enabled: true }));
    expect(await screen.findByRole('checkbox', { name: '允许保存完整会话记录' })).toBeChecked();
    expect(screen.getByRole('checkbox', { name: '允许保存训练样本贡献' })).not.toBeChecked();
  });

  it('shows record metadata and requests owner deletion only after confirmation', async () => {
    const user = userEvent.setup();
    vi.spyOn(portalApi, 'recordConsents').mockResolvedValue({ items: [
      { owner_id: 'u1', purpose: 'full_record', enabled: false, version: 1 },
      { owner_id: 'u1', purpose: 'training_contribution', enabled: false, version: 0 },
    ] });
    const records = { items: [{ id: 'v1234567', owner_id: 'u1', purpose: 'full_record' as const,
      kind: 'turn' as const, consent_version: 1, created_at: '2026-11-02T00:00:00Z', expires_at: '2026-12-02T00:00:00Z',
      payload_bytes: 32, attachment_bytes: 0, attachment_count: 0, state: 'active' as const }], next_before: '' };
    const deleteRecord = vi.spyOn(portalApi, 'deleteMyRecord').mockResolvedValue({ id: 'l1234567', owner_id: 'u1',
      record_id: 'v1234567', state: 'pending', attempts: 0, requested_at: '2026-11-02T00:00:00Z' });
    renderSettings(records);

    await user.click(await screen.findByRole('button', { name: '删除记录' }));
    expect(screen.getByRole('group', { name: '确认删除记录 v1234567' })).toHaveTextContent('不会删除普通会话或资源库附件');
    expect(deleteRecord).not.toHaveBeenCalled();
    await user.click(screen.getByRole('button', { name: '确认删除记录' }));

    await waitFor(() => expect(deleteRecord.mock.calls[0]?.[0]).toBe('v1234567'));
    expect(await screen.findByText('删除请求已受理；后台清理完成前，记录仍可能短暂显示在列表中。')).toBeInTheDocument();
  });

  it('pauses only personal sync when its setting is switched off', async () => {
    const user = userEvent.setup();
    vi.spyOn(portalApi, 'recordConsents').mockResolvedValue({ items: [] });
    renderSettings();

    await user.click(await screen.findByRole('checkbox', { name: '同步个人数据到当前账号' }));

    expect(personalSyncStore.setEnabled).toHaveBeenCalledWith('u1', false, window.location.origin);
    expect(screen.getByText(/同步已暂停/)).toBeInTheDocument();
    expect(screen.getByText(/待同步内容留在本机/)).toBeInTheDocument();
  });

  it('shows task capture statuses as metadata with safe failure-code labels', async () => {
    vi.spyOn(portalApi, 'recordConsents').mockResolvedValue({ items: [] });
    const page = {
      items: [
        captureStatus('j1111111', 'queued'),
        captureStatus('j2222222', 'leased', { purpose: 'training_contribution', kind: 'sample' }),
        captureStatus('j3333333', 'stored'),
        captureStatus('j4444444', 'failed', { failure_code: 'queue_full' }),
        captureStatus('j5555555', 'rejected', { failure_code: 'content_rejected' }),
        captureStatus('j6666666', 'failed', { failure_code: '<script>alert(1)</script>' }),
      ].map((item) => ({
        ...item,
        payload: 'private record body',
        attachments: ['private-file'],
        lease_owner: 'internal-worker',
        attempts: 9,
        reserved_bytes: 1024,
      })),
      next_before: '',
    } as unknown as CapturePage;
    renderSettings(undefined, [{ page }]);

    await screen.findAllByText('等待持久化');
    const section = await screen.findByRole('region', { name: '任务记录采集状态' });
    expect(within(section).getAllByText('等待持久化')).toHaveLength(2);
    expect(within(section).getByText('已保存')).toBeInTheDocument();
    expect(within(section).getAllByText('未保存')).toHaveLength(3);
    expect(within(section).getByText('queue_full').tagName).toBe('CODE');
    expect(within(section).getByText('content_rejected').tagName).toBe('CODE');
    expect(within(section).getByText(/暂不可用/)).toBeInTheDocument();
    expect(section).not.toHaveTextContent(/private record body|private-file|internal-worker|1024|j1111111|t1234567/);
    expect(section).toHaveTextContent('采集于');
    expect(section).toHaveTextContent('到期');
  });

  it('shows a loading state then a clear empty state for task capture statuses', async () => {
    vi.spyOn(portalApi, 'recordConsents').mockResolvedValue({ items: [] });
    let resolvePage!: (page: CapturePage) => void;
    const page = new Promise<CapturePage>((resolve) => { resolvePage = resolve; });
    renderSettings(undefined, [{ promise: page }]);

    expect(screen.getByText('正在读取任务记录状态…')).toBeInTheDocument();
    await act(async () => { resolvePage(emptyCapturePage); await page; });
    expect(await screen.findByRole('heading', { name: '没有任务记录状态' })).toBeInTheDocument();
  });

  it('shows a retry action when task capture statuses fail to load', async () => {
    vi.spyOn(portalApi, 'recordConsents').mockResolvedValue({ items: [] });
    renderSettings(undefined, [{ error: new Error('状态读取失败') }]);

    expect(await screen.findByRole('alert')).toHaveTextContent('状态读取失败');
    expect(screen.getByRole('button', { name: '重试' })).toBeInTheDocument();
  });

  it('polls while capture statuses are pending and stops after they settle', async () => {
    vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] });
    vi.spyOn(portalApi, 'recordConsents').mockResolvedValue({ items: [] });
    const pending = { items: [captureStatus('j1234567', 'leased')], next_before: '' };
    const stored = { items: [captureStatus('j1234567', 'stored')], next_before: '' };
    const { captureStatuses } = renderSettings(undefined, [{ page: pending }, { page: stored }]);

    expect(await screen.findByText('等待持久化')).toBeInTheDocument();
    expect(captureStatuses).toHaveBeenCalledTimes(1);
    await act(async () => { await vi.advanceTimersByTimeAsync(3000); });
    expect(captureStatuses).toHaveBeenCalledTimes(2);
    expect(await screen.findByText('已保存')).toBeInTheDocument();
    await act(async () => { await vi.advanceTimersByTimeAsync(6000); });
    expect(captureStatuses).toHaveBeenCalledTimes(2);
  });
});
