import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { components } from '../api/schema';
import { ApiError } from '../api/client';
import { portalApi } from '../app/api';
import { ToastProvider } from '../app/toast';
import { SchedulePanel } from './SchedulePanel';

const timestamp = '2026-10-23T01:00:00.000Z';
const host = { id: 'h1234567', name: '开发电脑', owner_id: 'u1234567', template_id: 'std', isolation: 'container', state: 'running', state_reason: '', runtime_generation: 1, control_version: 1, control_state: 'ai', url: '', workspaces: [], resources: { cpu_milli: 500, mem_mib: 1024, cpu_limit_milli: 1000, mem_limit_mib: 2048 }, expires_at: timestamp, last_active_at: timestamp, created_at: timestamp } satisfies Awaited<ReturnType<typeof portalApi.hosts>>['items'][number];
const model = { id: 'p1234567', name: '团队模型', base_url: 'https://models.example.test/v1', model_name: 'morde-test', shared: true, has_api_key: true, capabilities: { text: true, tools: true, vision: false }, checked_at: timestamp, created_at: timestamp, updated_at: timestamp } satisfies Awaited<ReturnType<typeof portalApi.modelConnections>>['items'][number];
const onboarding = { model_status: 'ready', models: [{ id: model.id, name: model.name, capabilities: model.capabilities }], selected_model_id: model.id, computer_status: 'ready', computers: [{ id: host.id, name: host.name, state: 'running' }], default_host_id: host.id, can_submit: true, next_action: 'submit_task' } satisfies Awaited<ReturnType<typeof portalApi.onboardingStatus>>;
const run: components['schemas']['ScheduleRun'] = { id: 'y1234567', schedule_id: 'j1234567', schedule_version: 3, occurrence_key: '2026-10-23', scheduled_at: timestamp, coalesced_count: 0, status: 'running', skip_reason: null, task_id: 'q1234567', observation_outcome: 'unknown', created_at: timestamp, updated_at: timestamp, finished_at: null };
const schedule: components['schemas']['Schedule'] = { id: 'j1234567', version: 3, status: 'enabled', host_id: host.id, model_id: model.id, objective: '整理项目目录', timing: { kind: 'daily', starts_on: '2026-10-23', local_time: '09:00', time_zone: 'Asia/Shanghai' }, budget: { active_seconds: 1800, tool_calls: 100, model_tokens: 50000 }, misfire_policy: 'skip', notification_events: ['completed', 'failed', 'waiting_user'], observation: null, next_run_at: timestamp, last_run: run, created_at: timestamp, updated_at: timestamp };

function renderPanel() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(<QueryClientProvider client={client}><ToastProvider><MemoryRouter><SchedulePanel /></MemoryRouter></ToastProvider></QueryClientProvider>);
}

describe('SchedulePanel', () => {
  beforeEach(() => {
    vi.spyOn(portalApi, 'schedules').mockResolvedValue({ items: [], next_cursor: null });
    vi.spyOn(portalApi, 'hosts').mockResolvedValue({ items: [host] });
    vi.spyOn(portalApi, 'modelConnections').mockResolvedValue({ items: [model] });
    vi.spyOn(portalApi, 'onboardingStatus').mockResolvedValue(onboarding);
  });
  afterEach(() => { cleanup(); vi.restoreAllMocks(); });

  it('creates a once schedule with explicit Host, model, policy, and notification choices', async () => {
    const create = vi.spyOn(portalApi, 'createSchedule').mockResolvedValue(schedule);
    const user = userEvent.setup();
    renderPanel();
    await user.click(screen.getByRole('button', { name: '＋ 新建计划' }));
    await user.type(await screen.findByLabelText('任务目标'), '每天汇总新文件');
    await user.selectOptions(screen.getByLabelText('错过执行时间'), 'run_once');
    await user.click(screen.getByRole('checkbox', { name: '任务完成' }));
    await user.click(screen.getByRole('button', { name: '创建并启用' }));

    await waitFor(() => expect(create).toHaveBeenCalledWith(expect.objectContaining({
      host_id: host.id, model_id: model.id, objective: '每天汇总新文件', misfire_policy: 'run_once', notification_events: ['failed', 'waiting_user'],
      timing: expect.objectContaining({ kind: 'once', time_zone: expect.any(String), run_at: expect.any(String) }),
    })));
  });

  it('submits a valid recurring weekly wall time', async () => {
    const create = vi.spyOn(portalApi, 'createSchedule').mockResolvedValue(schedule);
    const user = userEvent.setup();
    renderPanel();
    await user.click(screen.getByRole('button', { name: '＋ 新建计划' }));
    await user.type(await screen.findByLabelText('任务目标'), '每周汇总');
    await user.click(screen.getByLabelText('每周'));
    await user.click(screen.getByRole('button', { name: '创建并启用' }));

    await waitFor(() => expect(create).toHaveBeenCalledWith(expect.objectContaining({
      timing: { kind: 'weekly', starts_on: expect.any(String), local_time: '09:00', time_zone: expect.any(String), weekdays: [1, 2, 3, 4, 5] },
    })));
  });

  it('submits a daily local wall time and timezone', async () => {
    const create = vi.spyOn(portalApi, 'createSchedule').mockResolvedValue(schedule);
    const user = userEvent.setup();
    renderPanel();
    await user.click(screen.getByRole('button', { name: '＋ 新建计划' }));
    await user.type(await screen.findByLabelText('任务目标'), '每天生成摘要');
    await user.click(screen.getByLabelText('每天'));
    await user.click(screen.getByRole('button', { name: '创建并启用' }));

    await waitFor(() => expect(create).toHaveBeenCalledWith(expect.objectContaining({
      timing: { kind: 'daily', starts_on: expect.any(String), local_time: '09:00', time_zone: expect.any(String) },
    })));
  });

  it('creates a web field observation without exposing the observed content', async () => {
    const create = vi.spyOn(portalApi, 'createSchedule').mockResolvedValue(schedule);
    const user = userEvent.setup();
    renderPanel();
    await user.click(screen.getByRole('button', { name: '＋ 新建计划' }));
    await user.type(await screen.findByLabelText('任务目标'), '检查状态页');
    await user.click(screen.getByRole('checkbox', { name: '独立检查指定网页字段是否变化' }));
    await user.type(screen.getByLabelText('网页地址（HTTP 或 HTTPS）'), 'https://status.example.test/');
    await user.type(screen.getByLabelText('CSS 选择器'), '#service-status');
    await user.selectOptions(screen.getByLabelText('采集字段'), 'value');
    await user.click(screen.getByRole('button', { name: '创建并启用' }));

    await waitFor(() => expect(create).toHaveBeenCalledWith(expect.objectContaining({
      observation: { url: 'https://status.example.test/', selector: '#service-status', field: 'value' },
    })));
  });

  it('rejects non-HTTP observation URLs before saving', async () => {
    const create = vi.spyOn(portalApi, 'createSchedule').mockResolvedValue(schedule);
    const user = userEvent.setup();
    renderPanel();
    await user.click(screen.getByRole('button', { name: '＋ 新建计划' }));
    await user.type(await screen.findByLabelText('任务目标'), '检查状态页');
    await user.click(screen.getByRole('checkbox', { name: '独立检查指定网页字段是否变化' }));
    await user.type(screen.getByLabelText('网页地址（HTTP 或 HTTPS）'), 'ftp://status.example.test/');
    await user.type(screen.getByLabelText('CSS 选择器'), '#service-status');
    await user.click(screen.getByRole('button', { name: '创建并启用' }));

    expect(await screen.findByRole('alert')).toHaveTextContent('网页地址必须使用 HTTP 或 HTTPS。');
    expect(create).not.toHaveBeenCalled();
  });

  it('prefills and clears a configured observation when editing', async () => {
    const observedSchedule: components['schemas']['Schedule'] = {
      ...schedule,
      observation: { url: 'https://status.example.test/', selector: '#service-status', field: 'text' },
    };
    vi.mocked(portalApi.schedules).mockResolvedValue({ items: [observedSchedule], next_cursor: null });
    const update = vi.spyOn(portalApi, 'updateSchedule').mockResolvedValue(observedSchedule);
    const user = userEvent.setup();
    renderPanel();
    await user.click(await screen.findByRole('button', { name: '编辑' }));
    expect(screen.getByLabelText('网页地址（HTTP 或 HTTPS）')).toHaveValue('https://status.example.test/');
    expect(screen.getByLabelText('CSS 选择器')).toHaveValue('#service-status');
    await user.click(screen.getByRole('checkbox', { name: '独立检查指定网页字段是否变化' }));
    await user.click(screen.getByRole('button', { name: '保存计划' }));

    await waitFor(() => expect(update).toHaveBeenCalledWith(observedSchedule.id, expect.objectContaining({ observation: null })));
  });

  it('uses the schedule version when editing and renders linked run history', async () => {
    vi.mocked(portalApi.schedules).mockResolvedValue({ items: [schedule], next_cursor: null });
    const update = vi.spyOn(portalApi, 'updateSchedule').mockResolvedValue({ ...schedule, version: 4, objective: '整理并归档项目目录' });
    vi.spyOn(portalApi, 'scheduleRuns').mockResolvedValue({ items: [run], next_cursor: null });
    const user = userEvent.setup();
    renderPanel();

    await user.click(await screen.findByRole('button', { name: '编辑' }));
    const objective = screen.getByRole('textbox', { name: '任务目标' });
    await user.clear(objective);
    await user.type(objective, '整理并归档项目目录');
    await user.click(screen.getByRole('button', { name: '保存计划' }));
    await waitFor(() => expect(update).toHaveBeenCalledWith(schedule.id, expect.objectContaining({ expected_version: 3, objective: '整理并归档项目目录' })));

    await user.click(screen.getByRole('button', { name: '运行记录' }));
    expect(await screen.findByRole('link', { name: '查看任务' })).toHaveAttribute('href', '/tasks/q1234567');
  });

  it('explains runs skipped after schedule authorization ends', async () => {
    vi.mocked(portalApi.schedules).mockResolvedValue({ items: [schedule], next_cursor: null });
    vi.spyOn(portalApi, 'scheduleRuns').mockResolvedValue({ items: [{ ...run, status: 'skipped', skip_reason: 'authorization_ended', task_id: null }], next_cursor: null });
    const user = userEvent.setup();
    renderPanel();
    await user.click(await screen.findByRole('button', { name: '运行记录' }));
    expect(await screen.findByText(/执行授权已失效/)).toBeInTheDocument();
  });

  it('requests cancellation explicitly and reports it as pending until confirmed', async () => {
    const pendingSchedule = { ...schedule, status: 'disabled' as const, version: 4, last_run: { ...run, status: 'stopping' as const } };
    vi.mocked(portalApi.schedules).mockResolvedValueOnce({ items: [schedule], next_cursor: null }).mockResolvedValue({ items: [pendingSchedule], next_cursor: null });
    const action = vi.spyOn(portalApi, 'scheduleAction').mockResolvedValue({ schedule: pendingSchedule, current_run: { ...run, status: 'stopping' } });
    const user = userEvent.setup();
    renderPanel();
    await user.click(await screen.findByRole('button', { name: '停用' }));
    const dialog = screen.getByRole('dialog');
    expect(within(dialog).getByText(/当前这次执行正在执行中/)).toBeInTheDocument();
    await user.click(within(dialog).getByRole('button', { name: '停用并请求取消' }));

    await waitFor(() => expect(action).toHaveBeenCalledWith(schedule.id, { action: 'disable', expected_version: schedule.version, current_run_action: 'cancel' }));
    expect(await screen.findByText('计划已停用；当前任务的取消仍在确认中。')).toBeInTheDocument();
    expect(screen.getByText(/仍在等待任务停止确认/)).toBeInTheDocument();
  });

  it('does not hide a version conflict after an edit', async () => {
    vi.mocked(portalApi.schedules).mockResolvedValue({ items: [schedule], next_cursor: null });
    vi.spyOn(portalApi, 'updateSchedule').mockRejectedValue(new ApiError({ message: '计划版本已变化，请刷新后重试', code: 'conflict', status: 409 }));
    const user = userEvent.setup();
    renderPanel();
    await user.click(await screen.findByRole('button', { name: '编辑' }));
    await user.clear(screen.getByRole('textbox', { name: '任务目标' }));
    await user.type(screen.getByRole('textbox', { name: '任务目标' }), '修改后的目标');
    await user.click(screen.getByRole('button', { name: '保存计划' }));

    expect(await screen.findByRole('alert')).toHaveTextContent(/计划版本已变化/);
    expect(screen.getByRole('textbox', { name: '任务目标' })).toHaveValue('修改后的目标');
  });
});
