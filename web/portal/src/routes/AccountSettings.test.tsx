import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, cleanup, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { MemoryRouter } from 'react-router';
import { portalApi } from '../app/api';
import { ToastNoticeCenter, ToastProvider } from '../app/toast';
import { AccountSettings } from './AccountSettings';

vi.mock('../app/auth', () => ({ useAuth: () => ({ user: { id: 'u1', name: 'Alice', role: 'user' } }) }));
const personalSyncStore = vi.hoisted(() => ({
  subscribeStatus: vi.fn((_userID: string, listener: (status: { pendingCount: number; conflictCount: number }) => void) => {
    listener({ pendingCount: 2, conflictCount: 1 });
    return vi.fn();
  }),
}));
vi.mock('./personal-data-store', () => ({
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
    vi.spyOn(portalApi, 'capabilities').mockResolvedValue({ service_instance_id: 'service-account-test' } as Awaited<ReturnType<typeof portalApi.capabilities>>);
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
    const view = render(<QueryClientProvider client={queryClient}><ToastProvider><ToastNoticeCenter /><MemoryRouter><AccountSettings /></MemoryRouter></ToastProvider></QueryClientProvider>);
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
    const user = userEvent.setup();
    await user.click(screen.getByRole('tab', { name: '隐私与同意' }));
    expect(await screen.findByRole('checkbox', { name: '允许保存完整会话记录' })).not.toBeChecked();
    expect(screen.getByRole('checkbox', { name: '允许保存训练样本贡献' })).not.toBeChecked();
    expect(screen.queryByRole('checkbox', { name: '同步个人数据到当前账号' })).not.toBeInTheDocument();
    await user.click(screen.getByRole('tab', { name: '个人数据' }));
    expect(screen.getByText('保存后自动更新云端；手机连接后会获取最新资料。')).toBeInTheDocument();
    expect(screen.getByText('待提交 2 项 · 冲突 1 项')).toBeInTheDocument();
    expect(screen.getByRole('link', { name: '个人数据' })).toHaveAttribute('href', '/personal');
    expect(personalSyncStore.subscribeStatus).toHaveBeenCalledWith('u1', expect.any(Function), window.location.origin, 'service-account-test');
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
    await user.click(screen.getByRole('tab', { name: '隐私与同意' }));

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
    await user.click(screen.getByRole('tab', { name: '已保存记录' }));

    await user.click(await screen.findByRole('button', { name: '删除记录' }));
    expect(screen.getByRole('group', { name: '确认删除记录 v1234567' })).toHaveTextContent('不会删除普通会话或资源库附件');
    expect(deleteRecord).not.toHaveBeenCalled();
    await user.click(screen.getByRole('button', { name: '确认删除记录' }));

    await waitFor(() => expect(deleteRecord.mock.calls[0]?.[0]).toBe('v1234567'));
    expect(await screen.findByText('删除请求已受理；后台清理完成前，记录仍可能短暂显示在列表中。')).toBeInTheDocument();
  });

  it('keeps a record confirmation when switching between compact account sections', async () => {
    const user = userEvent.setup();
    const records = { items: [{ id: 'v7654321', owner_id: 'u1', purpose: 'full_record' as const,
      kind: 'turn' as const, consent_version: 1, created_at: '2026-11-02T00:00:00Z', expires_at: '2026-12-02T00:00:00Z',
      payload_bytes: 32, attachment_bytes: 0, attachment_count: 0, state: 'active' as const }], next_before: '' };
    const deleteRecord = vi.spyOn(portalApi, 'deleteMyRecord');
    renderSettings(records);

    await user.click(screen.getByRole('tab', { name: '已保存记录' }));
    await user.click(await screen.findByRole('button', { name: '删除记录' }));
    expect(screen.getByRole('group', { name: '确认删除记录 v7654321' })).toBeInTheDocument();
    await user.click(screen.getByRole('tab', { name: '账号' }));
    await user.click(screen.getByRole('tab', { name: '已保存记录' }));

    expect(screen.getByRole('group', { name: '确认删除记录 v7654321' })).toBeInTheDocument();
    expect(deleteRecord).not.toHaveBeenCalled();
  });

  it('uses four item pages for long record lists instead of expanding the page vertically', async () => {
    const user = userEvent.setup();
    const recordsQuery = vi.spyOn(portalApi, 'myRecords').mockResolvedValue({ items: [], next_before: '' });
    renderSettings();

    await user.click(screen.getByRole('tab', { name: '已保存记录' }));
    await waitFor(() => expect(recordsQuery).toHaveBeenCalledWith({ limit: 4 }));
    expect(await screen.findByRole('heading', { name: '没有已保存记录' })).toBeInTheDocument();
  });

  it('moves through four-record pages and keeps the previous page available', async () => {
    const user = userEvent.setup();
    const item = (index: number) => ({ id: `v${index.toString().padStart(7, '0')}`, owner_id: 'u1', purpose: 'full_record' as const,
      kind: 'turn' as const, consent_version: 1, created_at: '2026-11-02T00:00:00Z', expires_at: '2026-12-02T00:00:00Z',
      payload_bytes: 100 + index, attachment_bytes: 0, attachment_count: 0, state: 'active' as const });
    renderSettings({ items: [item(0), item(1), item(2), item(3)], next_before: 'page-two' });
    const recordsQuery = vi.mocked(portalApi.myRecords);
    recordsQuery.mockResolvedValueOnce({ items: [item(4)], next_before: '' });
    await user.click(screen.getByRole('tab', { name: '已保存记录' }));
    const hasRecordBytes = (bytes: string) => screen.getAllByRole('article').some((article) => article.textContent?.includes(bytes));

    await waitFor(() => expect(hasRecordBytes('100 B')).toBe(true));
    await user.click(screen.getByRole('button', { name: '下一页' }));
    await waitFor(() => expect(hasRecordBytes('104 B')).toBe(true));
    expect(hasRecordBytes('100 B')).toBe(false);
    expect(recordsQuery).toHaveBeenLastCalledWith({ limit: 4, before_id: 'page-two' });
    await user.click(screen.getByRole('button', { name: '上一页' }));
    await waitFor(() => expect(hasRecordBytes('100 B')).toBe(true));
    expect(hasRecordBytes('104 B')).toBe(false);
  });

  it('shows read-only personal save status without a pause control', async () => {
    vi.spyOn(portalApi, 'recordConsents').mockResolvedValue({ items: [] });
    renderSettings();
    await userEvent.setup().click(screen.getByRole('tab', { name: '个人数据' }));

    expect(await screen.findByText('待提交 2 项 · 冲突 1 项')).toBeInTheDocument();
    expect(screen.queryByRole('checkbox', { name: '同步个人数据到当前账号' })).not.toBeInTheDocument();
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
    await userEvent.setup().click(screen.getByRole('tab', { name: '记录状态' }));

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
    await userEvent.setup().click(screen.getByRole('tab', { name: '记录状态' }));

    expect(screen.getByText('正在读取任务记录状态…')).toBeInTheDocument();
    await act(async () => { resolvePage(emptyCapturePage); await page; });
    expect(await screen.findByRole('heading', { name: '没有任务记录状态' })).toBeInTheDocument();
  });

  it('shows a retry action when task capture statuses fail to load', async () => {
    vi.spyOn(portalApi, 'recordConsents').mockResolvedValue({ items: [] });
    renderSettings(undefined, [{ error: new Error('状态读取失败') }]);
    await userEvent.setup().click(screen.getByRole('tab', { name: '记录状态' }));

    expect(await screen.findByRole('alert')).toHaveTextContent('状态读取失败');
    expect(screen.getByRole('button', { name: '重试' })).toBeInTheDocument();
  });

  it('polls while capture statuses are pending and stops after they settle', async () => {
    vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] });
    vi.spyOn(portalApi, 'recordConsents').mockResolvedValue({ items: [] });
    const pending = { items: [captureStatus('j1234567', 'leased')], next_before: '' };
    const stored = { items: [captureStatus('j1234567', 'stored')], next_before: '' };
    const { captureStatuses } = renderSettings(undefined, [{ page: pending }, { page: stored }]);
    await userEvent.setup().click(screen.getByRole('tab', { name: '记录状态' }));

    expect(await screen.findByText('等待持久化')).toBeInTheDocument();
    expect(captureStatuses).toHaveBeenCalledTimes(1);
    await act(async () => { await vi.advanceTimersByTimeAsync(3000); });
    expect(captureStatuses).toHaveBeenCalledTimes(2);
    expect(await screen.findByText('已保存')).toBeInTheDocument();
    await act(async () => { await vi.advanceTimersByTimeAsync(6000); });
    expect(captureStatuses).toHaveBeenCalledTimes(2);
  });
});
