import { describe, expect, it } from 'vitest';
import { createK2TaskFixture } from '../mocks/k2-task-fixture';
import { readK2TaskProjection } from './k2-task-projection';

describe('readK2TaskProjection', () => {
  it('keeps legacy Tasks compatible when target K2 fields are absent', () => {
    expect(readK2TaskProjection({ id: 'q1234567', status: 'running' })).toEqual({ kind: 'legacy' });
  });

  it('reads the local K2 fixture and preserves server-computed dependency satisfaction', () => {
    const sample = createK2TaskFixture('u1234567', 'm1234567', '2026-11-24T00:00:00Z').tasks[2];
    const result = readK2TaskProjection(sample);

    expect(result).toMatchObject({
      kind: 'available',
      projection: {
        dependencies: [{ task_id: 'q0000002', required_revision: 3, task_status: 'completed', satisfied: false }],
        child_jobs: [],
        result_review: null,
      },
    });
  });

  it('rejects partial or malformed K2 projections instead of silently treating them as final state', () => {
    expect(readK2TaskProjection({ dependencies: [] })).toEqual({ kind: 'invalid' });
    expect(readK2TaskProjection({ dependencies: null, child_jobs: [], result_review: null })).toEqual({ kind: 'invalid' });
    expect(readK2TaskProjection({
      dependencies: [{ task_id: 'q1234567', required_revision: 3, task_status: 'completed', satisfied: 'yes' }],
      child_jobs: [], result_review: null,
    })).toEqual({ kind: 'invalid' });
  });

  it('accepts missing or null review retry IDs for older snapshots and rejects malformed IDs', () => {
    const review = { state: 'failed', source_task_id: 'q1234567', source_task_revision: 1,
      source_event_sequence: 2, reply_message_id: null };
    const task = { dependencies: [], child_jobs: [], result_review: review };
    expect(readK2TaskProjection(task).kind).toBe('available');
    expect(readK2TaskProjection({ ...task, result_review: { ...review, trigger_message_id: null } }).kind).toBe('available');
    expect(readK2TaskProjection({ ...task, result_review: { ...review, trigger_message_id: 12 } })).toEqual({ kind: 'invalid' });
  });
});
