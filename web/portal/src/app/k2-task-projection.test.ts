import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';
import { readK2TaskProjection } from './k2-task-projection';

const sharedContract = JSON.parse(readFileSync(
  resolve(process.cwd(), '../../docs/实施/Morde个人Muse三会话并行实施/.local/muse-a00-contract-v1.json'), 'utf8',
)) as { cases: Array<{ name: string; tasks?: unknown[] }> };

describe('readK2TaskProjection', () => {
  it('keeps legacy Tasks compatible when target K2 fields are absent', () => {
    expect(readK2TaskProjection({ id: 'q1234567', status: 'running' })).toEqual({ kind: 'legacy' });
  });

  it('reads the shared K2 sample and preserves server-computed dependency satisfaction', () => {
    const sample = sharedContract.cases.find((item) => item.name === 'k2_parallel_tasks_revisions_children_and_review')?.tasks?.[1];
    expect(sample).toBeDefined();
    const result = readK2TaskProjection(sample);

    expect(result).toMatchObject({
      kind: 'available',
      projection: {
        dependencies: [{ task_id: 'q0000001', required_revision: 2, task_status: 'running', satisfied: false }],
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
});
