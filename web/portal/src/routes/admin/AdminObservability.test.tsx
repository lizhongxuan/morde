import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { setupServer } from 'msw/node';
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest';
import type { components } from '../../api/schema';
import { AdminObservability } from './AdminObservability';

type Overview = components['schemas']['AdminObservabilityOverview'];
type TracePage = components['schemas']['AdminObservabilityTracePage'];
type Trace = components['schemas']['AdminObservabilityTrace'];
type EventPage = components['schemas']['AdminObservabilityTraceEvents'];

const available: components['schemas']['AdminObservabilitySource'] = { status: 'available', reason: null, updated_at: '2026-11-27T10:00:00Z' };
const emptyOverview: Overview = {
  sources: {
    spans: available,
    logs: { status: 'disabled', reason: '服务端结构化日志未启用。', updated_at: null },
    dsh_events: { status: 'disabled', reason: 'DSH 事件存储未启用。', updated_at: null },
  },
  spans_24h: { request_count: 0, error_count: 0, p95_duration_ms: 0 },
};
const emptyTracePage: TracePage = { items: [], next_cursor: null, sources: { spans: available } };
const emptyTrace: Trace = { trace_id: 'trace-1', spans: [], coverage: available };
const emptyEvents: EventPage = { task_id: null, agent_sessions: [], events: [], next_sequence: null, coverage: { status: 'disabled', reason: 'DSH 事件存储未启用。', updated_at: null } };

const server = setupServer(
  http.get('*/api/admin/observability/overview', () => HttpResponse.json(emptyOverview, { headers: { 'Cache-Control': 'no-store' } })),
  http.get('*/api/admin/observability/traces', () => HttpResponse.json(emptyTracePage, { headers: { 'Cache-Control': 'no-store' } })),
  http.get('*/api/admin/observability/traces/:trace_id', ({ params }) => HttpResponse.json({ ...emptyTrace, trace_id: String(params.trace_id) }, { headers: { 'Cache-Control': 'no-store' } })),
  http.get('*/api/admin/observability/traces/:trace_id/events', () => HttpResponse.json(emptyEvents, { headers: { 'Cache-Control': 'no-store' } })),
  http.get('*/api/admin/observability/traces/:trace_id/logs', () => HttpResponse.json({ items: [], next_cursor: null, coverage: emptyOverview.sources.logs }, { headers: { 'Cache-Control': 'no-store' } })),
);

function renderPage() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={client}><AdminObservability /></QueryClientProvider>);
}

beforeAll(() => server.listen());
afterEach(() => { cleanup(); server.resetHandlers(); });
afterAll(() => server.close());

describe('admin Harness observability', () => {
  it('shows independent empty states and makes the hidden-reasoning boundary clear', async () => {
    renderPage();

    expect(await screen.findByText('没有符合条件的 trace。')).toBeInTheDocument();
    expect(screen.getByText('DSH 事件存储未启用。')).toBeInTheDocument();
    expect(screen.getByText('服务端结构化日志未启用。')).toBeInTheDocument();
    expect(screen.getByText(/Harness 未记录的内部推理过程不会显示/)).toBeInTheDocument();
    expect(screen.getByText('0 ms')).toBeInTheDocument();
  });

  it('shows partial source coverage and the returned reason beside trace results', async () => {
    const partial: Overview = {
      ...emptyOverview,
      sources: {
        spans: { status: 'partial', reason: '部分服务实例尚未接入。', updated_at: null },
        logs: { status: 'partial', reason: '日志队列丢弃了部分记录。', updated_at: null },
        dsh_events: { status: 'unavailable', reason: 'DSH 事件存储连接失败。', updated_at: null },
      },
    };
    server.use(
      http.get('*/api/admin/observability/overview', () => HttpResponse.json(partial)),
      http.get('*/api/admin/observability/traces', () => HttpResponse.json({ ...emptyTracePage, sources: { spans: partial.sources.spans } })),
    );
    renderPage();

    expect((await screen.findAllByText('部分服务实例尚未接入。')).length).toBeGreaterThan(0);
    expect(screen.getByText('日志队列丢弃了部分记录。')).toBeInTheDocument();
    expect(screen.getByText('DSH 事件存储连接失败。')).toBeInTheDocument();
    expect(screen.getAllByText('部分可用').length).toBeGreaterThan(0);
    expect(screen.getByText('不可用')).toBeInTheDocument();
  });

  it('keeps trace search usable when the overview request fails', async () => {
    server.use(http.get('*/api/admin/observability/overview', () => HttpResponse.json({ error: { code: 'internal', message: '概览存储暂时不可用' } }, { status: 503 })));
    renderPage();

    expect(await screen.findByRole('alert')).toHaveTextContent('可观测性概览读取失败：服务暂时不可用');
    expect(screen.getByText('没有符合条件的 trace。')).toBeInTheDocument();
    expect(screen.getByRole('heading', { name: 'Trace 检索' })).toBeInTheDocument();
  });

  it('submits the trace, task, and keyword filters to the admin search endpoint', async () => {
    const user = userEvent.setup();
    let receivedQuery = new URLSearchParams();
    server.use(http.get('*/api/admin/observability/traces', ({ request }) => {
      receivedQuery = new URL(request.url).searchParams;
      return HttpResponse.json({ ...emptyTracePage, items: [{
        trace_id: 'trace-found', request_id: null, task_id: 'task-found', started_at: '2026-11-27T10:00:00Z', ended_at: null,
        status: 'ok', span_count: 2, error_count: 0,
      }] });
    }));
    renderPage();

    await user.type(screen.getByLabelText('Trace ID'), 'trace-found');
    await user.type(screen.getByLabelText('Task ID'), 'task-found');
    await user.type(screen.getByLabelText('关键词'), 'tool search');
    await user.click(screen.getByRole('button', { name: '搜索' }));

    expect(await screen.findByRole('button', { name: 'trace-found' })).toBeInTheDocument();
    expect(receivedQuery.get('trace_id')).toBe('trace-found');
    expect(receivedQuery.get('task_id')).toBe('task-found');
    expect(receivedQuery.get('q')).toBe('tool search');
  });

  it('keeps span and checkpoint event sections available when the log source fails', async () => {
    const user = userEvent.setup();
    const summary: components['schemas']['AdminObservabilityTraceSummary'] = {
      trace_id: 'trace-1', request_id: 'request-1', task_id: 'task-1', started_at: '2026-11-27T10:00:00Z', ended_at: null,
      status: 'ok', span_count: 1, error_count: 0,
    };
    server.use(
      http.get('*/api/admin/observability/traces', () => HttpResponse.json({ ...emptyTracePage, items: [summary] })),
      http.get('*/api/admin/observability/traces/:trace_id', () => HttpResponse.json({
        trace_id: 'trace-1', coverage: available,
        spans: [{ trace_id: 'trace-1', span_id: 'span-1', parent_span_id: null, name: 'agent.run', kind: 'internal', status: 'ok', start_at: '2026-11-27T10:00:00Z', end_at: null, attributes: {}, events: [], links: [] }],
      } satisfies Trace)),
      http.get('*/api/admin/observability/traces/:trace_id/logs', () => HttpResponse.json({ error: { code: 'internal', message: '日志服务不可用' } }, { status: 503 })),
    );
    renderPage();

    await user.click(await screen.findByRole('button', { name: 'trace-1' }));
    expect(await screen.findByText('agent.run')).toBeInTheDocument();
    const eventsRegion = screen.getByRole('region', { name: 'Harness / DSH 执行事件' });
    expect(await within(eventsRegion).findByText('DSH 事件存储未启用。')).toBeInTheDocument();
    const logsRegion = screen.getByRole('region', { name: '关联服务端日志' });
    expect(await within(logsRegion).findByRole('alert')).toHaveTextContent('关联日志读取失败：服务暂时不可用');
  });

  it('builds trace-scoped span trees, shows cross-trace links, and paginates events and logs', async () => {
    const user = userEvent.setup();
    const primaryTraceID = 'a'.repeat(32);
    const linkedTraceID = 'b'.repeat(32);
    const sharedSpanID = '1'.repeat(16);
    const traceItem: components['schemas']['AdminObservabilityTraceSummary'] = {
      trace_id: primaryTraceID, request_id: 'request-1', task_id: 'task-1', started_at: '2026-11-27T10:00:00Z', ended_at: null,
      status: 'ok', span_count: 5, error_count: 0,
    };
    const sessions: components['schemas']['AdminObservabilityAgentSession'][] = [{ session_id: 'session-1', parent_session_id: null, step_id: 'step-1', role: 'root' }];
    const events: components['schemas']['AdminObservabilityDSHEvent'][] = [
      { session_id: 'session-1', sequence: 1, time: '2026-11-27T10:00:01Z', type: 'tool.call', data: { tool: 'search' } },
      { session_id: 'session-1', sequence: 2, time: '2026-11-27T10:00:02Z', type: 'tool.result', data: { ok: true } },
    ];
    const receivedLogsURLs: URL[] = [];
    const structuredLogs: components['schemas']['AdminObservabilityLog'][] = [
      { time: '2026-11-27T10:00:01Z', severity: 'info', service: 'portal', message: 'request accepted', trace_id: primaryTraceID, span_id: sharedSpanID, request_id: 'request-1', task_id: 'task-1', attributes: { route: '/api/tasks' } },
      { time: '2026-11-27T10:00:02Z', severity: 'warn', service: 'portal', message: 'task checkpoint committed', trace_id: primaryTraceID, span_id: null, request_id: 'request-1', task_id: 'task-1', attributes: { status: 'ok' } },
    ];
    server.use(
      http.get('*/api/admin/observability/traces', () => HttpResponse.json({ ...emptyTracePage, items: [traceItem] })),
      http.get('*/api/admin/observability/traces/:trace_id', () => HttpResponse.json({
        trace_id: primaryTraceID, coverage: available,
        spans: [
          { trace_id: primaryTraceID, span_id: sharedSpanID, parent_span_id: null, name: 'agent.run', kind: 'internal', status: 'ok', start_at: '2026-11-27T10:00:00Z', end_at: null, attributes: { tool_count: 1 }, events: [], links: [{ trace_id: linkedTraceID, span_id: sharedSpanID, attributes: { relation: 'async_attempt' } }] },
          { trace_id: primaryTraceID, span_id: '2'.repeat(16), parent_span_id: sharedSpanID, name: 'tool.execute', kind: 'client', status: 'ok', start_at: '2026-11-27T10:00:00.500Z', end_at: null, attributes: { tool_name: 'search' }, events: [], links: [] },
          { trace_id: primaryTraceID, span_id: '3'.repeat(16), parent_span_id: '4'.repeat(16), name: 'orphan.span', kind: 'internal', status: 'unset', start_at: '2026-11-27T10:00:00.750Z', end_at: null, attributes: {}, events: [], links: [] },
          { trace_id: linkedTraceID, span_id: sharedSpanID, parent_span_id: null, name: 'attempt.resume', kind: 'internal', status: 'ok', start_at: '2026-11-27T10:00:01Z', end_at: null, attributes: {}, events: [], links: [] },
          { trace_id: primaryTraceID, span_id: '5'.repeat(16), parent_span_id: sharedSpanID, name: 'morde.tool.call', kind: 'internal', status: 'ok', start_at: '2026-11-27T10:00:01.500Z', end_at: null, attributes: { 'morde.status': 'needs_user' }, events: [], links: [] },
        ],
      } satisfies Trace)),
      http.get('*/api/admin/observability/traces/:trace_id/events', ({ request }) => {
        const fromSequence = new URL(request.url).searchParams.get('from_sequence');
        const secondPage = fromSequence === '2';
        return HttpResponse.json({ task_id: 'task-1', agent_sessions: sessions, events: [events[secondPage ? 1 : 0]!], next_sequence: secondPage ? null : 2, coverage: available } satisfies EventPage);
      }),
      http.get('*/api/admin/observability/traces/:trace_id/logs', ({ request }) => {
        const url = new URL(request.url);
        receivedLogsURLs.push(url);
        const secondPage = url.searchParams.get('cursor') === 'log-next';
        return HttpResponse.json({ items: [structuredLogs[secondPage ? 1 : 0]!], next_cursor: secondPage ? null : 'log-next', coverage: available });
      }),
    );
    renderPage();

    await user.click(await screen.findByRole('button', { name: primaryTraceID }));
    expect(await screen.findByText('agent.run')).toBeInTheDocument();
    const primaryTraceRegion = screen.getByRole('region', { name: `Trace ${primaryTraceID} spans` });
    const linkedTraceRegion = screen.getByRole('region', { name: `Trace ${linkedTraceID} spans` });
    expect(within(primaryTraceRegion).getByText('检索 Trace')).toBeInTheDocument();
    expect(within(linkedTraceRegion).getByText('关联 Trace')).toBeInTheDocument();
    const childSpanName = screen.getByText('tool.execute', { selector: '.observability-span-name' });
    expect(childSpanName.closest('li')?.parentElement).toHaveClass('observability-span-children');
    expect(childSpanName.closest('.observability-span-trace')).toBe(primaryTraceRegion);
    expect(within(primaryTraceRegion).getByText(/1 个 Span 的父 Span 不在此 Trace 的返回结果中/)).toBeInTheDocument();
    const rootSpan = within(primaryTraceRegion).getByText('agent.run', { selector: '.observability-span-name' });
    await user.click(rootSpan);
    expect(within(primaryTraceRegion).getByText('跨 Trace Link')).toBeInTheDocument();
    expect(within(primaryTraceRegion).getByText('需用户确认')).toBeInTheDocument();
    expect(within(primaryTraceRegion).getByText(linkedTraceID)).toBeInTheDocument();
    expect(within(primaryTraceRegion).getByText(sharedSpanID, { selector: '.observability-span-links code' })).toBeInTheDocument();
    const logsRegion = screen.getByRole('region', { name: '关联服务端日志' });
    expect(await within(logsRegion).findByText('request accepted')).toBeInTheDocument();
    expect(receivedLogsURLs[0]?.pathname).toBe(`/api/admin/observability/traces/${primaryTraceID}/logs`);
    expect(receivedLogsURLs[0]?.searchParams.get('request_id')).toBe('request-1');
    expect(receivedLogsURLs[0]?.searchParams.get('task_id')).toBe('task-1');
    await user.click(within(logsRegion).getByRole('button', { name: '更多日志' }));
    expect(await within(logsRegion).findByText('task checkpoint committed')).toBeInTheDocument();
    expect(receivedLogsURLs.at(-1)?.searchParams.get('cursor')).toBe('log-next');
    await user.click(within(logsRegion).getByRole('button', { name: '上一页' }));
    expect(await within(logsRegion).findByText('request accepted')).toBeInTheDocument();
    const sessionSummary = await screen.findByText((_, element) => element?.tagName === 'SUMMARY' && Boolean(element.textContent?.includes('主 Agent · session-1')));
    const sessionDetails = sessionSummary.closest('details')!;
    expect(sessionDetails).not.toHaveAttribute('open');
    await user.click(sessionSummary);
    expect(sessionDetails).toHaveAttribute('open');
    expect(await screen.findByText('tool.call')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: '更多事件' }));
    expect(await screen.findByText('tool.result')).toBeInTheDocument();
    const eventsRegion = screen.getByRole('region', { name: 'Harness / DSH 执行事件' });
    await user.click(within(eventsRegion).getByRole('button', { name: '上一页' }));
    expect(await screen.findByText('tool.call')).toBeInTheDocument();
    expect(within(eventsRegion).getByText('序号 1')).toBeInTheDocument();
  });
});
