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
  sources: { spans: available, dsh_events: { status: 'disabled', reason: 'DSH 事件存储未启用。', updated_at: null } },
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
    expect(screen.getByText(/Harness 未记录的内部推理过程不会显示/)).toBeInTheDocument();
    expect(screen.getByText('0 ms')).toBeInTheDocument();
  });

  it('shows partial source coverage and the returned reason beside trace results', async () => {
    const partial: Overview = {
      ...emptyOverview,
      sources: {
        spans: { status: 'partial', reason: '部分服务实例尚未接入。', updated_at: null },
        dsh_events: { status: 'unavailable', reason: 'DSH 事件存储连接失败。', updated_at: null },
      },
    };
    server.use(
      http.get('*/api/admin/observability/overview', () => HttpResponse.json(partial)),
      http.get('*/api/admin/observability/traces', () => HttpResponse.json({ ...emptyTracePage, sources: { spans: partial.sources.spans } })),
    );
    renderPage();

    expect((await screen.findAllByText('部分服务实例尚未接入。')).length).toBeGreaterThan(0);
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

  it('loads span detail and paginates collapsed DSH event records', async () => {
    const user = userEvent.setup();
    const traceItem: components['schemas']['AdminObservabilityTraceSummary'] = {
      trace_id: 'trace-1', request_id: 'request-1', task_id: 'task-1', started_at: '2026-11-27T10:00:00Z', ended_at: null,
      status: 'ok', span_count: 1, error_count: 0,
    };
    const sessions: components['schemas']['AdminObservabilityAgentSession'][] = [{ session_id: 'session-1', parent_session_id: null, step_id: 'step-1', role: 'main' }];
    const events: components['schemas']['AdminObservabilityDSHEvent'][] = [
      { session_id: 'session-1', sequence: 1, time: '2026-11-27T10:00:01Z', type: 'tool.call', data: { tool: 'search' } },
      { session_id: 'session-1', sequence: 2, time: '2026-11-27T10:00:02Z', type: 'tool.result', data: { ok: true } },
    ];
    server.use(
      http.get('*/api/admin/observability/traces', () => HttpResponse.json({ ...emptyTracePage, items: [traceItem] })),
      http.get('*/api/admin/observability/traces/:trace_id', () => HttpResponse.json({
        trace_id: 'trace-1', coverage: available,
        spans: [{ span_id: 'span-1', parent_span_id: null, name: 'agent.run', kind: 'internal', status: 'ok', start_at: '2026-11-27T10:00:00Z', end_at: null, attributes: { tool_count: 1 }, events: [] }],
      } satisfies Trace)),
      http.get('*/api/admin/observability/traces/:trace_id/events', ({ request }) => {
        const fromSequence = new URL(request.url).searchParams.get('from_sequence');
        const secondPage = fromSequence === '2';
        return HttpResponse.json({ task_id: 'task-1', agent_sessions: sessions, events: [events[secondPage ? 1 : 0]!], next_sequence: secondPage ? null : 2, coverage: available } satisfies EventPage);
      }),
    );
    renderPage();

    await user.click(await screen.findByRole('button', { name: 'trace-1' }));
    expect(await screen.findByText('agent.run')).toBeInTheDocument();
    const sessionSummary = await screen.findByText((_, element) => element?.tagName === 'SUMMARY' && Boolean(element.textContent?.includes('main · session-1')));
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
