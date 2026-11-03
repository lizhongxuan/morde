import { cleanup, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { gatewayApi, type GatewayTaskObservationSnapshot } from '../api/gateway';
import TaskObservation from './TaskObservation';

class TestEventSource {
  static instances: TestEventSource[] = [];
  readonly url: string;
  readonly listeners = new Map<string, EventListener>();
  onopen: ((event: Event) => void) | null = null;
  onerror: ((event: Event) => void) | null = null;
  close = vi.fn();

  constructor(url: string) { this.url = url; TestEventSource.instances.push(this); }
  addEventListener(type: string, callback: EventListenerOrEventListenerObject | null) {
    if (typeof callback === 'function') this.listeners.set(type, callback);
  }
  emit(type: string, data: string, lastEventId = '') {
    this.listeners.get(type)?.(new MessageEvent(type, { data, lastEventId }));
  }
}

const taskID = 'q1234567';
const snapshot = (eventSequence: number): GatewayTaskObservationSnapshot => ({
  task_id: taskID,
  host_id: 'h1234567',
  status: 'running',
  event_sequence: eventSequence,
  updated_at: '2026-11-03T12:00:00Z',
  artifacts: [{ id: 'a1234567', label: 'report', media_type: 'text/plain', workspace_id: 'w1234567', revision: 2,
    size_bytes: 15, sha256: 'a'.repeat(64), created_at: '2026-11-03T12:00:00Z' }],
});

describe('TaskObservation', () => {
  afterEach(() => {
    cleanup();
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
    TestEventSource.instances = [];
  });

  it('resumes from the refreshed cursor and displays only event and verified artifact metadata', async () => {
    vi.spyOn(gatewayApi, 'taskObservation').mockResolvedValueOnce(snapshot(4)).mockResolvedValue(snapshot(7));
    vi.stubGlobal('EventSource', TestEventSource);
    render(<TaskObservation taskID={taskID} />);

    expect(await screen.findByText(`Task ${taskID}`)).toBeVisible();
    expect(screen.getByText('report')).toBeVisible();
    await waitFor(() => expect(TestEventSource.instances).toHaveLength(1));
    expect(new URL(TestEventSource.instances[0]!.url).searchParams.get('after')).toBe('4');

    TestEventSource.instances[0]!.emit('task.snapshot_required', '{"current_sequence":7}');
    await waitFor(() => expect(TestEventSource.instances).toHaveLength(2));
    expect(new URL(TestEventSource.instances[1]!.url).searchParams.get('after')).toBe('7');

    TestEventSource.instances[1]!.emit('task.event', JSON.stringify({ sequence: 8, type: 'task.step',
      created_at: '2026-11-03T12:00:10Z', payload: 'private task content' }), '8');
    expect(await screen.findByText('#8 · 执行步骤已更新')).toBeVisible();
    expect(screen.queryByText('private task content')).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /暂停|继续|停止|下载|终端/ })).not.toBeInTheDocument();
    expect(screen.queryByText(/绝对路径|文件正文/)).not.toBeInTheDocument();
  });
});
