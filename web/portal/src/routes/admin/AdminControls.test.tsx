import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { portalApi } from '../../app/api';
import { AdminControls } from './AdminControls';

type ControlsData = Awaited<ReturnType<typeof portalApi.adminControls>>;

const initialData: ControlsData = {
  rules: [{
    id: 3,
    threshold: 100,
    enabled: true,
    note: '首次扩容通知',
    created_at: '2026-10-20T08:00:00Z',
    last_fired_at: '2026-10-28T08:00:00Z',
    fired_accounts: 101,
    version: 2,
  }],
  accounts_total: 108,
  next_threshold: 200,
  notify_configured: true,
  notifications: [{ kind: 'rule_fired', rule_id: 3, accounts_total: 101, created_at: '2026-10-28T08:00:00Z' }],
};

function renderPage() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(<QueryClientProvider client={client}><AdminControls /></QueryClientProvider>);
}

describe('admin notify-only controls', () => {
  afterEach(() => { cleanup(); vi.restoreAllMocks(); });

  it('shows notify-only rules and the local inbox without global switches', async () => {
    vi.spyOn(portalApi, 'adminControls').mockResolvedValue(initialData);
    renderPage();

    expect(await screen.findByRole('row', { name: /100 个账号/ })).toBeInTheDocument();
    expect(screen.getByText(/所有通知只写入 Morde 本地接收箱，不会发送到外部服务/)).toBeInTheDocument();
    expect(screen.getByText(/不会关闭注册、暂停服务或修改额度/)).toBeInTheDocument();
    expect(screen.getByRole('row', { name: /100 个账号/ })).toHaveTextContent('v2');
    expect(screen.getByRole('heading', { name: '本地接收箱' })).toBeInTheDocument();
    expect(screen.getByRole('row', { name: /rule_fired.*#3.*101/ })).toBeInTheDocument();
    expect(screen.queryByText(/注册总开关|全局开关/)).not.toBeInTheDocument();
  });

  it('requires confirmation before creating a notify-only rule', async () => {
    const user = userEvent.setup();
    let data = { ...initialData, rules: [] as ControlsData['rules'] };
    vi.spyOn(portalApi, 'adminControls').mockImplementation(async () => data);
    const create = vi.spyOn(portalApi, 'createAdminControlRule').mockImplementation(async (body) => {
      const created = { ...initialData.rules[0]!, id: 4, threshold: body.threshold, note: body.note ?? '', last_fired_at: null, fired_accounts: 0, version: 1 };
      data = { ...data, rules: [created] };
      return created;
    });
    renderPage();

    await screen.findByText('尚未创建通知规则。');
    await user.type(screen.getByLabelText('账号阈值'), '500');
    await user.type(screen.getByLabelText('备注'), '容量提醒');
    await user.click(screen.getByRole('button', { name: '继续创建' }));
    const confirmation = screen.getByRole('group', { name: '确认创建通知规则' });
    expect(create).not.toHaveBeenCalled();
    expect(confirmation).toHaveTextContent('账号数达到 500 时通知本地接收箱');
    await user.click(within(confirmation).getByRole('button', { name: '确认创建' }));

    expect(create).toHaveBeenCalledWith({ threshold: 500, note: '容量提醒' });
    expect(await screen.findByRole('row', { name: /500 个账号/ })).toHaveTextContent('容量提醒');
    expect(await screen.findByText('通知规则已创建。')).toBeInTheDocument();
  });

  it('requires confirmation before updating and sends the expected version and rearm flag', async () => {
    const user = userEvent.setup();
    let data = initialData;
    vi.spyOn(portalApi, 'adminControls').mockImplementation(async () => data);
    const update = vi.spyOn(portalApi, 'updateAdminControlRule').mockImplementation(async (id, body) => {
      const current = data.rules.find((item) => item.id === id)!;
      const saved = { ...current, ...body, version: current.version + 1 };
      data = { ...data, rules: data.rules.map((item) => item.id === id ? saved : item) };
      return saved;
    });
    renderPage();

    const row = await screen.findByRole('row', { name: /100 个账号/ });
    await user.click(within(row).getByRole('button', { name: '编辑' }));
    const editThreshold = screen.getByLabelText('账号阈值', { selector: '#control-edit-threshold-3' });
    const editNote = screen.getByLabelText('备注', { selector: '#control-edit-note-3' });
    await user.clear(editThreshold);
    await user.type(editThreshold, '120');
    await user.clear(editNote);
    await user.type(editNote, '更新阈值');
    await user.click(screen.getByRole('checkbox', { name: /重新武装此规则/ }));
    await user.click(screen.getByRole('button', { name: '继续保存' }));
    const confirmation = screen.getByRole('group', { name: '确认修改通知规则 3' });
    expect(update).not.toHaveBeenCalled();
    await user.click(within(confirmation).getByRole('button', { name: '确认保存' }));

    expect(update).toHaveBeenCalledWith(3, { expected_version: 2, threshold: 120, enabled: true, note: '更新阈值', rearm: true });
    expect(await screen.findByRole('row', { name: /120 个账号/ })).toHaveTextContent('v3');
    expect(await screen.findByText('通知规则已保存。')).toBeInTheDocument();
  });

  it('requires confirmation before deleting and passes the current version', async () => {
    const user = userEvent.setup();
    let data = initialData;
    vi.spyOn(portalApi, 'adminControls').mockImplementation(async () => data);
    const remove = vi.spyOn(portalApi, 'deleteAdminControlRule').mockImplementation(async (id) => {
      data = { ...data, rules: data.rules.filter((item) => item.id !== id) };
    });
    renderPage();

    const row = await screen.findByRole('row', { name: /100 个账号/ });
    await user.click(within(row).getByRole('button', { name: '删除' }));
    const confirmation = screen.getByRole('group', { name: '确认删除通知规则 3' });
    expect(remove).not.toHaveBeenCalled();
    await user.click(within(confirmation).getByRole('button', { name: '确认删除' }));

    expect(remove).toHaveBeenCalledWith(3, 2);
    expect(await screen.findByText('尚未创建通知规则。')).toBeInTheDocument();
    expect(await screen.findByText('通知规则已删除。')).toBeInTheDocument();
  });

  it('reloads the latest rule version after a conflict', async () => {
    const user = userEvent.setup();
    let data = initialData;
    vi.spyOn(portalApi, 'adminControls').mockImplementation(async () => data);
    vi.spyOn(portalApi, 'updateAdminControlRule').mockRejectedValueOnce(Object.assign(new Error('规则版本冲突'), { code: 'conflict', status: 409 }));
    renderPage();

    const row = await screen.findByRole('row', { name: /100 个账号/ });
    await user.click(within(row).getByRole('button', { name: '编辑' }));
    await user.click(screen.getByRole('button', { name: '继续保存' }));
    await user.click(screen.getByRole('button', { name: '确认保存' }));
    expect(await screen.findByText(/规则版本已变化/)).toBeInTheDocument();

    data = { ...initialData, rules: [{ ...initialData.rules[0]!, version: 3, threshold: 120 }] };
    await user.click(screen.getByRole('button', { name: '重新加载' }));
    expect(await screen.findByRole('row', { name: /120 个账号/ })).toHaveTextContent('v3');
    await user.click(within(screen.getByRole('row', { name: /120 个账号/ })).getByRole('button', { name: '编辑' }));
    expect(screen.getByLabelText('账号阈值', { selector: '#control-edit-threshold-3' })).toHaveValue(120);
  });

  it('evaluates notify rules and writes a test notification only to the local inbox', async () => {
    const user = userEvent.setup();
    let data = { ...initialData, notifications: [] as ControlsData['notifications'] };
    vi.spyOn(portalApi, 'adminControls').mockImplementation(async () => data);
    const evaluate = vi.spyOn(portalApi, 'evaluateAdminControls').mockResolvedValue({ accounts_total: 108, fired: [] });
    const notifyTest = vi.spyOn(portalApi, 'adminControlNotifyTest').mockImplementation(async () => {
      data = { ...data, notifications: [{ kind: 'test', rule_id: null, accounts_total: 108, created_at: '2026-11-02T08:00:00Z' }] };
      return { sent: true, configured: true, created_at: '2026-11-02T08:00:00Z' };
    });
    renderPage();

    await screen.findByRole('row', { name: /100 个账号/ });
    await user.click(screen.getByRole('button', { name: '立即评估' }));
    expect(evaluate).toHaveBeenCalledOnce();
    expect(await screen.findByText('评估了 108 个账号，触发 0 条通知规则。')).toBeInTheDocument();

    await user.click(screen.getByRole('button', { name: '写入测试通知' }));
    expect(notifyTest).toHaveBeenCalledOnce();
    expect(await screen.findByText(/测试通知已写入本地接收箱/)).toBeInTheDocument();
    expect(await screen.findByRole('row', { name: /test.*108/ })).toBeInTheDocument();
    expect(screen.getByText('—')).toBeInTheDocument();
  });
});
