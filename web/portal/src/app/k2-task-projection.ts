import type { components } from '../api/schema';

type TaskStatus = components['schemas']['TaskStatus'];
type TaskStepStatus = components['schemas']['TaskStepStatus'];

export type K2TaskDependency = components['schemas']['TaskDependency'];
export type K2ChildJob = components['schemas']['TaskChildJob'];
export type K2ResultReview = components['schemas']['TaskResultReview'];
export type K2TaskProjection = Pick<components['schemas']['TaskSummary'], 'dependencies' | 'child_jobs' | 'result_review'>;

export type K2TaskProjectionRead =
  | { kind: 'legacy' }
  | { kind: 'invalid' }
  | { kind: 'available'; projection: K2TaskProjection };

const taskStatuses = new Set<TaskStatus>([
  'queued', 'running', 'waiting_user', 'pausing', 'paused', 'cancelling', 'completed', 'failed', 'cancelled',
]);
const taskStepStatuses = new Set<TaskStepStatus>(['queued', 'running', 'waiting_user', 'succeeded', 'failed', 'unknown']);
const resultReviewStates = new Set<K2ResultReview['state']>(['pending', 'processing', 'published', 'failed']);
const projectionFields = ['dependencies', 'child_jobs', 'result_review'] as const;

function record(value: unknown): Record<string, unknown> | null {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
    ? value as Record<string, unknown>
    : null;
}

function nonEmptyString(value: unknown): value is string {
  return typeof value === 'string' && value.length > 0;
}

function revision(value: unknown, minimum = 0): value is number {
  return typeof value === 'number' && Number.isSafeInteger(value) && value >= minimum;
}

function isDependency(value: unknown): value is K2TaskDependency {
  const item = record(value);
  return Boolean(item && nonEmptyString(item.task_id) && revision(item.required_revision)
    && typeof item.task_status === 'string' && taskStatuses.has(item.task_status as TaskStatus)
    && typeof item.satisfied === 'boolean');
}

function isChildJob(value: unknown): value is K2ChildJob {
  const item = record(value);
  return Boolean(item && nonEmptyString(item.job_id) && nonEmptyString(item.task_step_id)
    && typeof item.title === 'string' && revision(item.task_revision)
    && typeof item.status === 'string' && taskStepStatuses.has(item.status as TaskStepStatus));
}

function isResultReview(value: unknown): value is K2ResultReview | null {
  if (value === null) return true;
  const item = record(value);
  return Boolean(item && typeof item.state === 'string' && resultReviewStates.has(item.state as K2ResultReview['state'])
    && nonEmptyString(item.source_task_id) && revision(item.source_task_revision)
    && revision(item.source_event_sequence, 1)
    && (item.reply_message_id === null || nonEmptyString(item.reply_message_id)));
}

/** Validates the OpenAPI K2 projection while tolerating older Task snapshots without K2 fields. */
export function readK2TaskProjection(value: unknown): K2TaskProjectionRead {
  const task = record(value);
  if (!task) return { kind: 'invalid' };
  const hasProjection = projectionFields.some((field) => Object.prototype.hasOwnProperty.call(task, field));
  if (!hasProjection) return { kind: 'legacy' };
  if (!projectionFields.every((field) => Object.prototype.hasOwnProperty.call(task, field))) return { kind: 'invalid' };

  const { dependencies, child_jobs: childJobs, result_review: resultReview } = task;
  if (!Array.isArray(dependencies) || !dependencies.every(isDependency)
    || !Array.isArray(childJobs) || !childJobs.every(isChildJob) || !isResultReview(resultReview)) {
    return { kind: 'invalid' };
  }

  return {
    kind: 'available',
    projection: {
      dependencies,
      child_jobs: childJobs,
      result_review: resultReview,
    },
  };
}
