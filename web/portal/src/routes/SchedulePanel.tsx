import { useState } from 'react';
import type { FormEvent } from 'react';
import { Link } from 'react-router';
import { useInfiniteQuery, useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import type { components } from '../api/schema';
import { ApiError } from '../api/client';
import { errorDetails, portalApi } from '../app/api';
import { formatDate } from '../app/format';
import { useToast } from '../app/toast';
import { Modal } from '../components/Modal';

type Schedule = components['schemas']['Schedule'];
type ScheduleRun = components['schemas']['ScheduleRun'];
type ScheduleTiming = components['schemas']['ScheduleTiming'];
type ScheduleEvent = components['schemas']['Notification']['kind'];
type ScheduleSave = Omit<components['schemas']['CreateScheduleRequest'], 'host_id' | 'observation'> & {
  host_id: string; observation: components['schemas']['ObservationSpec'] | null;
};

const weekdays = [
  { value: 1, label: '周一' }, { value: 2, label: '周二' }, { value: 3, label: '周三' },
  { value: 4, label: '周四' }, { value: 5, label: '周五' }, { value: 6, label: '周六' }, { value: 7, label: '周日' },
];
const scheduleStatusLabel: Record<Schedule['status'], string> = { enabled: '已启用', disabled: '已停用', completed: '已完成' };
const runStatusLabel: Record<ScheduleRun['status'], string> = {
  queued: '排队中', running: '执行中', waiting_user: '等待你处理', pausing: '正在暂停', paused: '已暂停', stopping: '正在确认停止',
  completed: '已完成', failed: '失败', cancelled: '已取消', skipped: '已跳过', missed: '已错过',
};
const eventLabel: Record<ScheduleEvent, string> = { completed: '任务完成', failed: '任务失败', waiting_user: '需要你处理' };
const activeRunStatuses = new Set<ScheduleRun['status']>(['queued', 'running', 'waiting_user', 'pausing', 'paused', 'stopping']);

export function SchedulePanel() {
  const client = useQueryClient();
  const { notify } = useToast();
  const [editing, setEditing] = useState<Schedule | null>(null);
  const [creating, setCreating] = useState(false);
  const [actionTarget, setActionTarget] = useState<Schedule | null>(null);
  const [actionError, setActionError] = useState('');
  const schedules = useInfiniteQuery({
    queryKey: ['schedules'], queryFn: ({ pageParam }) => portalApi.schedules(pageParam),
    initialPageParam: undefined as string | undefined,
    getNextPageParam: (page) => page.next_cursor ?? undefined,
    refetchInterval: (query) => query.state.data?.pages.some((page) => page.items.some((schedule) => schedule.last_run && activeRunStatuses.has(schedule.last_run.status))) ? 5000 : false,
  });
  const hosts = useQuery({ queryKey: ['hosts'], queryFn: portalApi.hosts });
  const models = useQuery({ queryKey: ['model-connections'], queryFn: portalApi.modelConnections });
  const onboarding = useQuery({ queryKey: ['onboarding-status'], queryFn: portalApi.onboardingStatus });
  const readyModelIDs = new Set(onboarding.data?.models.map((model) => model.id) ?? []);
  const usableModels = models.data?.items.filter((model) => readyModelIDs.has(model.id)) ?? [];
  const runningHosts = hosts.data?.items.filter((host) => host.state === 'running') ?? [];
  const scheduleItems = schedules.data?.pages.flatMap((page) => page.items) ?? [];
  const invalidateSchedules = () => Promise.all([
    client.invalidateQueries({ queryKey: ['schedules'] }),
    client.invalidateQueries({ queryKey: ['schedule-runs'] }),
  ]);
  const create = useMutation({
    mutationFn: (body: components['schemas']['CreateScheduleRequest']) => portalApi.createSchedule(body),
    onSuccess: async () => { setCreating(false); await invalidateSchedules(); notify('计划已创建并启用。', 'success'); },
    onError: (error) => { if (isConflict(error)) void invalidateSchedules(); },
  });
  const update = useMutation({
    mutationFn: ({ id, body }: { id: string; body: components['schemas']['UpdateScheduleRequest'] }) => portalApi.updateSchedule(id, body),
    onSuccess: async () => { setEditing(null); await invalidateSchedules(); notify('计划已保存。', 'success'); },
    onError: (error) => { if (isConflict(error)) void invalidateSchedules(); },
  });
  const action = useMutation({
    mutationFn: ({ schedule, body }: { schedule: Schedule; body: components['schemas']['ScheduleActionRequest'] }) => portalApi.scheduleAction(schedule.id, body),
    onSuccess: async (result) => {
      setActionTarget(null);
      setActionError('');
      await invalidateSchedules();
      if (result.schedule.status === 'disabled' && result.current_run?.status === 'stopping') notify('计划已停用；当前任务的取消仍在确认中。', 'success');
      else if (result.schedule.status === 'disabled' && result.current_run && activeRunStatuses.has(result.current_run.status)) notify('计划已停用；当前任务会继续运行。', 'success');
      else if (result.schedule.status === 'disabled' && !result.current_run && result.schedule.last_run && !activeRunStatuses.has(result.schedule.last_run.status)) notify('计划已停用；当前任务已先行结束。', 'success');
      else notify(result.schedule.status === 'enabled' ? '计划已启用。' : '计划已停用。', 'success');
    },
    onError: (error) => {
      setActionError(isConflict(error) ? `${errorDetails(error).message} 操作未完成，计划列表正在刷新；请检查最新状态后重试。` : errorDetails(error).message);
      if (isConflict(error)) void invalidateSchedules();
    },
  });

  const save = (schedule: Schedule | undefined, body: ScheduleSave) => {
    if (schedule) update.mutate({ id: schedule.id, body: { ...body, expected_version: schedule.version } });
    else {
      const { observation, ...createBody } = body;
      create.mutate(observation ? { ...createBody, observation } : createBody);
    }
  };
  const doDisable = (schedule: Schedule, currentRunAction: 'continue' | 'cancel') => {
    setActionError('');
    action.mutate({ schedule, body: { action: 'disable', expected_version: schedule.version, current_run_action: currentRunAction } });
  };
  const doEnable = (schedule: Schedule) => {
    setActionError('');
    action.mutate({ schedule, body: { action: 'enable', expected_version: schedule.version } });
  };

  return <section className="schedule-panel task-section" aria-labelledby="schedules-heading">
    <div className="task-section-heading">
      <div><h2 id="schedules-heading">定时计划</h2><p className="muted">按指定时间提交云端任务；每次执行都可以在任务详情中查看和处理。</p></div>
      <button type="button" className="button button-primary" onClick={() => { setEditing(null); setCreating(true); }}>＋ 新建计划</button>
    </div>
    {actionError && <p className="form-error schedule-action-error" role="alert">{actionError}</p>}
    {schedules.isPending ? <p className="muted" role="status">正在加载计划…</p>
      : schedules.isError ? <div className="error-card" role="alert">无法读取计划：{errorDetails(schedules.error).message}<button type="button" className="button button-secondary" onClick={() => void schedules.refetch()}>重试</button></div>
        : !scheduleItems.length ? <div className="empty-card compact"><p>还没有定时计划。创建后，系统会在下次计划时间提交任务。</p></div>
          : <div className="schedule-list">{scheduleItems.map((schedule) => <article className="schedule-card" key={schedule.id}>
            <header className="schedule-card-heading"><div><h3>{schedule.objective}</h3><span className={`status status-${schedule.status}`}><span aria-hidden="true" className="status-dot" />{scheduleStatusLabel[schedule.status]}</span></div>
              <span className="muted">版本 {schedule.version}</span></header>
            <p className="muted schedule-card-meta">主机 <span className="mono">{schedule.host_id}</span> · 模型 <span className="mono">{schedule.model_id}</span> · {timingLabel(schedule.timing)}</p>
            <dl className="schedule-facts"><div><dt>下次执行</dt><dd>{schedule.next_run_at ? formatDate(schedule.next_run_at) : '—'}</dd></div><div><dt>最近执行</dt><dd>{schedule.last_run ? <>{runStatusLabel[schedule.last_run.status]} · {formatDate(schedule.last_run.scheduled_at ?? schedule.last_run.created_at)}</> : '尚无执行记录'}</dd></div></dl>
            {schedule.last_run?.status === 'stopping' && <p className="notice notice-warning schedule-stop-pending" role="status">取消请求已发送，仍在等待任务停止确认；计划停用不会立即把任务标记为已取消。</p>}
            <div className="host-actions schedule-actions">
              <button type="button" className="button button-secondary button-small" onClick={() => { setCreating(false); setEditing(schedule); }}>编辑</button>
              {schedule.status !== 'completed' && <button type="button" className="button button-secondary button-small" disabled={action.isPending} onClick={() => {
                if (schedule.status !== 'enabled') doEnable(schedule);
                else if (schedule.last_run && activeRunStatuses.has(schedule.last_run.status)) setActionTarget(schedule);
                else doDisable(schedule, 'continue');
              }}>{schedule.status === 'enabled' ? '停用' : '启用'}</button>}
              <ScheduleRuns schedule={schedule} />
            </div>
          </article>)}</div>}
    {schedules.hasNextPage && <button type="button" className="button button-secondary schedule-load-more" disabled={schedules.isFetchingNextPage} onClick={() => void schedules.fetchNextPage()}>{schedules.isFetchingNextPage ? '正在加载…' : '加载更多计划'}</button>}
    {(creating || editing) && <ScheduleEditor
      key={editing?.id ?? 'new'} schedule={editing ?? undefined} hosts={runningHosts} models={usableModels}
      saving={create.isPending || update.isPending}
      error={(create.isError && errorDetails(create.error).message) || (update.isError && errorDetails(update.error).message) || ''}
      conflict={Boolean(update.error && isConflict(update.error))}
      onCancel={() => { setCreating(false); setEditing(null); create.reset(); update.reset(); }}
      onSave={(body) => save(editing ?? undefined, body)}
    />}
    {actionTarget && <Modal title="停用定时计划？" onClose={() => { if (!action.isPending) setActionTarget(null); }}>
      <div className="modal-body">
        <p>停用后不会再创建新的任务。当前这次执行正在{runStatusLabel[actionTarget.last_run!.status]}。</p>
        <p className="muted">选择继续会让当前任务运行；请求取消后，页面会保留“正在确认停止”状态，直到系统收到停止结果。</p>
        {actionError && <p className="form-error" role="alert">{actionError}</p>}
      </div>
      <footer className="modal-actions schedule-disable-actions">
        <button type="button" className="button button-secondary" disabled={action.isPending} onClick={() => setActionTarget(null)}>返回</button>
        <button type="button" className="button button-secondary" disabled={action.isPending} onClick={() => doDisable(actionTarget, 'continue')}>停用并继续</button>
        <button type="button" className="button button-danger" disabled={action.isPending} onClick={() => doDisable(actionTarget, 'cancel')}>{action.isPending ? '正在提交…' : '停用并请求取消'}</button>
      </footer>
    </Modal>}
  </section>;
}

function ScheduleEditor({ schedule, hosts, models, saving, error, conflict, onCancel, onSave }: {
  schedule?: Schedule;
  hosts: Awaited<ReturnType<typeof portalApi.hosts>>['items'];
  models: Awaited<ReturnType<typeof portalApi.modelConnections>>['items'];
  saving: boolean; error: string; conflict: boolean; onCancel: () => void;
  onSave: (body: ScheduleSave) => void;
}) {
  const initialTiming = schedule?.timing;
  const [kind, setKind] = useState<ScheduleTiming['kind']>(initialTiming?.kind ?? 'once');
  const [runAt, setRunAt] = useState(initialTiming?.kind === 'once' ? toLocalInput(initialTiming.run_at) : defaultRunAt());
  const [startsOn, setStartsOn] = useState(initialTiming && initialTiming.kind !== 'once' ? initialTiming.starts_on : localToday());
  const [localTime, setLocalTime] = useState(initialTiming && initialTiming.kind !== 'once' ? initialTiming.local_time : '09:00');
  const [timeZone, setTimeZone] = useState(initialTiming?.time_zone ?? (Intl.DateTimeFormat().resolvedOptions().timeZone || 'Asia/Shanghai'));
  const [selectedDays, setSelectedDays] = useState<number[]>(initialTiming?.kind === 'weekly' ? initialTiming.weekdays : [1, 2, 3, 4, 5]);
  const [hostID, setHostID] = useState(schedule?.host_id ?? hosts[0]?.id ?? '');
  const [modelID, setModelID] = useState(schedule?.model_id ?? models[0]?.id ?? '');
  const [objective, setObjective] = useState(schedule?.objective ?? '');
  const [activeSeconds, setActiveSeconds] = useState(String(schedule?.budget.active_seconds ?? 1800));
  const [toolCalls, setToolCalls] = useState(String(schedule?.budget.tool_calls ?? 100));
  const [modelTokens, setModelTokens] = useState(String(schedule?.budget.model_tokens ?? 50000));
  const [misfirePolicy, setMisfirePolicy] = useState<components['schemas']['MisfirePolicy']>(schedule?.misfire_policy ?? 'skip');
  const [notificationEvents, setNotificationEvents] = useState<ScheduleEvent[]>(schedule?.notification_events ?? ['completed', 'failed', 'waiting_user']);
  const [observationEnabled, setObservationEnabled] = useState(Boolean(schedule?.observation));
  const [observationURL, setObservationURL] = useState(schedule?.observation?.url ?? '');
  const [observationSelector, setObservationSelector] = useState(schedule?.observation?.selector ?? '');
  const [observationField, setObservationField] = useState<'text' | 'value'>(schedule?.observation?.field ?? 'text');
  const [validationError, setValidationError] = useState('');

  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const cleanObjective = objective.trim();
    if (!cleanObjective || !hostID || !modelID || !timeZone.trim()) { setValidationError('请填写任务目标、云端电脑、模型和时区。'); return; }
    if (kind === 'weekly' && selectedDays.length === 0) { setValidationError('每周计划至少选择一天。'); return; }
    const observationURLValue = observationURL.trim();
    const observationSelectorValue = observationSelector.trim();
    if (observationEnabled) {
      let parsedObservationURL: URL;
      try { parsedObservationURL = new URL(observationURLValue); }
      catch { setValidationError('请填写有效的网页地址（HTTP 或 HTTPS）。'); return; }
      if (!['http:', 'https:'].includes(parsedObservationURL.protocol)) { setValidationError('网页地址必须使用 HTTP 或 HTTPS。'); return; }
      if (!observationSelectorValue) { setValidationError('请填写 CSS 选择器。'); return; }
      try { document.querySelector(observationSelectorValue); }
      catch { setValidationError('CSS 选择器格式无效。'); return; }
    }
    const timing: ScheduleTiming = kind === 'once'
      ? { kind: 'once', run_at: new Date(runAt).toISOString(), time_zone: timeZone.trim() }
      : kind === 'daily'
        ? { kind: 'daily', starts_on: startsOn, local_time: localTime, time_zone: timeZone.trim() }
        : { kind: 'weekly', starts_on: startsOn, local_time: localTime, time_zone: timeZone.trim(), weekdays: [...selectedDays].sort((a, b) => a - b) };
    setValidationError('');
    onSave({
      host_id: hostID, model_id: modelID, objective: cleanObjective, timing,
      budget: { active_seconds: Number(activeSeconds), tool_calls: Number(toolCalls), model_tokens: Number(modelTokens) },
      misfire_policy: misfirePolicy, notification_events: notificationEvents,
      observation: observationEnabled ? { url: observationURLValue, selector: observationSelectorValue, field: observationField } : null,
    });
  };

  return <form className="form-card form-stack schedule-editor" onSubmit={submit}>
    <div><p className="eyebrow">{schedule ? `计划版本 ${schedule.version}` : '新建计划'}</p><h3>{schedule ? '编辑定时计划' : '创建定时计划'}</h3></div>
    {conflict && <p className="notice notice-warning" role="alert">{error} 当前变更未保存，计划列表正在刷新；请核对最新版本和状态后重试。</p>}
    {error && !conflict && <p className="form-error" role="alert">保存失败：{error}</p>}
    {validationError && <p className="form-error" role="alert">{validationError}</p>}
    {(hosts.length === 0 || models.length === 0) && <p className="notice notice-warning" role="status">{hosts.length === 0 && <>需要至少一台运行中的云端电脑。<Link to="/hosts">管理云端电脑</Link>。 </>}{models.length === 0 && <>需要先配置并检查可用模型。<Link to="/connections">检查模型设置</Link>。</>}</p>}
    <label htmlFor="schedule-objective">任务目标</label>
    <textarea id="schedule-objective" required maxLength={32768} value={objective} onChange={(event) => setObjective(event.target.value)} placeholder="描述每次计划运行要完成的任务" />
    <div className="schedule-form-grid">
      <label>云端电脑<select required aria-label="计划云端电脑" value={hostID} onChange={(event) => setHostID(event.target.value)} disabled={!hosts.length}>{hosts.map((host) => <option key={host.id} value={host.id}>{host.name || host.id} · {host.state}</option>)}</select></label>
      <label>AI 模型<select required aria-label="计划 AI 模型" value={modelID} onChange={(event) => setModelID(event.target.value)} disabled={!models.length}>{models.map((model) => <option key={model.id} value={model.id}>{model.name} · {model.model_name}</option>)}</select></label>
    </div>
    <fieldset className="fieldset"><legend>运行时间</legend>
      <div className="schedule-kind-options">
        {([['once', '一次'], ['daily', '每天'], ['weekly', '每周']] as const).map(([value, label]) => <label className="radio-row" key={value}><input type="radio" name="schedule-kind" value={value} checked={kind === value} onChange={() => setKind(value)} />{label}</label>)}
      </div>
      {kind === 'once' ? <label>执行时间<input aria-label="一次性执行时间" type="datetime-local" required min={localNowMinute()} value={runAt} onChange={(event) => setRunAt(event.target.value)} /></label>
        : <div className="schedule-form-grid"><label>开始日期<input aria-label="开始日期" type="date" required value={startsOn} onChange={(event) => setStartsOn(event.target.value)} /></label><label>本地时间<input aria-label="本地时间" type="time" required value={localTime} onChange={(event) => setLocalTime(event.target.value)} /></label></div>}
      {kind === 'weekly' && <div className="schedule-weekdays" role="group" aria-label="每周运行日">{weekdays.map((day) => <label className="checkbox-row" key={day.value}><input type="checkbox" checked={selectedDays.includes(day.value)} onChange={(event) => setSelectedDays((current) => event.target.checked ? [...current, day.value] : current.filter((value) => value !== day.value))} />{day.label}</label>)}</div>}
      <label htmlFor="schedule-timezone">时区（IANA）</label><input id="schedule-timezone" required maxLength={64} value={timeZone} onChange={(event) => setTimeZone(event.target.value)} placeholder="Asia/Shanghai" />
      <small className="muted">夏令时跳时将跳过；重复时刻采用较早的那次。一次性时间按当前设备时区选择具体时刻。</small>
    </fieldset>
    <fieldset className="fieldset"><legend>执行限制</legend>
      <div className="schedule-form-grid schedule-budget-grid">
        <label>最长活动秒数<input aria-label="最长活动秒数" type="number" required min={1} max={1800} value={activeSeconds} onChange={(event) => setActiveSeconds(event.target.value)} /></label>
        <label>工具调用次数<input aria-label="工具调用次数" type="number" required min={1} max={100} value={toolCalls} onChange={(event) => setToolCalls(event.target.value)} /></label>
        <label>模型 Token<input aria-label="模型 Token" type="number" required min={1} max={50000} value={modelTokens} onChange={(event) => setModelTokens(event.target.value)} /></label>
      </div>
      <label htmlFor="schedule-misfire">错过执行时间</label><select id="schedule-misfire" value={misfirePolicy} onChange={(event) => setMisfirePolicy(event.target.value as components['schemas']['MisfirePolicy'])}><option value="skip">跳过</option><option value="run_once">补执行一次并合并错过项</option></select>
    </fieldset>
    <fieldset className="fieldset"><legend>站内通知</legend>
      {(Object.keys(eventLabel) as ScheduleEvent[]).map((event) => <label className="checkbox-row" key={event}><input type="checkbox" checked={notificationEvents.includes(event)} onChange={(change) => setNotificationEvents((current) => change.target.checked ? [...current, event] : current.filter((value) => value !== event))} />{eventLabel[event]}</label>)}
      <small className="muted">完成通知按计划订阅的事件生成；读取通知不会审批或执行任务。</small>
    </fieldset>
    <fieldset className="fieldset"><legend>网页字段变化观察（可选）</legend>
      <label className="checkbox-row"><input type="checkbox" checked={observationEnabled} onChange={(event) => setObservationEnabled(event.target.checked)} />独立检查指定网页字段是否变化</label>
      {observationEnabled && <>
        <label htmlFor="schedule-observation-url">网页地址（HTTP 或 HTTPS）</label>
        <input id="schedule-observation-url" type="url" required maxLength={2048} value={observationURL} onChange={(event) => setObservationURL(event.target.value)} placeholder="https://example.com/status" />
        <label htmlFor="schedule-observation-selector">CSS 选择器</label>
        <input id="schedule-observation-selector" type="text" required maxLength={1024} value={observationSelector} onChange={(event) => setObservationSelector(event.target.value)} placeholder="#status" />
        <label htmlFor="schedule-observation-field">采集字段</label>
        <select id="schedule-observation-field" value={observationField} onChange={(event) => setObservationField(event.target.value as 'text' | 'value')}>
          <option value="text">节点文本</option><option value="value">表单值</option>
        </select>
        <small className="muted">首次计划执行会照常运行任务，并用首次采集结果建立快照；后续执行会独立检查该字段是否变化。请填写页面地址和 CSS 选择器，无需编写 JavaScript。采集到的字段内容不会显示在此处。</small>
      </>}
    </fieldset>
    <div className="form-actions"><button type="button" className="button button-secondary" disabled={saving} onClick={onCancel}>取消</button><button type="submit" className="button button-primary" disabled={saving || hosts.length === 0 || models.length === 0}>{saving ? '保存中…' : schedule ? '保存计划' : '创建并启用'}</button></div>
  </form>;
}

function ScheduleRuns({ schedule }: { schedule: Schedule }) {
  const [open, setOpen] = useState(false);
  const runs = useInfiniteQuery({
    queryKey: ['schedule-runs', schedule.id], queryFn: ({ pageParam }) => portalApi.scheduleRuns(schedule.id, pageParam),
    initialPageParam: undefined as string | undefined,
    getNextPageParam: (page) => page.next_cursor ?? undefined,
    enabled: open,
  });
  const items = runs.data?.pages.flatMap((page) => page.items) ?? [];
  return <div className="schedule-runs-toggle">
    <button type="button" className="button button-quiet button-small" aria-expanded={open} onClick={() => setOpen((value) => !value)}>{open ? '收起运行记录' : '运行记录'}</button>
    {open && <div className="schedule-runs" aria-label={`${schedule.objective}运行记录`}>
      {runs.isPending ? <p className="muted" role="status">正在读取运行记录…</p>
        : runs.isError ? <p className="form-error" role="alert">无法读取运行记录：{errorDetails(runs.error).message}<button type="button" className="button button-quiet" onClick={() => void runs.refetch()}>重试</button></p>
          : !items.length ? <p className="muted">还没有运行记录。</p>
            : <ul>{items.map((run) => <li key={run.id}><span>{runStatusLabel[run.status]} · {formatDate(run.scheduled_at ?? run.created_at)}{run.coalesced_count > 0 ? ` · 合并 ${run.coalesced_count} 次错过执行` : ''}{run.skip_reason ? ` · ${skipReasonLabel(run.skip_reason)}` : ''}</span>{run.task_id && <Link to={`/tasks/${encodeURIComponent(run.task_id)}`}>查看任务</Link>}</li>)}</ul>}
      {runs.hasNextPage && <button type="button" className="button button-quiet button-small" disabled={runs.isFetchingNextPage} onClick={() => void runs.fetchNextPage()}>{runs.isFetchingNextPage ? '正在加载…' : '更多运行记录'}</button>}
    </div>}
  </div>;
}

function timingLabel(timing: ScheduleTiming) {
  if (timing.kind === 'once') return `一次 · ${formatDate(timing.run_at)}（${timing.time_zone}）`;
  if (timing.kind === 'daily') return `每天 ${timing.local_time} · ${timing.time_zone}`;
  return `每周 ${timing.weekdays.map((day) => weekdays[day - 1]?.label).filter(Boolean).join('、')} ${timing.local_time} · ${timing.time_zone}`;
}

function skipReasonLabel(reason: ScheduleRun['skip_reason']) {
  return reason === 'dst_gap' ? '夏令时跳时' : reason === 'overlap' ? '上一次执行未结束' : reason === 'authorization_ended' ? '执行授权已失效' : '';
}

function isConflict(error: unknown) { return error instanceof ApiError && error.status === 409; }

function localNowMinute() {
  const now = new Date();
  now.setSeconds(0, 0);
  return toLocalInput(now.toISOString());
}

function defaultRunAt() {
  const date = new Date();
  date.setDate(date.getDate() + 1);
  date.setHours(9, 0, 0, 0);
  return toLocalInput(date.toISOString());
}

function localToday() {
  const now = new Date();
  return `${now.getFullYear()}-${String(now.getMonth() + 1).padStart(2, '0')}-${String(now.getDate()).padStart(2, '0')}`;
}

function toLocalInput(value: string) {
  const date = new Date(value);
  const offset = date.getTimezoneOffset() * 60_000;
  return new Date(date.getTime() - offset).toISOString().slice(0, 16);
}
