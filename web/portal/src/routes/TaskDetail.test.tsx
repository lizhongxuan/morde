import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, cleanup, render, screen, waitFor } from '@testing-library/react';
import type { ReactNode } from 'react';
import { MemoryRouter, Route, Routes } from 'react-router';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { portalApi } from '../app/api';
import { ToastProvider } from '../app/toast';
import { TaskDetail } from './TaskDetail';

const timestamp = '2026-10-22T08:00:00.000Z';
const task = {
  id: 'q1234567', owner_id: 'u1234567', host_id: 'h1234567', conversation_id: 'c1234567', model_id: 'test-model',
  objective: '整理项目改动', status: 'running', current_step: '读取文件', result: null, waiting_reason: null,
  budget: { active_seconds: 1800, tool_calls: 100, model_tokens: 50000 },
  usage: { active_seconds: 15, tool_calls: 2, model_tokens: null },
  steps: [{ id: 's1234567', sequence: 1, kind: 'tool', status: 'running', title: '读取文件', summary: '正在读取项目内容。', created_at: timestamp }],
  batch_items: [], artifacts: [], pending_request: null, clarification_request: null, stop_request: null,
  created_at: timestamp, updated_at: timestamp, finished_at: null,
} satisfies Awaited<ReturnType<typeof portalApi.task>>;

class TestEventSource {
  static instances: TestEventSource[] = [];
  private listeners = new Map<string, Set<EventListener>>();

  constructor() { TestEventSource.instances.push(this); }
  addEventListener(type: string, listener: EventListener) {
    this.listeners.set(type, new Set(this.listeners.get(type)).add(listener));
  }
  removeEventListener(type: string, listener: EventListener) {
    this.listeners.get(type)?.delete(listener);
  }
  close() {}
  emit(type: string, data: string) {
    const event = new MessageEvent(type, { data });
    for (const listener of this.listeners.get(type) ?? []) listener(event);
  }
}

function renderTask() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, refetchInterval: false } } });
  const child: ReactNode = <Routes><Route path="/tasks/:id" element={<TaskDetail />} /></Routes>;
  return render(<QueryClientProvider client={client}><ToastProvider><MemoryRouter initialEntries={[`/tasks/${task.id}`]}>{child}</MemoryRouter></ToastProvider></QueryClientProvider>);
}

describe('TaskDetail event recovery', () => {
  afterEach(() => { cleanup(); vi.restoreAllMocks(); vi.unstubAllGlobals(); TestEventSource.instances = []; });

  it('does not refetch the task for each streamed assistant text delta', async () => {
    vi.stubGlobal('EventSource', TestEventSource);
    const getTask = vi.spyOn(portalApi, 'task').mockResolvedValue(task);
    renderTask();

    expect(await screen.findByRole('heading', { name: '整理项目改动' })).toBeInTheDocument();
    const [stream] = TestEventSource.instances;
    await act(async () => {
      stream!.emit('task.message', JSON.stringify({ payload: { role: 'assistant', message_id: 'm1234567', turn_id: 's1234567', delta: '回复片段' } }));
      await new Promise((resolve) => setTimeout(resolve, 0));
    });

    expect(getTask).toHaveBeenCalledTimes(1);
  });

  it('reloads the authoritative task snapshot after a cursor expires', async () => {
    vi.stubGlobal('EventSource', TestEventSource);
    const getTask = vi.spyOn(portalApi, 'task').mockResolvedValue(task);
    renderTask();

    expect(await screen.findByRole('heading', { name: '整理项目改动' })).toBeInTheDocument();
    const [stream] = TestEventSource.instances;
    await act(async () => stream!.emit('task.snapshot_required', JSON.stringify({ sequence: 9 })));

    await waitFor(() => expect(getTask).toHaveBeenCalledTimes(2));
  });
});
