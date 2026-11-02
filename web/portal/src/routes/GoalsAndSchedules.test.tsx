import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { components } from '../api/schema';
import { api, ApiError } from '../api/client';
import { portalApi } from '../app/api';
import { GoalsAndSchedules } from './GoalsAndSchedules';

const goal: components['schemas']['PersonalObject'] = {
  object_type: 'goal', object_id: 'goal_1', revision: 4,
  body: { title: '完成迁移', description: '整理旧数据', status: 'active', steps: [], related_material_ids: [] },
};

const schedule: components['schemas']['Schedule'] = {
  id: 'j1234567', version: 2, status: 'enabled', host_id: 'h1234567', model_id: 'model-1', objective: '检查日报',
  timing: { kind: 'daily', starts_on: '2026-11-02', local_time: '09:00', time_zone: 'Asia/Shanghai' },
  budget: { active_seconds: 300, tool_calls: 12, model_tokens: 9000 }, misfire_policy: 'skip',
  notification_events: ['completed', 'failed'], observation: null, next_run_at: '2026-11-03T01:00:00Z',
  last_run: null, current_run: null, created_at: '2026-11-01T00:00:00Z', updated_at: '2026-11-02T00:00:00Z',
};

describe('GoalsAndSchedules', () => {
  afterEach(() => { cleanup(); vi.restoreAllMocks(); });

  function renderPage() {
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
    return render(<QueryClientProvider client={queryClient}><GoalsAndSchedules /></QueryClientProvider>);
  }

  function mockAvailableModels() {
    vi.spyOn(api, 'GET').mockResolvedValue({
      data: { items: [{ id: 'model-1', name: '团队模型', model_name: 'gpt-test', capabilities: { text: true, tools: true, vision: false } }] },
      response: new Response(null, { status: 200 }),
    } as never);
  }

  function mockEmptyGoalsAndSchedules() {
    vi.spyOn(portalApi, 'goals').mockResolvedValue({ items: [], next_cursor: null });
    vi.spyOn(portalApi, 'schedules').mockResolvedValue({ items: [], next_cursor: '' });
    vi.spyOn(portalApi, 'hosts').mockResolvedValue({ items: [{
      id: 'h1234567', name: '个人主机', owner_id: 'u1234567', template_id: 't1', isolation: 'container', state: 'running',
      state_reason: '', runtime_generation: 1, control_version: 1, control_state: 'ai', url: 'https://host.test', workspaces: [],
      resources: { cpu_milli: 1000, mem_mib: 1024, cpu_limit_milli: 1000, mem_limit_mib: 1024 }, expires_at: null, last_active_at: null, created_at: '2026-11-01T00:00:00Z',
    }] });
    mockAvailableModels();
  }

  it('shows cloud goals and schedule states and explains that legacy local data is untouched', async () => {
    vi.spyOn(portalApi, 'goals').mockResolvedValue({ items: [goal], next_cursor: null });
    vi.spyOn(portalApi, 'schedules').mockResolvedValue({ items: [schedule], next_cursor: '' });
    vi.spyOn(portalApi, 'hosts').mockResolvedValue({ items: [] });
    mockAvailableModels();
    renderPage();

    expect(await screen.findByRole('heading', { level: 3, name: '完成迁移' })).toBeInTheDocument();
    expect(screen.getByText(/旧 nanoMuse 本机目标和计划不会由此页自动读取、导入、启用或删除/)).toBeInTheDocument();
    expect(screen.getByRole('heading', { level: 3, name: '检查日报' })).toBeInTheDocument();
    expect(screen.getByText('版本 2')).toBeInTheDocument();
    expect(screen.getByText('每天 09:00 (Asia/Shanghai) · 主机：h1234567 · 模型：团队模型')).toBeInTheDocument();
  });

  it('updates a goal using the latest server revision and preserves its existing fields', async () => {
    const user = userEvent.setup();
    vi.spyOn(portalApi, 'goals').mockResolvedValue({ items: [goal], next_cursor: null });
    vi.spyOn(portalApi, 'schedules').mockResolvedValue({ items: [], next_cursor: '' });
    vi.spyOn(portalApi, 'getGoal').mockResolvedValue(goal);
    const update = vi.spyOn(portalApi, 'updateGoal').mockResolvedValue({ object_type: 'goal', object_id: 'goal_1', result_revision: 5, account_change_sequence: 7, replay: false });
    vi.spyOn(portalApi, 'hosts').mockResolvedValue({ items: [] });
    mockAvailableModels();
    renderPage();

    await user.click(await screen.findByRole('button', { name: '编辑' }));
    const title = await screen.findByLabelText('目标名称');
    await user.clear(title);
    await user.type(title, '完成云端迁移');
    await user.click(screen.getByRole('button', { name: '保存目标' }));

    await waitFor(() => expect(update).toHaveBeenCalledWith('goal_1', {
      base_revision: 4,
      body: { title: '完成云端迁移', description: '整理旧数据', status: 'active', steps: [], related_material_ids: [] },
    }));
  });

  it('creates a goal only after explicit form submission', async () => {
    const user = userEvent.setup();
    mockEmptyGoalsAndSchedules();
    const create = vi.spyOn(portalApi, 'createGoal').mockResolvedValue({ object_type: 'goal', object_id: 'goal_new', result_revision: 1, account_change_sequence: 1, replay: false });
    renderPage();

    await user.click(await screen.findByRole('button', { name: '＋ 新建目标' }));
    await user.type(screen.getByLabelText('目标名称'), '每周复盘');
    await user.type(screen.getByLabelText('说明'), '每周五总结进度');
    await user.click(screen.getByRole('button', { name: '保存目标' }));

    await waitFor(() => expect(create).toHaveBeenCalledWith(expect.objectContaining({
      id: expect.any(String),
      body: { title: '每周复盘', description: '每周五总结进度', status: 'active', steps: [], related_material_ids: [] },
    })));
  });

  it('creates an enabled cloud schedule through the Portal session API', async () => {
    const user = userEvent.setup();
    mockEmptyGoalsAndSchedules();
    const create = vi.spyOn(portalApi, 'createSchedule').mockResolvedValue(schedule);
    renderPage();

    await user.click(await screen.findByRole('button', { name: '＋ 新建云端计划' }));
    await user.type(screen.getByLabelText('计划内容'), '检查工作日进度');
    await user.click(screen.getByRole('button', { name: '创建并启用' }));

    await waitFor(() => expect(create).toHaveBeenCalledWith(expect.objectContaining({
      host_id: 'h1234567', model_id: 'model-1', objective: '检查工作日进度',
      timing: expect.objectContaining({ kind: 'daily', time_zone: expect.any(String) }),
      budget: { active_seconds: 900, tool_calls: 20, model_tokens: 10000 },
    })));
  });

  it('creates an every-N-hours schedule using elapsed-hour timing', async () => {
    const user = userEvent.setup();
    mockEmptyGoalsAndSchedules();
    const create = vi.spyOn(portalApi, 'createSchedule').mockResolvedValue({
      ...schedule,
      timing: { kind: 'every_n_hours', interval_hours: 6, time_zone: 'Asia/Shanghai' },
    });
    renderPage();

    await user.click(await screen.findByRole('button', { name: '＋ 新建云端计划' }));
    await user.type(screen.getByLabelText('计划内容'), '每六小时检查一次');
    await user.click(screen.getByRole('radio', { name: '每 N 小时' }));
    await user.clear(screen.getByLabelText('间隔小时'));
    await user.type(screen.getByLabelText('间隔小时'), '6');
    await user.click(screen.getByRole('button', { name: '创建并启用' }));

    await waitFor(() => expect(create).toHaveBeenCalledWith(expect.objectContaining({
      objective: '每六小时检查一次',
      timing: { kind: 'every_n_hours', interval_hours: 6, time_zone: expect.any(String) },
    })));
  });

  it('updates the current schedule version and sends version-checked actions', async () => {
    const user = userEvent.setup();
    vi.spyOn(portalApi, 'goals').mockResolvedValue({ items: [], next_cursor: null });
    vi.spyOn(portalApi, 'schedules').mockResolvedValue({ items: [schedule], next_cursor: '' });
    vi.spyOn(portalApi, 'hosts').mockResolvedValue({ items: [] });
    vi.spyOn(portalApi, 'getSchedule').mockResolvedValue(schedule);
    const update = vi.spyOn(portalApi, 'updateSchedule').mockResolvedValue({ ...schedule, version: 3, objective: '检查新日报' });
    const action = vi.spyOn(portalApi, 'scheduleAction').mockResolvedValue({ schedule: { ...schedule, status: 'disabled', version: 3 }, current_run: null });
    mockAvailableModels();
    renderPage();

    await user.click(await screen.findByRole('button', { name: '编辑版本' }));
    const objective = await screen.findByLabelText('计划内容');
    await user.clear(objective);
    await user.type(objective, '检查新日报');
    await user.click(screen.getByRole('button', { name: '保存新版本' }));
    await waitFor(() => expect(update).toHaveBeenCalledWith(schedule.id, expect.objectContaining({ expected_version: 2, objective: '检查新日报' })));

    await user.click(await screen.findByRole('button', { name: '停用' }));
    await waitFor(() => expect(action).toHaveBeenCalledWith(schedule.id, { action: 'disable', expected_version: 2 }));
  });

  it('locks schedule writes after a server permission denial without requesting another credential', async () => {
    const user = userEvent.setup();
    mockEmptyGoalsAndSchedules();
    vi.spyOn(portalApi, 'createSchedule').mockRejectedValue(new ApiError({ status: 403, code: 'forbidden', message: '计划管理权限不足' }));
    renderPage();

    await user.click(await screen.findByRole('button', { name: '＋ 新建云端计划' }));
    await user.type(screen.getByLabelText('计划内容'), '安全检查');
    await user.click(screen.getByRole('button', { name: '创建并启用' }));

    expect(await screen.findByText(/不会请求或保存额外授权凭据/)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: '＋ 新建云端计划' })).toBeDisabled();
  });

  it('loads schedule run history when requested', async () => {
    const user = userEvent.setup();
    vi.spyOn(portalApi, 'goals').mockResolvedValue({ items: [], next_cursor: null });
    vi.spyOn(portalApi, 'schedules').mockResolvedValue({ items: [schedule], next_cursor: '' });
    vi.spyOn(portalApi, 'scheduleRuns').mockResolvedValue({ items: [{
      id: 'y1234567', schedule_id: schedule.id, schedule_version: 2, occurrence_key: 'occurrence-1', scheduled_at: schedule.next_run_at,
      coalesced_count: 0, status: 'completed', skip_reason: null, task_id: 'task-1', notification_events: ['completed'], observation: null,
      observation_outcome: 'unknown', created_at: '2026-11-02T01:00:00Z', updated_at: '2026-11-02T01:05:00Z', finished_at: '2026-11-02T01:05:00Z',
    }], next_cursor: '' });
    vi.spyOn(portalApi, 'hosts').mockResolvedValue({ items: [] });
    mockAvailableModels();
    renderPage();

    await user.click(await screen.findByRole('button', { name: '运行记录' }));
    expect(await screen.findByText(/已完成/)).toBeInTheDocument();
    expect(screen.getByText(/计划版本 2 · 任务 task-1/)).toBeInTheDocument();
  });
});
