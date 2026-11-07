import { useState } from 'react';
import type { FormEvent } from 'react';
import { useInfiniteQuery, useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import type { components } from '../api/schema';
import { api, unwrapApiResponse } from '../api/client';
import { errorDetails, portalApi } from '../app/api';

type GoalBody = components['schemas']['GoalBody'];
type GoalStep = components['schemas']['GoalStep'];
type PersonalObject = components['schemas']['PersonalObject'];
type GoalRecord = Omit<PersonalObject, 'body' | 'object_type'> & { object_type: 'goal'; body: GoalBody };
type Schedule = components['schemas']['Schedule'];
type ScheduleRun = components['schemas']['ScheduleRun'];
type ScheduleCreateRequest = components['schemas']['ScheduleCreateRequest'];
type ScheduleUpdateRequest = components['schemas']['ScheduleUpdateRequest'];
type ScheduleFields = Omit<ScheduleCreateRequest, 'observation'>;
type Host = Awaited<ReturnType<typeof portalApi.hosts>>['items'][number];
type AvailableModel = Awaited<ReturnType<typeof loadAvailableModels>>['items'][number];

async function loadAvailableModels() {
  return unwrapApiResponse(await api.GET('/api/available-models'));
}

function makeID(prefix: string) {
  const uuid = globalThis.crypto?.randomUUID?.();
  return uuid ? `${prefix}_${uuid}` : `${prefix}_${Date.now()}_${Math.random().toString(36).slice(2)}`;
}

function toGoalRecord(value: PersonalObject): GoalRecord | null {
  if (value.object_type !== 'goal') return null;
  return { ...value, object_type: 'goal', body: value.body as GoalBody };
}

function forbidden(error: unknown) {
  return typeof error === 'object' && error !== null && 'status' in error && (error as { status?: unknown }).status === 403;
}

function statusLabel(status: GoalBody['status']) {
  return ({ active: '进行中', paused: '已暂停', done: '已完成', cancelled: '已取消' })[status];
}

function scheduleStatusLabel(status: Schedule['status']) {
  return ({ enabled: '已启用', disabled: '已停用', completed: '已完成' })[status];
}

function runStatusLabel(status: ScheduleRun['status']) {
  return ({ queued: '排队中', running: '运行中', waiting_user: '等待你确认', pausing: '正在暂停', paused: '已暂停', stopping: '正在停止', completed: '已完成', failed: '失败', cancelled: '已取消', skipped: '已跳过', missed: '错过' })[status];
}

function skipReasonLabel(reason: NonNullable<ScheduleRun['skip_reason']>) {
  return ({ dst_gap: '夏令时跳过', overlap: '与上次运行重叠', authorization_ended: '授权已结束' })[reason];
}

function isActiveRun(run: ScheduleRun | null) {
  return run !== null && ['queued', 'running', 'waiting_user', 'pausing', 'paused', 'stopping'].includes(run.status);
}

export function GoalsAndSchedules() {
  const client = useQueryClient();
  const [goalFormOpen, setGoalFormOpen] = useState(false);
  const [expandedGoalID, setExpandedGoalID] = useState<string | null>(null);
  const [editingGoalID, setEditingGoalID] = useState<string | null>(null);
  const [deleteGoalID, setDeleteGoalID] = useState<string | null>(null);
  const [scheduleForm, setScheduleForm] = useState<{ mode: 'create' } | { mode: 'edit'; schedule: Schedule } | null>(null);
  const [expandedScheduleID, setExpandedScheduleID] = useState<string | null>(null);
  const [scheduleWriteDenied, setScheduleWriteDenied] = useState(false);
  const goals = useInfiniteQuery({
    queryKey: ['goals'],
    queryFn: ({ pageParam }) => portalApi.goals(pageParam),
    initialPageParam: undefined as string | undefined,
    getNextPageParam: (page) => page.next_cursor || undefined,
  });
  const schedules = useInfiniteQuery({
    queryKey: ['schedules'],
    queryFn: ({ pageParam }) => portalApi.schedules(pageParam ? { cursor: pageParam } : {}),
    initialPageParam: undefined as string | undefined,
    getNextPageParam: (page) => page.next_cursor || undefined,
  });
  const hosts = useQuery({ queryKey: ['hosts'], queryFn: portalApi.hosts });
  const models = useQuery({ queryKey: ['available-models'], queryFn: loadAvailableModels });
  const goalDetail = useQuery({
    queryKey: ['goals', 'detail', expandedGoalID],
    queryFn: () => portalApi.getGoal(expandedGoalID!),
    enabled: Boolean(expandedGoalID),
  });
  const runs = useInfiniteQuery({
    queryKey: ['schedules', expandedScheduleID, 'runs'],
    queryFn: ({ pageParam }) => portalApi.scheduleRuns(expandedScheduleID!, pageParam ? { cursor: pageParam } : {}),
    initialPageParam: undefined as string | undefined,
    getNextPageParam: (page) => page.next_cursor || undefined,
    enabled: Boolean(expandedScheduleID),
  });

  const goalCreate = useMutation({
    mutationFn: (body: GoalBody) => portalApi.createGoal({ id: makeID('goal'), body }),
    onSuccess: async () => {
      setGoalFormOpen(false);
      await client.invalidateQueries({ queryKey: ['goals'] });
    },
  });
  const goalUpdate = useMutation({
    mutationFn: async ({ id, body }: { id: string; body: GoalBody }) => {
      const latest = await portalApi.getGoal(id);
      return portalApi.updateGoal(id, { base_revision: latest.revision, body });
    },
    onSuccess: async (_receipt, variables) => {
      await Promise.all([
        client.invalidateQueries({ queryKey: ['goals'] }),
        client.invalidateQueries({ queryKey: ['goals', 'detail', variables.id] }),
      ]);
      setEditingGoalID(null);
    },
  });
  const goalDelete = useMutation({
    mutationFn: async (id: string) => {
      const latest = await portalApi.getGoal(id);
      return portalApi.deleteGoal(id, { base_revision: latest.revision });
    },
    onSuccess: async (_receipt, id) => {
      setDeleteGoalID(null);
      setExpandedGoalID((current) => current === id ? null : current);
      await client.invalidateQueries({ queryKey: ['goals'] });
    },
  });
  const goalStatus = useMutation({
    mutationFn: async ({ id, status }: { id: string; status: GoalBody['status'] }) => {
      const latest = await portalApi.getGoal(id);
      const body: GoalBody = { ...(latest.body as GoalBody), status };
      return portalApi.updateGoal(id, { base_revision: latest.revision, body });
    },
    onSuccess: async (_receipt, variables) => {
      await Promise.all([
        client.invalidateQueries({ queryKey: ['goals'] }),
        client.invalidateQueries({ queryKey: ['goals', 'detail', variables.id] }),
      ]);
    },
  });

  const scheduleCreate = useMutation({
    mutationFn: (body: ScheduleCreateRequest) => portalApi.createSchedule(body),
    onSuccess: async () => {
      setScheduleForm(null);
      await client.invalidateQueries({ queryKey: ['schedules'] });
    },
    onError: (error) => { if (forbidden(error)) setScheduleWriteDenied(true); },
  });
  const scheduleUpdate = useMutation({
    mutationFn: async ({ id, fields }: { id: string; fields: ScheduleFields }) => {
      const latest = await portalApi.getSchedule(id);
      const body: ScheduleUpdateRequest = { expected_version: latest.version, ...fields };
      return portalApi.updateSchedule(id, body);
    },
    onSuccess: async () => {
      setScheduleForm(null);
      await client.invalidateQueries({ queryKey: ['schedules'] });
    },
    onError: (error) => { if (forbidden(error)) setScheduleWriteDenied(true); },
  });
  const scheduleAction = useMutation({
    mutationFn: async ({ id, action, currentRunAction }: { id: string; action: 'enable' | 'disable'; currentRunAction?: 'continue' | 'cancel' }) => {
      const latest = await portalApi.getSchedule(id);
      return portalApi.scheduleAction(id, {
        action,
        expected_version: latest.version,
        ...(currentRunAction ? { current_run_action: currentRunAction } : {}),
      });
    },
    onSuccess: async (_result, variables) => {
      await Promise.all([
        client.invalidateQueries({ queryKey: ['schedules'] }),
        client.invalidateQueries({ queryKey: ['schedules', variables.id, 'runs'] }),
      ]);
    },
    onError: (error) => { if (forbidden(error)) setScheduleWriteDenied(true); },
  });

  const goalRecords = (goals.data?.pages.flatMap((page) => page.items.map(toGoalRecord).filter((item): item is GoalRecord => item !== null)) ?? []);
  const scheduleRecords = schedules.data?.pages.flatMap((page) => page.items) ?? [];
  const hostItems = hosts.data?.items ?? [];
  const modelItems = models.data?.items ?? [];
  const detail = goalDetail.data ? toGoalRecord(goalDetail.data) : null;
  const scheduleWriteError = scheduleCreate.error ?? scheduleUpdate.error ?? scheduleAction.error;
  const goalWriteError = goalCreate.error ?? goalUpdate.error ?? goalDelete.error ?? goalStatus.error;

  return <section className="goals-grid">
    <section className="connection-section" aria-labelledby="goals-title">
      <div className="connection-section-heading">
        <div><h2 id="goals-title">目标</h2><p className="muted">目标状态和修订版本保存在你的个人云端数据中。</p></div>
        <button type="button" className="button button-primary" onClick={() => setGoalFormOpen((open) => !open)}>{goalFormOpen ? '收起' : '＋ 新建目标'}</button>
      </div>
      {goalFormOpen && <GoalEditorForm
        busy={goalCreate.isPending}
        onCancel={() => { setGoalFormOpen(false); goalCreate.reset(); }}
        onSubmit={(body) => goalCreate.mutate(body)}
      />}
      {goalWriteError && <div className="error-card" role="alert">{errorDetails(goalWriteError).message}</div>}
      {goals.isPending ? <div className="loading-card" role="status">正在读取目标…</div>
        : goals.isError ? <div className="error-card" role="alert">{errorDetails(goals.error).message}<button type="button" className="button button-secondary" onClick={() => void goals.refetch()}>重试</button></div>
          : goalRecords.length === 0 ? <div className="empty-card"><h3>还没有云端目标</h3><p>创建后可随时更新状态或删除目标。</p></div>
            : <div className="schedule-list">{goalRecords.map((goal) => <article className="schedule-card" key={goal.object_id}>
              <div className="schedule-card-heading"><div><h3>{goal.body.title}</h3><span className={`status ${goal.body.status === 'done' ? 'status-ready' : goal.body.status === 'active' ? 'status-running' : ''}`}>{statusLabel(goal.body.status)}</span></div><span className="muted">修订 {goal.revision}</span></div>
              {goal.body.description && <p className="schedule-card-meta">{goal.body.description}</p>}
              {goal.body.steps.length > 0 && <ul>{goal.body.steps.map((step) => <li key={step.id}>{step.title} · {goalStepStatus(step.status)}</li>)}</ul>}
              <div className="schedule-actions">
                <button type="button" className="button button-secondary" onClick={() => { setExpandedGoalID((current) => current === goal.object_id ? null : goal.object_id); setEditingGoalID(null); }}>查看详情</button>
                <button type="button" className="button button-secondary" onClick={() => { setExpandedGoalID(goal.object_id); setEditingGoalID(goal.object_id); }}>编辑</button>
                {goal.body.status !== 'done' && goal.body.status !== 'cancelled' && <button type="button" className="button button-secondary" disabled={goalStatus.isPending} onClick={() => goalStatus.mutate({ id: goal.object_id, status: goal.body.status === 'paused' ? 'active' : 'paused' })}>{goal.body.status === 'paused' ? '恢复目标' : '暂停目标'}</button>}
                {goal.body.status !== 'done' && <button type="button" className="button button-secondary" disabled={goalStatus.isPending} onClick={() => goalStatus.mutate({ id: goal.object_id, status: 'done' })}>标记完成</button>}
                {deleteGoalID === goal.object_id ? <><button type="button" className="button button-danger" disabled={goalDelete.isPending} onClick={() => goalDelete.mutate(goal.object_id)}>确认删除</button><button type="button" className="button button-secondary" onClick={() => setDeleteGoalID(null)}>取消</button></> : <button type="button" className="button button-danger-quiet" onClick={() => setDeleteGoalID(goal.object_id)}>删除</button>}
              </div>
              {expandedGoalID === goal.object_id && <div className="schedule-runs">
                {goalDetail.isPending ? <p role="status">正在读取目标详情…</p>
                  : goalDetail.isError ? <p role="alert">{errorDetails(goalDetail.error).message}<button type="button" className="button button-secondary" onClick={() => void goalDetail.refetch()}>重试</button></p>
                    : detail && <>
                      <p><strong>当前状态：</strong>{statusLabel(detail.body.status)} · <strong>服务端修订：</strong>{detail.revision}</p>
                      {editingGoalID === goal.object_id ? <GoalEditorForm
                        initial={detail.body}
                        busy={goalUpdate.isPending}
                        onCancel={() => setEditingGoalID(null)}
                        onSubmit={(body) => goalUpdate.mutate({ id: detail.object_id, body })}
                      /> : <p>{detail.body.description || '此目标没有补充说明。'}</p>}
                    </>}
              </div>}
            </article>)}</div>}
      {goals.hasNextPage && <button type="button" className="button button-secondary schedule-load-more" disabled={goals.isFetchingNextPage} onClick={() => void goals.fetchNextPage()}>{goals.isFetchingNextPage ? '正在加载…' : '加载更多目标'}</button>}
    </section>

    <section className="connection-section" aria-labelledby="schedules-title">
      <div className="connection-section-heading">
        <div><h2 id="schedules-title">定时计划</h2><p className="muted">查看计划版本、运行状态和历史记录。</p></div>
        <button type="button" className="button button-primary" disabled={scheduleWriteDenied || hosts.isPending || models.isPending || !modelItems.length} onClick={() => setScheduleForm({ mode: 'create' })}>＋ 新建云端计划</button>
      </div>
      {scheduleWriteDenied && <p className="notice notice-warning" role="alert">服务端拒绝了计划写操作（403）。此页已切换为只读状态，不会请求或保存额外授权凭据。</p>}
      {scheduleWriteError && <div className="error-card" role="alert">{errorDetails(scheduleWriteError).message}</div>}
      {scheduleForm && <ScheduleEditor
        key={scheduleForm.mode === 'edit' ? scheduleForm.schedule.id : 'new-schedule'}
        initial={scheduleForm.mode === 'edit' ? scheduleForm.schedule : null}
        hosts={hostItems}
        models={modelItems}
        busy={scheduleCreate.isPending || scheduleUpdate.isPending}
        onCancel={() => setScheduleForm(null)}
        onSubmit={(fields) => scheduleForm.mode === 'create'
          ? scheduleCreate.mutate(fields)
          : scheduleUpdate.mutate({ id: scheduleForm.schedule.id, fields })}
      />}
      {(hosts.isError || models.isError) && <div className="error-card" role="alert">
        <span>{hosts.isError ? `无法读取可用主机：${errorDetails(hosts.error).message}` : `无法读取可用模型：${errorDetails(models.error).message}`}</span>
        <button type="button" className="button button-secondary" onClick={() => { if (hosts.isError) void hosts.refetch(); if (models.isError) void models.refetch(); }}>重试</button>
      </div>}
      {!hosts.isPending && !hosts.isError && !hostItems.length && <p className="notice notice-warning">当前没有可访问的主机。可以创建仅文本计划；使用浏览器观察时必须选择 Host。</p>}
      {!models.isPending && !models.isError && !modelItems.length && <p className="notice notice-warning">当前没有可用模型。创建计划需要先配置模型。</p>}
      {schedules.isPending ? <div className="loading-card" role="status">正在读取定时计划…</div>
        : schedules.isError ? <div className="error-card" role="alert">{errorDetails(schedules.error).message}<button type="button" className="button button-secondary" onClick={() => void schedules.refetch()}>重试</button></div>
          : scheduleRecords.length === 0 ? <div className="empty-card"><h3>还没有云端计划</h3><p>只有通过上方按钮提交创建后，计划才会写入云端。</p></div>
            : <div className="schedule-list">{scheduleRecords.map((schedule) => {
              const currentRunActive = isActiveRun(schedule.current_run);
              const scheduleRuns = expandedScheduleID === schedule.id ? runs.data?.pages.flatMap((page) => page.items) ?? [] : [];
              return <article className="schedule-card" key={schedule.id}>
                <div className="schedule-card-heading"><div><h3>{schedule.title}</h3><span className={`status ${schedule.status === 'enabled' ? 'status-running' : schedule.status === 'completed' ? 'status-ready' : ''}`}>{scheduleStatusLabel(schedule.status)}</span></div><span className="muted">版本 {schedule.version}</span></div>
                <p className="schedule-card-meta">执行内容：{schedule.objective}</p>
                <p className="schedule-card-meta">{timingLabel(schedule.timing)} · 主机：{schedule.host_id ? hostItems.find((host) => host.id === schedule.host_id)?.name ?? schedule.host_id : '无（文本计划）'} · 模型：{modelItems.find((model) => model.id === schedule.model_id)?.name ?? schedule.model_id}</p>
                <dl className="schedule-facts"><div><dt>下次运行</dt><dd>{schedule.next_run_at ? formatDate(schedule.next_run_at) : '无'}</dd></div><div><dt>当前运行</dt><dd>{schedule.current_run ? runStatusLabel(schedule.current_run.status) : '未运行'}</dd></div><div><dt>上次运行</dt><dd>{schedule.last_run ? `${runStatusLabel(schedule.last_run.status)} · ${formatDate(schedule.last_run.updated_at)}` : '尚无记录'}</dd></div><div><dt>运行预算</dt><dd>{schedule.budget.active_seconds} 秒 · {schedule.budget.tool_calls} 次工具调用 · {schedule.budget.model_tokens.toLocaleString()} tokens</dd></div></dl>
                <div className="schedule-actions">
                  {!scheduleWriteDenied && schedule.status !== 'completed' && !(schedule.status === 'enabled' && currentRunActive) && <button type="button" className="button button-secondary" disabled={scheduleAction.isPending} onClick={() => scheduleAction.mutate({ id: schedule.id, action: schedule.status === 'enabled' ? 'disable' : 'enable' })}>{schedule.status === 'enabled' ? '停用' : '启用'}</button>}
                  {!scheduleWriteDenied && schedule.status !== 'completed' && currentRunActive && schedule.status === 'enabled' && <>
                    <button type="button" className="button button-secondary" disabled={scheduleAction.isPending} onClick={() => scheduleAction.mutate({ id: schedule.id, action: 'disable', currentRunAction: 'continue' })}>停用并继续当前运行</button>
                    <button type="button" className="button button-danger-quiet" disabled={scheduleAction.isPending} onClick={() => scheduleAction.mutate({ id: schedule.id, action: 'disable', currentRunAction: 'cancel' })}>停用并取消当前运行</button>
                  </>}
                  {!scheduleWriteDenied && <button type="button" className="button button-secondary" onClick={() => setScheduleForm({ mode: 'edit', schedule })}>编辑版本</button>}
                  <button type="button" className="button button-secondary" onClick={() => setExpandedScheduleID((current) => current === schedule.id ? null : schedule.id)}>{expandedScheduleID === schedule.id ? '收起运行记录' : '运行记录'}</button>
                </div>
                {expandedScheduleID === schedule.id && <div className="schedule-runs">
                  {runs.isPending ? <p role="status">正在读取运行记录…</p>
                    : runs.isError ? <p role="alert">{errorDetails(runs.error).message}<button type="button" className="button button-secondary" onClick={() => void runs.refetch()}>重试</button></p>
                      : scheduleRuns.length ? <ul>{scheduleRuns.map((run) => <li key={run.id}><span>{run.scheduled_at ? formatDate(run.scheduled_at) : formatDate(run.created_at)} · {runStatusLabel(run.status)}{run.skip_reason ? ` · ${skipReasonLabel(run.skip_reason)}` : ''}</span><span>计划版本 {run.schedule_version}{run.task_id ? ` · 任务 ${run.task_id}` : ''}</span></li>)}</ul>
                        : <p>还没有运行记录。</p>}
                  {runs.hasNextPage && <button type="button" className="button button-secondary" disabled={runs.isFetchingNextPage} onClick={() => void runs.fetchNextPage()}>{runs.isFetchingNextPage ? '正在加载…' : '加载更多运行记录'}</button>}
                </div>}
              </article>;
            })}</div>}
      {schedules.hasNextPage && <button type="button" className="button button-secondary schedule-load-more" disabled={schedules.isFetchingNextPage} onClick={() => void schedules.fetchNextPage()}>{schedules.isFetchingNextPage ? '正在加载…' : '加载更多计划'}</button>}
    </section>
  </section>;
}

function GoalEditorForm({ initial, busy, onCancel, onSubmit }: { initial?: GoalBody; busy: boolean; onCancel: () => void; onSubmit: (body: GoalBody) => void }) {
  const [title, setTitle] = useState(initial?.title ?? '');
  const [description, setDescription] = useState(initial?.description ?? '');
  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const body: GoalBody = initial
      ? { ...initial, title: title.trim(), description: description.trim() }
      : { title: title.trim(), description: description.trim(), status: 'active', steps: [], related_material_ids: [] };
    onSubmit(body);
  };
  return <form className="form-card form-stack schedule-editor" onSubmit={submit}>
    <h3>{initial ? '编辑云端目标' : '新建云端目标'}</h3>
    <label htmlFor="goal-title">目标名称</label><input id="goal-title" value={title} maxLength={200} required onChange={(event) => setTitle(event.target.value)} />
    <label htmlFor="goal-description">说明</label><textarea id="goal-description" value={description} maxLength={8000} rows={3} onChange={(event) => setDescription(event.target.value)} />
    <div className="form-actions"><button type="button" className="button button-secondary" onClick={onCancel}>取消</button><button type="submit" className="button button-primary" disabled={busy}>{busy ? '保存中…' : '保存目标'}</button></div>
  </form>;
}

function ScheduleEditor({ initial, hosts, models, busy, onCancel, onSubmit }: {
  initial: Schedule | null;
  hosts: Host[];
  models: AvailableModel[];
  busy: boolean;
  onCancel: () => void;
  onSubmit: (fields: ScheduleFields) => void;
}) {
  const timezone = Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC';
  const [hostID, setHostID] = useState(initial?.host_id ?? '');
  const [modelID, setModelID] = useState(initial?.model_id ?? models[0]?.id ?? '');
  const [objective, setObjective] = useState(initial?.objective ?? '');
  const [kind, setKind] = useState<components['schemas']['ScheduleTiming']['kind']>(initial?.timing.kind ?? 'daily');
  const [runAt, setRunAt] = useState(initial?.timing.kind === 'once' ? toDateTimeLocal(initial.timing.run_at, initial.timing.time_zone) : defaultDateTimeLocal());
  const [startsOn, setStartsOn] = useState(initial?.timing.kind === 'daily' || initial?.timing.kind === 'weekly' ? initial.timing.starts_on : localDate());
  const [localTime, setLocalTime] = useState(initial?.timing.kind === 'daily' || initial?.timing.kind === 'weekly' ? initial.timing.local_time : '09:00');
  const [timeZone, setTimeZone] = useState(initial?.timing.time_zone ?? timezone);
  const [weekdays, setWeekdays] = useState<number[]>(initial?.timing.kind === 'weekly' ? initial.timing.weekdays : [1, 2, 3, 4, 5]);
  const [intervalHours, setIntervalHours] = useState(initial?.timing.kind === 'every_n_hours' ? initial.timing.interval_hours : 24);
  const [activeSeconds, setActiveSeconds] = useState(initial?.budget.active_seconds ?? 900);
  const [toolCalls, setToolCalls] = useState(initial?.budget.tool_calls ?? 20);
  const [modelTokens, setModelTokens] = useState(initial?.budget.model_tokens ?? 10000);
  const [misfirePolicy, setMisfirePolicy] = useState<components['schemas']['ScheduleMisfirePolicy']>(initial?.misfire_policy ?? 'skip');
  const [notificationEvents, setNotificationEvents] = useState<components['schemas']['ScheduleNotificationEvent'][]>(initial?.notification_events ?? ['completed', 'failed', 'waiting_user']);
  const [formError, setFormError] = useState('');

  const toggleWeekday = (day: number) => setWeekdays((current) => current.includes(day) ? current.filter((value) => value !== day) : [...current, day].sort((a, b) => a - b));
  const toggleNotification = (event: components['schemas']['ScheduleNotificationEvent']) => setNotificationEvents((current) => current.includes(event) ? current.filter((value) => value !== event) : [...current, event]);
  const selectedHostExists = hostID === '' ? !initial?.observation : hosts.some((host) => host.id === hostID) || initial?.host_id === hostID;
  const selectedModelExists = models.some((model) => model.id === modelID) || (initial?.model_id === modelID);
  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    try {
      const timing: components['schemas']['ScheduleTiming'] = kind === 'once'
        ? { kind, run_at: zonedDateTimeToISO(runAt, timeZone), time_zone: timeZone }
        : kind === 'daily'
          ? { kind, starts_on: startsOn, local_time: localTime, time_zone: timeZone }
          : kind === 'weekly'
            ? { kind, starts_on: startsOn, local_time: localTime, time_zone: timeZone, weekdays }
            : {
              kind, interval_hours: Number(intervalHours), time_zone: timeZone,
              ...(initial?.timing.kind === 'every_n_hours' && initial.timing.anchor_at !== undefined ? { anchor_at: initial.timing.anchor_at } : {}),
            };
      setFormError('');
      onSubmit({
        host_id: hostID || null,
        model_id: modelID,
        objective: objective.trim(),
        timing,
        budget: { active_seconds: Number(activeSeconds), tool_calls: Number(toolCalls), model_tokens: Number(modelTokens) },
        misfire_policy: misfirePolicy,
        notification_events: notificationEvents,
      });
    } catch (error) {
      setFormError(error instanceof Error ? error.message : '运行时间无效，请检查日期、时间和时区。');
    }
  };

  return <form className="form-card form-stack schedule-editor" onSubmit={submit}>
    <h3>{initial ? `编辑计划定义 · 版本 ${initial.version}` : '新建云端计划'}</h3>
    <div className="schedule-form-grid">
      <label>主机<select value={hostID} onChange={(event) => setHostID(event.target.value)}>{!initial?.observation && <option value="">仅文本计划（无主机）</option>}{initial?.host_id && !hosts.some((host) => host.id === initial.host_id) && <option value={initial.host_id}>{initial.host_id}</option>}{hosts.map((host) => <option key={host.id} value={host.id}>{host.name} · {host.state}</option>)}</select></label>
      <label>模型<select value={modelID} required onChange={(event) => setModelID(event.target.value)}>{initial && !models.some((model) => model.id === initial.model_id) && <option value={initial.model_id}>{initial.model_id}</option>}{models.map((model) => <option key={model.id} value={model.id}>{model.name} · {model.model_name}</option>)}</select></label>
      <label className="schedule-form-wide">计划内容<textarea value={objective} maxLength={32768} rows={3} required onChange={(event) => setObjective(event.target.value)} placeholder="描述计划运行时需要完成的工作" /></label>
    </div>
    <fieldset className="fieldset">
      <legend>运行频率</legend>
      <div className="schedule-kind-options">
        {(['once', 'daily', 'weekly', 'every_n_hours'] as const).map((value) => <label className="radio-row" key={value}><input type="radio" name="schedule-kind" checked={kind === value} onChange={() => setKind(value)} />{({ once: '一次', daily: '每天', weekly: '每周', every_n_hours: '每 N 小时' })[value]}</label>)}
      </div>
      {kind === 'once' ? <label>运行时间<input type="datetime-local" required value={runAt} onChange={(event) => setRunAt(event.target.value)} /></label> : kind === 'every_n_hours' ? <>
        <label>间隔小时<input type="number" min={1} max={168} required value={intervalHours} onChange={(event) => setIntervalHours(Number(event.target.value))} /></label>
        {initial?.timing.kind === 'every_n_hours' && initial.timing.anchor_at && <p className="muted schedule-form-wide">调度锚点：{initial.timing.anchor_at}（编辑时保留该相位）</p>}
      </> : <div className="schedule-form-grid">
        <label>开始日期<input type="date" required value={startsOn} onChange={(event) => setStartsOn(event.target.value)} /></label>
        <label>本地时间<input type="time" required value={localTime} onChange={(event) => setLocalTime(event.target.value)} /></label>
      </div>}
      <label>IANA 时区<input value={timeZone} maxLength={64} required onChange={(event) => setTimeZone(event.target.value)} /></label>
      {kind === 'weekly' && <div className="schedule-weekdays" aria-label="每周运行日">{[['一', 1], ['二', 2], ['三', 3], ['四', 4], ['五', 5], ['六', 6], ['日', 7]].map(([label, day]) => <label className="checkbox-row" key={day}><input type="checkbox" checked={weekdays.includes(Number(day))} onChange={() => toggleWeekday(Number(day))} />周{label}</label>)}</div>}
    </fieldset>
    <fieldset className="fieldset">
      <legend>运行预算</legend>
      <div className="schedule-form-grid schedule-budget-grid">
        <label>活动秒数<input type="number" min={1} max={1800} required value={activeSeconds} onChange={(event) => setActiveSeconds(Number(event.target.value))} /></label>
        <label>工具调用次数<input type="number" min={1} max={100} required value={toolCalls} onChange={(event) => setToolCalls(Number(event.target.value))} /></label>
        <label>模型 tokens<input type="number" min={1} max={50000} required value={modelTokens} onChange={(event) => setModelTokens(Number(event.target.value))} /></label>
      </div>
    </fieldset>
    <label>错过运行时间时<select value={misfirePolicy} onChange={(event) => setMisfirePolicy(event.target.value as typeof misfirePolicy)}><option value="skip">跳过</option><option value="run_once">补运行一次</option></select></label>
    <fieldset className="fieldset"><legend>通知事件</legend>{(['completed', 'failed', 'waiting_user'] as const).map((value) => <label className="checkbox-row" key={value}><input type="checkbox" checked={notificationEvents.includes(value)} onChange={() => toggleNotification(value)} />{({ completed: '运行完成', failed: '运行失败', waiting_user: '等待用户输入' })[value]}</label>)}</fieldset>
    {!initial && <p className="notice notice-warning">提交后会创建并启用这个云端计划。请检查主机、模型和触发时间。</p>}
    {formError && <p className="form-error" role="alert">{formError}</p>}
    <div className="form-actions"><button type="button" className="button button-secondary" onClick={onCancel}>取消</button><button type="submit" className="button button-primary" disabled={busy || !selectedHostExists || !selectedModelExists || (kind === 'weekly' && weekdays.length === 0)}>{busy ? '保存中…' : initial ? '保存新版本' : '创建并启用'}</button></div>
  </form>;
}

function goalStepStatus(status: GoalStep['status']) {
  return ({ pending: '待处理', in_progress: '进行中', done: '已完成', blocked: '受阻', skipped: '已跳过' })[status];
}

function timingLabel(timing: components['schemas']['ScheduleTiming']) {
  if (timing.kind === 'once') return `一次 · ${formatDate(timing.run_at)} (${timing.time_zone})`;
  if (timing.kind === 'every_n_hours') return `每 ${timing.interval_hours} 小时 (${timing.time_zone})${timing.anchor_at ? ` · 锚点 ${timing.anchor_at}` : ''}`;
  if (timing.kind === 'daily') return `每天 ${timing.local_time} (${timing.time_zone})`;
  return `每周 ${timing.weekdays.map((day) => `周${({ 1: '一', 2: '二', 3: '三', 4: '四', 5: '五', 6: '六', 7: '日' } as Record<number, string>)[day]}`).join('、')} ${timing.local_time} (${timing.time_zone})`;
}

function formatDate(value: string) {
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? value : date.toLocaleString();
}

function localDate() {
  const date = new Date();
  return `${date.getFullYear()}-${String(date.getMonth() + 1).padStart(2, '0')}-${String(date.getDate()).padStart(2, '0')}`;
}

function defaultDateTimeLocal() {
  const date = new Date(Date.now() + 60 * 60 * 1000);
  return `${date.getFullYear()}-${String(date.getMonth() + 1).padStart(2, '0')}-${String(date.getDate()).padStart(2, '0')}T${String(date.getHours()).padStart(2, '0')}:${String(date.getMinutes()).padStart(2, '0')}`;
}

function toDateTimeLocal(value: string, timeZone: string) {
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return defaultDateTimeLocal();
  const parts = zonedParts(date, timeZone);
  return `${parts.year}-${parts.month}-${parts.day}T${parts.hour}:${parts.minute}`;
}

function zonedDateTimeToISO(value: string, timeZone: string) {
  const match = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2})$/.exec(value);
  if (!match) throw new Error('请选择有效的运行时间。');
  const target = match.slice(1).map(Number);
  const [year, month, day, hour, minute] = target as [number, number, number, number, number];
  const targetUTC = Date.UTC(year, month - 1, day, hour, minute);
  let timestamp = targetUTC;
  for (let attempt = 0; attempt < 4; attempt++) {
    const parts = zonedParts(new Date(timestamp), timeZone);
    const representedUTC = Date.UTC(Number(parts.year), Number(parts.month) - 1, Number(parts.day), Number(parts.hour), Number(parts.minute));
    timestamp += targetUTC - representedUTC;
  }
  const resolved = zonedParts(new Date(timestamp), timeZone);
  const expected = [year, month, day, hour, minute].map((part) => String(part).padStart(2, '0'));
  const actual = [resolved.year, resolved.month, resolved.day, resolved.hour, resolved.minute];
  if (actual.some((part, index) => part !== expected[index])) {
    throw new Error('该本地时间在所选时区不存在，可能是夏令时切换导致。请选择其他时间。');
  }
  return new Date(timestamp).toISOString();
}

function zonedParts(date: Date, timeZone: string) {
  const parts = new Intl.DateTimeFormat('en-CA', {
    timeZone, year: 'numeric', month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', hourCycle: 'h23',
  }).formatToParts(date);
  return Object.fromEntries(parts.filter((part) => part.type !== 'literal').map((part) => [part.type, part.value])) as Record<'year' | 'month' | 'day' | 'hour' | 'minute', string>;
}
