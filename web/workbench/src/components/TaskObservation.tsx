import { useEffect, useState } from 'react';
import { ApiError } from '../api/client';
import { gatewayApi, type GatewayTaskObservationSnapshot } from '../api/gateway';

type ObservationEvent = { sequence: number; type: string; created_at: string };

const taskStatusLabels: Record<string, string> = {
  queued: '排队中', running: '运行中', waiting_user: '等待确认', paused: '已暂停', pausing: '正在暂停',
  cancelling: '正在停止', completed: '已完成', failed: '失败', cancelled: '已取消',
};
const terminalTaskStatuses = new Set(['completed', 'failed', 'cancelled']);

function validSnapshot(value: GatewayTaskObservationSnapshot, taskID: string): boolean {
  return value !== null && typeof value === 'object' && value.task_id === taskID
    && typeof value.host_id === 'string' && value.host_id.length > 0
    && typeof value.status === 'string' && value.status.length > 0 && value.status.length <= 64
    && Number.isSafeInteger(value.event_sequence) && value.event_sequence >= 0
    && Number.isFinite(Date.parse(value.updated_at)) && Array.isArray(value.artifacts)
    && value.artifacts.every((artifact) => artifact !== null && typeof artifact === 'object'
      && typeof artifact.id === 'string' && artifact.id.length > 0
      && typeof artifact.label === 'string' && artifact.label.length <= 512
      && typeof artifact.media_type === 'string' && artifact.media_type.length <= 256
      && typeof artifact.workspace_id === 'string' && artifact.workspace_id.length > 0
      && Number.isSafeInteger(artifact.revision) && artifact.revision >= 0
      && Number.isSafeInteger(artifact.size_bytes) && artifact.size_bytes >= 0
      && /^[a-f0-9]{64}$/.test(artifact.sha256) && Number.isFinite(Date.parse(artifact.created_at)));
}

function validEvent(value: unknown): value is ObservationEvent {
  if (value === null || typeof value !== 'object' || Array.isArray(value)) return false;
  const event = value as Record<string, unknown>;
  return Number.isSafeInteger(event.sequence) && Number(event.sequence) > 0
    && typeof event.type === 'string' && event.type.length > 0 && event.type.length <= 64
    && typeof event.created_at === 'string' && Number.isFinite(Date.parse(event.created_at));
}

function eventLabel(type: string): string {
  switch (type) {
    case 'task.status': return '任务状态已更新';
    case 'task.step': return '执行步骤已更新';
    case 'task.artifact': return '已核验产物有更新';
    case 'task.waiting_user': return '任务等待确认';
    case 'task.result': return '任务结果已更新';
    default: return '任务进度已更新';
  }
}

function observationError(error: unknown): string {
  if (error instanceof ApiError && [401, 403, 404].includes(error.status)) return 'Task 观察授权已失效或已撤销；请从 Portal Assistant 重新打开。';
  return 'Task 只读观察暂不可用，请重试。';
}

export default function TaskObservation({ taskID }: { taskID: string }) {
  const [snapshot, setSnapshot] = useState<GatewayTaskObservationSnapshot | null>(null);
  const [events, setEvents] = useState<ObservationEvent[]>([]);
  const [connection, setConnection] = useState<'connecting' | 'connected' | 'disconnected'>('connecting');
  const [error, setError] = useState('');
  const [retry, setRetry] = useState(0);
  const invalidTaskID = !taskID || taskID.length > 128;

  const retryRead = () => {
    setSnapshot(null);
    setEvents([]);
    setError('');
    setConnection('connecting');
    setRetry((value) => value + 1);
  };

  useEffect(() => {
    let current = true;
    let revoked = false;
    let source: EventSource | null = null;
    let retryTimer: number | undefined;
    let cursor = 0;

    const closeSource = () => {
      source?.close();
      source = null;
    };
    const readSnapshot = async () => {
      const value = await gatewayApi.taskObservation(taskID);
      if (!validSnapshot(value, taskID)) throw new Error('Task 观察快照无效');
      if (current) {
        cursor = Math.max(cursor, value.event_sequence);
        setSnapshot(value);
        setError('');
      }
      return value;
    };
    const connect = () => {
      if (!current || revoked) return;
      source = new EventSource(gatewayApi.taskObservationEventsURL(taskID, cursor), { withCredentials: true });
      source.onopen = () => current && setConnection('connected');
      source.addEventListener('task.event', (rawEvent) => {
        const message = rawEvent as MessageEvent<string>;
        let value: unknown;
        try { value = JSON.parse(message.data); } catch { return; }
        if (!validEvent(value) || value.sequence <= cursor || Number(message.lastEventId) !== value.sequence) return;
        cursor = Number(message.lastEventId);
        setEvents((previous) => [...previous, value].slice(-12));
        setConnection('connected');
        void readSnapshot().then((value) => {
          if (terminalTaskStatuses.has(value.status)) closeSource();
        }).catch((reason: unknown) => {
          if (!current) return;
          revoked = true;
          closeSource();
          setSnapshot(null);
          setEvents([]);
          setError(observationError(reason));
        });
      });
      source.addEventListener('task.snapshot_required', () => {
        closeSource();
        setConnection('connecting');
        void readSnapshot().then((value) => {
          if (current && !revoked) {
            if (terminalTaskStatuses.has(value.status)) setConnection('connected');
            else connect();
          }
        }).catch((reason: unknown) => {
          if (!current) return;
          revoked = true;
          setSnapshot(null);
          setEvents([]);
          setError(observationError(reason));
        });
      });
      source.onerror = () => {
        closeSource();
        if (!current || revoked) return;
        setConnection('disconnected');
        void readSnapshot().then((value) => {
          if (current && !revoked) {
            if (terminalTaskStatuses.has(value.status)) setConnection('connected');
            else retryTimer = window.setTimeout(connect, 1000);
          }
        }).catch((reason: unknown) => {
          if (!current) return;
          revoked = true;
          setSnapshot(null);
          setEvents([]);
          setError(observationError(reason));
        });
      };
    };

    if (!invalidTaskID) {
      void readSnapshot().then((value) => {
        if (terminalTaskStatuses.has(value.status)) setConnection('connected');
        else connect();
      }).catch((reason: unknown) => {
        if (current) setError(observationError(reason));
      });
    }
    return () => {
      current = false;
      closeSource();
      if (retryTimer !== undefined) window.clearTimeout(retryTimer);
    };
  }, [invalidTaskID, retry, taskID]);

  const visibleError = invalidTaskID ? 'Task ID 无效，请从 Portal Assistant 重新打开。' : error;

  return <main className="task-observation" aria-labelledby="task-observation-title">
    <header><p className="eyebrow">Workbench · 只读</p><h1 id="task-observation-title">任务观察</h1>
      <p className="muted">此页面只读取当前 Task 状态、事件和服务端核验过的产物元数据。</p></header>
    {visibleError && <section className="task-observation-card" role="alert"><p>{visibleError}</p>
      <button className="button button-secondary" type="button" onClick={retryRead} disabled={invalidTaskID}>重试读取</button>
    </section>}
    {!snapshot && !visibleError && <p className="task-observation-card" role="status">正在验证 Task 观察权限…</p>}
    {snapshot && <>
      <section className="task-observation-card" aria-label="任务状态">
        <h2>Task {snapshot.task_id}</h2>
        <p>状态：{taskStatusLabels[snapshot.status] ?? '处理中'}</p>
        <p>最近更新：{new Date(snapshot.updated_at).toLocaleString()}</p>
        <p>已确认事件序号：{snapshot.event_sequence}</p>
        <p role="status">事件连接：{connection === 'connected' ? '实时更新中' : connection === 'disconnected' ? '连接中断，正在按序号续读' : '正在连接'}</p>
      </section>
      <section className="task-observation-card" aria-label="任务事件">
        <h2>任务事件</h2>
        {events.length ? <ol>{events.map((event) => <li key={event.sequence}>
          <span>#{event.sequence} · {eventLabel(event.type)}</span><time dateTime={event.created_at}>{new Date(event.created_at).toLocaleString()}</time>
        </li>)}</ol> : <p className="muted">尚无新事件；重连时会从最后确认的事件序号续读。</p>}
      </section>
      <section className="task-observation-card" aria-label="已核验产物">
        <h2>已核验产物</h2>
        {snapshot.artifacts.length ? <ul>{snapshot.artifacts.map((artifact) => <li key={artifact.id}>
          <strong>{artifact.label}</strong><span>{artifact.media_type} · Workspace {artifact.workspace_id} v{artifact.revision}</span>
          <span>{artifact.size_bytes} bytes · SHA-256 {artifact.sha256}</span>
          <time dateTime={artifact.created_at}>{new Date(artifact.created_at).toLocaleString()}</time>
        </li>)}</ul> : <p className="muted">当前没有可显示的已核验产物。</p>}
      </section>
    </>}
  </main>;
}
