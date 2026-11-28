import { useMemo, useState, type FormEvent } from 'react';
import { useQuery } from '@tanstack/react-query';
import { errorDetails, portalApi } from '../../app/api';
import type { components } from '../../api/schema';
import { MordeIcon } from '../../components/MordeIcon';
import './AdminObservability.css';

type Source = components['schemas']['AdminObservabilitySource'];
type Trace = components['schemas']['AdminObservabilityTraceSummary'];
type Span = components['schemas']['AdminObservabilitySpan'];
type SpanNode = { span: Span; children: SpanNode[]; parent?: SpanNode; parentProblem: 'missing' | 'cycle' | null };
type SearchFilters = Pick<NonNullable<Parameters<typeof portalApi.adminObservabilityTraces>[0]>, 'trace_id' | 'request_id' | 'task_id' | 'q'>;
type TraceCursor = { filters: SearchFilters; cursor?: string; previous: (string | null)[] };
type EventPage = { fromSequence?: number; previous: (number | undefined)[] };
type LogPage = { cursor?: string; previous: (string | null)[] };

const numberFormat = new Intl.NumberFormat('zh-CN', { maximumFractionDigits: 1 });
const dateFormat = new Intl.DateTimeFormat('zh-CN', { dateStyle: 'medium', timeStyle: 'medium' });
const statusLabels: Record<Source['status'], string> = {
  available: '可用', partial: '部分可用', unavailable: '不可用', disabled: '未启用',
};
const traceStatusLabels: Record<string, string> = { ok: '成功', error: '错误', partial: '部分' };
const spanStatusLabels: Record<string, string> = { unset: '未标记', ok: '成功', error: '错误' };
const harnessStatusPresentation: Record<string, { label: string; tone: 'available' | 'partial' | 'disabled' }> = {
  running: { label: '执行中', tone: 'available' },
  completed: { label: '本次执行结束', tone: 'available' },
  needs_user: { label: '需用户确认', tone: 'partial' },
  waiting_user: { label: '等待用户', tone: 'partial' },
  failed: { label: '失败', tone: 'partial' },
  timed_out: { label: '已超时', tone: 'partial' },
  cancelled: { label: '已取消', tone: 'disabled' },
};

function displayTime(value: string | null | undefined) {
  if (!value) return '—';
  const time = Date.parse(value);
  return Number.isFinite(time) ? dateFormat.format(time) : value;
}

function SourceStatus({ title, source }: { title: string; source: Source }) {
  return <article className="observability-source">
    <div className="observability-source-heading"><h3>{title}</h3><span className={`observability-status is-${source.status}`}>{statusLabels[source.status]}</span></div>
    {source.reason && <p>{source.reason}</p>}
    {source.updated_at && <small>最近更新：{displayTime(source.updated_at)}</small>}
  </article>;
}

function CoverageNote({ title, source }: { title: string; source: Source }) {
  if (source.status === 'available' && !source.reason) return null;
  return <div className={`observability-coverage is-${source.status}`} role="status">
    <strong>{title}：{statusLabels[source.status]}</strong>
    <span>{source.reason || '服务端没有提供原因。'}</span>
  </div>;
}

function SectionError({ label, error, retry }: { label: string; error: unknown; retry: () => void }) {
  return <div className="error-card compact observability-error" role="alert">
    <span>{label}读取失败：{errorDetails(error).message}</span>
    <button className="button button-secondary button-small" type="button" onClick={retry}>重试</button>
  </div>;
}

function TraceResultRow({ item, selected, onSelect }: { item: Trace; selected: boolean; onSelect: (trace: Trace) => void }) {
  return <tr className={selected ? 'is-selected' : undefined}>
    <td><button type="button" className="observability-trace-link" onClick={() => onSelect(item)}>{item.trace_id}</button></td>
    <td>{item.request_id || '—'}</td><td>{item.task_id || '—'}</td><td>{displayTime(item.started_at)}</td>
    <td><span className={`observability-status is-${item.status === 'error' || item.error_count > 0 ? 'partial' : item.status === 'partial' ? 'disabled' : 'available'}`}>{traceStatusLabels[item.status] || item.status}</span></td>
    <td>{numberFormat.format(item.span_count)}</td><td>{numberFormat.format(item.error_count)}</td>
  </tr>;
}

function LogRecords({ logs }: { logs: components['schemas']['AdminObservabilityLog'][] }) {
  if (!logs.length) return <div className="empty-card compact" role="status"><p>没有找到与此 trace 关联的结构化服务端日志。</p></div>;
  return <div className="table-wrap"><table className="observability-log-table"><thead><tr><th>时间</th><th>级别</th><th>服务</th><th>消息</th><th>Trace / Span</th><th>Request / Task</th><th>属性</th></tr></thead><tbody>
    {logs.map((log, index) => <tr key={`${log.time}-${log.span_id ?? ''}-${index}`}>
      <td>{displayTime(log.time)}</td>
      <td><span className={`observability-status is-${['error', 'dpanic', 'panic', 'fatal', 'warn'].includes(log.severity) ? 'partial' : 'available'}`}>{log.severity}</span></td>
      <td>{log.service}</td><td className="observability-log-message">{log.message}</td>
      <td><code>{log.trace_id}</code><small>{log.span_id ? `Span ${log.span_id}` : 'Span —'}</small></td>
      <td><code>{log.request_id || '—'}</code><small>{log.task_id ? `Task ${log.task_id}` : 'Task —'}</small></td>
      <td><details className="observability-log-attributes"><summary>查看</summary><JsonValue value={log.attributes} /></details></td>
    </tr>)}
  </tbody></table></div>;
}

function JsonValue({ value }: { value: unknown }) {
  let text: string;
  try { text = JSON.stringify(value, null, 2) ?? 'null'; }
  catch { text = String(value); }
  return <pre className="observability-json">{text}</pre>;
}

function agentRoleLabel(role: string) {
  return role === 'root' ? '主 Agent' : '子 Agent';
}

function SpanTreeItem({ node }: { node: SpanNode }) {
  const { span } = node;
  const harnessStatus = typeof span.attributes['morde.status'] === 'string' ? harnessStatusPresentation[span.attributes['morde.status']] : undefined;
  const status = harnessStatus?.tone ?? (span.status === 'error' ? 'partial' : span.status === 'unset' ? 'disabled' : 'available');
  const statusLabel = harnessStatus?.label ?? spanStatusLabels[span.status] ?? span.status;
  return <li className="observability-span-node">
    <details className="observability-span">
      <summary><span className="observability-span-name">{span.name}</span><span className="muted">{span.kind}</span><span className={`observability-status is-${status}`}>{statusLabel}</span><span>{displayTime(span.start_at)}</span></summary>
      <dl className="observability-span-meta"><dt>Trace ID</dt><dd>{span.trace_id}</dd><dt>Span ID</dt><dd>{span.span_id}</dd><dt>父 Span</dt><dd>{span.parent_span_id || '—'}</dd><dt>结束时间</dt><dd>{displayTime(span.end_at)}</dd></dl>
      {node.parentProblem === 'missing' && <p className="observability-missing-parent" role="status">父 Span {span.parent_span_id} 不在本次结果中，无法确认其上级关系。</p>}
      {node.parentProblem === 'cycle' && <p className="observability-missing-parent" role="status">检测到 Span 父子关系循环，已将此节点单独展示。</p>}
      <h4>属性</h4><JsonValue value={span.attributes} />
      {span.events.length > 0 && <><h4>Span 事件（{span.events.length}）</h4><JsonValue value={span.events} /></>}
      {span.links.length > 0 && <section className="observability-span-links" aria-label={`Span links ${span.span_id}`}>
        <h4>Span Links（{span.links.length}）</h4>
        <ul>{span.links.map((link, index) => <li key={`${link.trace_id}:${link.span_id}:${index}`}>
          <span className={`observability-link-kind ${link.trace_id === span.trace_id ? 'is-same-trace' : 'is-cross-trace'}`}>{link.trace_id === span.trace_id ? '同 Trace Link' : '跨 Trace Link'}</span>
          <span>Trace <code>{link.trace_id}</code> · Span <code>{link.span_id}</code></span>
          {Object.keys(link.attributes).length > 0 && <details className="observability-link-attributes"><summary>属性</summary><JsonValue value={link.attributes} /></details>}
        </li>)}</ul>
      </section>}
      {node.children.length > 0 && <ol className="observability-span-children">{node.children.map((child) => <SpanTreeItem key={`${child.span.trace_id}:${child.span.span_id}`} node={child} />)}</ol>}
    </details>
  </li>;
}

function SpanList({ spans, primaryTraceID }: { spans: Span[]; primaryTraceID: string }) {
  if (!spans.length) return <div className="empty-card compact" role="status"><p>该 trace 没有 span 记录。</p></div>;
  const nodes = spans.map((span): SpanNode => ({ span, children: [], parentProblem: null }));
  const nodesByID = new Map(nodes.map((node) => [`${node.span.trace_id}:${node.span.span_id}`, node]));
  const rootsByTrace = new Map<string, SpanNode[]>();
  const missingParentsByTrace = new Map<string, number>();
  const spansByTrace = new Map<string, number>();
  for (const span of spans) spansByTrace.set(span.trace_id, (spansByTrace.get(span.trace_id) ?? 0) + 1);
  const addRoot = (traceID: string, node: SpanNode) => rootsByTrace.set(traceID, [...(rootsByTrace.get(traceID) ?? []), node]);
  for (const node of nodes) {
    if (!node.span.parent_span_id) {
      addRoot(node.span.trace_id, node);
      continue;
    }
    const parent = nodesByID.get(`${node.span.trace_id}:${node.span.parent_span_id}`);
    let ancestor = parent;
    let createsCycle = false;
    while (ancestor) {
      if (ancestor === node) { createsCycle = true; break; }
      ancestor = ancestor.parent;
    }
    if (!parent || createsCycle) {
      node.parentProblem = parent ? 'cycle' : 'missing';
      missingParentsByTrace.set(node.span.trace_id, (missingParentsByTrace.get(node.span.trace_id) ?? 0) + 1);
      addRoot(node.span.trace_id, node);
      continue;
    }
    node.parent = parent;
    parent.children.push(node);
  }
  const orderByStart = (left: SpanNode, right: SpanNode) => left.span.start_at.localeCompare(right.span.start_at) || left.span.span_id.localeCompare(right.span.span_id);
  const sortTree = (node: SpanNode) => { node.children.sort(orderByStart); node.children.forEach(sortTree); };
  for (const roots of rootsByTrace.values()) roots.sort(orderByStart).forEach(sortTree);
  const traceIDs = [...rootsByTrace.keys()].sort((left, right) => left === primaryTraceID ? -1 : right === primaryTraceID ? 1 : left.localeCompare(right));
  return <>
    {traceIDs.map((traceID) => {
      const roots = rootsByTrace.get(traceID) ?? [];
      const missingParentCount = missingParentsByTrace.get(traceID) ?? 0;
      return <section className="observability-span-trace" key={traceID} aria-label={`Trace ${traceID} spans`}>
        <div className="observability-span-trace-heading"><strong>Trace {traceID}</strong><span className="observability-status is-available">{traceID === primaryTraceID ? '检索 Trace' : '关联 Trace'}</span><span className="muted">{(spansByTrace.get(traceID) ?? 0)} 个 Span</span></div>
        {missingParentCount > 0 && <div className="observability-tree-warning" role="status">
          {missingParentCount} 个 Span 的父 Span 不在此 Trace 的返回结果中或关系存在循环，Span 树不完整。详情中保留了原始 parent_span_id。
        </div>}
        <ol className="observability-span-tree">{roots.map((node) => <SpanTreeItem key={`${node.span.trace_id}:${node.span.span_id}`} node={node} />)}</ol>
      </section>;
    })}
  </>;
}

function EventRecords({ events, sessions }: {
  events: components['schemas']['AdminObservabilityDSHEvent'][];
  sessions: components['schemas']['AdminObservabilityAgentSession'][];
}) {
  if (!events.length) return <div className="empty-card compact" role="status"><p>此页没有匹配的 DSH 事件记录。</p></div>;
  const bySession = new Map<string, typeof events>();
  for (const event of events) bySession.set(event.session_id, [...(bySession.get(event.session_id) ?? []), event]);
  const sessionMap = new Map(sessions.map((session) => [session.session_id, session]));
  return <div className="observability-event-sessions">
    {[...bySession.entries()].map(([sessionID, records]) => {
      const session = sessionMap.get(sessionID);
      return <details className="observability-session" key={sessionID}>
        <summary><span>{session ? agentRoleLabel(session.role) : 'Agent'} · {sessionID}</span><span className="muted">{records.length} 条事件</span></summary>
        {session && <p className="observability-session-meta">Step：{session.step_id || '—'} · 父会话：{session.parent_session_id || '—'}</p>}
        <ol className="observability-event-list">
          {records.map((event) => <li key={`${event.session_id}-${event.sequence}`}>
            <div className="observability-event-heading"><strong>{event.type}</strong><time>{displayTime(event.time)}</time><span>序号 {event.sequence}</span></div>
            <JsonValue value={event.data} />
          </li>)}
        </ol>
      </details>;
    })}
  </div>;
}

function TraceDetail({ trace, onClose }: { trace: Trace; onClose: () => void }) {
  const traceID = trace.trace_id;
  const [agentSessionID, setAgentSessionID] = useState('');
  const [eventPage, setEventPage] = useState<EventPage>({ previous: [] });
  const [logPage, setLogPage] = useState<LogPage>({ previous: [] });
  const detail = useQuery({
    queryKey: ['admin', 'observability', 'trace', traceID],
    queryFn: () => portalApi.adminObservabilityTrace(traceID),
  });
  const eventQuery = useQuery({
    queryKey: ['admin', 'observability', 'events', traceID, agentSessionID, eventPage.fromSequence ?? null],
    queryFn: () => portalApi.adminObservabilityEvents(traceID, {
      limit: 100,
      ...(agentSessionID ? { agent_session_id: agentSessionID } : {}),
      ...(eventPage.fromSequence !== undefined ? { from_sequence: eventPage.fromSequence } : {}),
    }),
  });
  const logQuery = useQuery({
    queryKey: ['admin', 'observability', 'logs', traceID, trace.request_id, trace.task_id, logPage.cursor ?? null],
    queryFn: () => portalApi.adminObservabilityLogs(traceID, {
      limit: 50,
      ...(trace.request_id ? { request_id: trace.request_id } : {}),
      ...(trace.task_id ? { task_id: trace.task_id } : {}),
      ...(logPage.cursor ? { cursor: logPage.cursor } : {}),
    }),
  });
  const sessions = eventQuery.data?.agent_sessions ?? [];
  const nextSequence = eventQuery.data?.next_sequence ?? null;
  const nextLogCursor = logQuery.data?.next_cursor ?? null;

  function setSession(value: string) {
    setAgentSessionID(value);
    setEventPage({ previous: [] });
  }

  function nextEvents() {
    if (nextSequence === null) return;
    setEventPage((current) => ({ fromSequence: nextSequence, previous: [...current.previous, current.fromSequence] }));
  }

  function previousEvents() {
    setEventPage((current) => {
      if (!current.previous.length) return current;
      const previous = [...current.previous];
      const fromSequence = previous.pop();
      return { fromSequence, previous };
    });
  }

  function nextLogs() {
    if (!nextLogCursor) return;
    setLogPage((current) => ({ cursor: nextLogCursor, previous: [...current.previous, current.cursor ?? null] }));
  }

  function previousLogs() {
    setLogPage((current) => {
      if (!current.previous.length) return current;
      const previous = [...current.previous];
      const cursor = previous.pop();
      return { ...(cursor ? { cursor } : {}), previous };
    });
  }

  return <section className="admin-detail-section observability-detail" aria-labelledby="observability-detail-heading">
    <div className="observability-section-heading">
      <div><h2 id="observability-detail-heading">Trace 详情</h2><p className="muted observability-identifier">Trace {traceID} · Request {trace.request_id || '—'} · Task {trace.task_id || '—'}</p></div>
      <button type="button" className="button button-quiet button-small" onClick={onClose}>关闭详情</button>
    </div>
    {detail.isPending && !detail.data ? <div className="loading-card" role="status">正在加载 trace spans…</div>
      : detail.isError && !detail.data ? <SectionError label="Trace spans" error={detail.error} retry={() => void detail.refetch()} />
        : detail.data ? <>
          {detail.isError && <SectionError label="Trace spans 刷新" error={detail.error} retry={() => void detail.refetch()} />}
          <CoverageNote title="Span 数据覆盖" source={detail.data.coverage} />
          <SpanList spans={detail.data.spans} primaryTraceID={detail.data.trace_id} />
        </> : null}

    <section className="observability-events" aria-labelledby="observability-events-heading">
      <div className="observability-section-heading">
        <div><h3 id="observability-events-heading">Harness / DSH 执行事件</h3><p>默认显示主 Agent 的 checkpoint 事件；切换会话可查看子 Agent。仅展示已记录事件，未记录的模型内部推理不会显示。</p></div>
        <label className="observability-session-filter">Agent 会话
          <select aria-label="按 Agent 会话筛选" value={agentSessionID} onChange={(event) => setSession(event.target.value)}>
            <option value="">主 Agent（默认）</option>
            {sessions.map((session) => <option key={session.session_id} value={session.session_id}>{agentRoleLabel(session.role)} · {session.step_id || '无 Step ID'} · {session.session_id}</option>)}
          </select>
        </label>
      </div>
      {eventQuery.isPending && !eventQuery.data ? <div className="loading-card" role="status">正在加载 DSH 事件…</div>
        : eventQuery.isError && !eventQuery.data ? <SectionError label="DSH 事件" error={eventQuery.error} retry={() => void eventQuery.refetch()} />
          : eventQuery.data ? <>
            {eventQuery.isError && <SectionError label="DSH 事件刷新" error={eventQuery.error} retry={() => void eventQuery.refetch()} />}
            <CoverageNote title="DSH 事件覆盖" source={eventQuery.data.coverage} />
            <EventRecords events={eventQuery.data.events} sessions={eventQuery.data.agent_sessions} />
            <div className="observability-pagination">
              <button type="button" className="button button-secondary button-small" disabled={!eventPage.previous.length || eventQuery.isFetching} onClick={previousEvents}>上一页</button>
              <span>{eventQuery.data.events.length} 条事件{eventPage.fromSequence !== undefined ? ` · 从序号 ${eventPage.fromSequence} 开始` : ''}</span>
              <button type="button" className="button button-secondary button-small" disabled={nextSequence === null || eventQuery.isFetching} onClick={nextEvents}>更多事件</button>
            </div>
          </> : null}
    </section>

    <section className="observability-logs" aria-labelledby="observability-logs-heading">
      <div className="observability-section-heading">
        <div><h3 id="observability-logs-heading">关联服务端日志</h3><p>按 trace_id 查询，并附带该请求已关联的 request_id、task_id。只展示服务端采集并经过字段脱敏的结构化日志。</p></div>
      </div>
      {logQuery.isPending && !logQuery.data ? <div className="loading-card" role="status">正在读取关联日志…</div>
        : logQuery.isError && !logQuery.data ? <SectionError label="关联日志" error={logQuery.error} retry={() => void logQuery.refetch()} />
          : logQuery.data ? <>
            {logQuery.isError && <SectionError label="关联日志刷新" error={logQuery.error} retry={() => void logQuery.refetch()} />}
            <CoverageNote title="服务端日志覆盖" source={logQuery.data.coverage} />
            <LogRecords logs={logQuery.data.items} />
            <div className="observability-pagination">
              <button type="button" className="button button-secondary button-small" disabled={!logPage.previous.length || logQuery.isFetching} onClick={previousLogs}>上一页</button>
              <span>{logQuery.data.items.length} 条日志</span>
              <button type="button" className="button button-secondary button-small" disabled={!nextLogCursor || logQuery.isFetching} onClick={nextLogs}>更多日志</button>
            </div>
          </> : null}
    </section>
  </section>;
}

export function AdminObservability() {
  const [draft, setDraft] = useState<SearchFilters>({});
  const [tracePage, setTracePage] = useState<TraceCursor>({ filters: {}, previous: [] });
  const [selectedTrace, setSelectedTrace] = useState<Trace | null>(null);
  const overview = useQuery({ queryKey: ['admin', 'observability', 'overview'], queryFn: portalApi.adminObservabilityOverview });
  const listQuery = useMemo(() => ({
    ...tracePage.filters,
    limit: 50,
    ...(tracePage.cursor ? { cursor: tracePage.cursor } : {}),
  }), [tracePage]);
  const traces = useQuery({
    queryKey: ['admin', 'observability', 'traces', listQuery],
    queryFn: () => portalApi.adminObservabilityTraces(listQuery),
  });

  function submitSearch(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const filters = Object.fromEntries(Object.entries(draft).map(([key, value]) => [key, value?.trim()]).filter(([, value]) => Boolean(value))) as SearchFilters;
    setTracePage({ filters, previous: [] });
    setSelectedTrace(null);
  }

  function clearSearch() {
    setDraft({});
    setTracePage({ filters: {}, previous: [] });
    setSelectedTrace(null);
  }

  function nextTraces() {
    const cursor = traces.data?.next_cursor;
    if (!cursor) return;
    setTracePage((current) => ({ filters: current.filters, cursor, previous: [...current.previous, current.cursor ?? null] }));
  }

  function previousTraces() {
    setTracePage((current) => {
      if (!current.previous.length) return current;
      const previous = [...current.previous];
      const cursor = previous.pop();
      return { filters: current.filters, ...(cursor ? { cursor } : {}), previous };
    });
  }

  return <section className="admin-content observability-page">
    <header className="page-heading"><div><h2>Harness 可观测</h2><p className="muted">按 trace、request、task 或关键词定位服务端请求、span 与 Harness 事件。</p></div></header>
    <p className="observability-privacy-note"><MordeIcon name="info" /> 只展示已采集到的 Harness 执行记录；模型服务未返回或 Harness 未记录的内部推理过程不会显示。</p>

    <section className="admin-detail-section" aria-labelledby="observability-overview-heading">
      <h2 id="observability-overview-heading">近 24 小时与数据源</h2>
      {overview.isPending && !overview.data ? <div className="loading-card" role="status">正在读取可观测性概览…</div>
        : overview.isError && !overview.data ? <SectionError label="可观测性概览" error={overview.error} retry={() => void overview.refetch()} />
          : overview.data ? <>
            {overview.isError && <SectionError label="可观测性概览刷新" error={overview.error} retry={() => void overview.refetch()} />}
            <div className="admin-detail-facts observability-metrics">
              <article><span>请求数</span><strong>{numberFormat.format(overview.data.spans_24h.request_count)}</strong><small>近 24 小时 span 汇总</small></article>
              <article><span>错误数</span><strong>{numberFormat.format(overview.data.spans_24h.error_count)}</strong><small>按已采集 span 统计</small></article>
              <article><span>P95 耗时</span><strong>{numberFormat.format(overview.data.spans_24h.p95_duration_ms)} ms</strong><small>按已采集 span 统计</small></article>
            </div>
            <div className="observability-sources">
              <SourceStatus title="请求与 Span" source={overview.data.sources.spans} />
              <SourceStatus title="服务端日志" source={overview.data.sources.logs} />
              <SourceStatus title="Harness / DSH 事件" source={overview.data.sources.dsh_events} />
            </div>
          </> : null}
    </section>

    <section className="admin-detail-section" aria-labelledby="observability-search-heading">
      <div className="observability-section-heading"><div><h2 id="observability-search-heading">Trace 检索</h2><p>精确 ID 查询和关键词查询由服务端执行。</p></div></div>
      <form className="observability-search" onSubmit={submitSearch}>
        <label>Trace ID<input value={draft.trace_id ?? ''} onChange={(event) => setDraft((value) => ({ ...value, trace_id: event.target.value }))} placeholder="trace_id" /></label>
        <label>Request ID<input value={draft.request_id ?? ''} onChange={(event) => setDraft((value) => ({ ...value, request_id: event.target.value }))} placeholder="request_id" /></label>
        <label>Task ID<input value={draft.task_id ?? ''} onChange={(event) => setDraft((value) => ({ ...value, task_id: event.target.value }))} placeholder="task_id" /></label>
        <label>关键词<input value={draft.q ?? ''} onChange={(event) => setDraft((value) => ({ ...value, q: event.target.value }))} placeholder="span 名称或属性" /></label>
        <div className="observability-search-actions"><button className="button button-primary" type="submit"><MordeIcon name="search" />搜索</button><button className="button button-secondary" type="button" onClick={clearSearch}>清空</button></div>
      </form>
      {traces.data && <CoverageNote title="Trace 数据源" source={traces.data.sources.spans} />}
      {traces.isPending && !traces.data ? <div className="loading-card" role="status">正在搜索 trace…</div>
        : traces.isError && !traces.data ? <SectionError label="Trace 列表" error={traces.error} retry={() => void traces.refetch()} />
          : traces.data ? <>
            {traces.isError && <SectionError label="Trace 列表刷新" error={traces.error} retry={() => void traces.refetch()} />}
            {traces.data.items.length === 0 ? <div className="empty-card compact observability-empty" role="status"><p>没有符合条件的 trace。</p></div>
              : <div className="table-wrap"><table className="observability-trace-table"><thead><tr><th>Trace ID</th><th>Request ID</th><th>Task ID</th><th>开始时间</th><th>状态</th><th>Spans</th><th>错误</th></tr></thead><tbody>
                {traces.data.items.map((item) => <TraceResultRow key={item.trace_id} item={item} selected={selectedTrace?.trace_id === item.trace_id} onSelect={setSelectedTrace} />)}
              </tbody></table></div>}
            <div className="observability-pagination"><button type="button" className="button button-secondary button-small" disabled={!tracePage.previous.length || traces.isFetching} onClick={previousTraces}>上一页</button><span>{traces.data.items.length} 条 trace</span><button type="button" className="button button-secondary button-small" disabled={!traces.data.next_cursor || traces.isFetching} onClick={nextTraces}>下一页</button></div>
          </> : null}
    </section>

    {selectedTrace && <TraceDetail key={selectedTrace.trace_id} trace={selectedTrace} onClose={() => setSelectedTrace(null)} />}
  </section>;
}
