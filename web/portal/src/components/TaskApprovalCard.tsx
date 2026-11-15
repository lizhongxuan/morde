import { useEffect, useState } from 'react';
import { formatDate } from '../app/format';
import type { components } from '../api/schema';

type ApprovalView = Pick<components['schemas']['TaskPendingRequest'],
  'id' | 'version' | 'status' | 'kind' | 'title' | 'object' | 'content' | 'impact' | 'disclosures' | 'expires_at'> &
  Partial<Pick<components['schemas']['TaskApprovalSnapshot'], 'decision' | 'responded_at'>>;

export type TaskApprovalDecision = 'approve' | 'reject';

function textDisclosure(value: components['schemas']['TaskApprovalTextDisclosure'] | undefined): string {
  if (!value) return '未知';
  if (value.state !== 'known') return value.state === 'not_applicable' ? '不适用' : '未知';
  return value.summary?.trim() || '服务端标记为已知，但未提供说明';
}

function costDisclosure(value: components['schemas']['TaskApprovalCostDisclosure'] | undefined): string {
  if (!value) return '未知';
  if (value.state !== 'known') return value.state === 'not_applicable' ? '不适用' : '未知';
  if (value.amount_micros === null || !Number.isSafeInteger(value.amount_micros) || value.amount_micros < 0 ||
    !value.currency || !/^[A-Z]{3}$/.test(value.currency) || !value.basis) return '费用数据不完整';
  const whole = Math.floor(value.amount_micros / 1_000_000).toLocaleString('zh-CN');
  const fraction = String(value.amount_micros % 1_000_000).padStart(6, '0').replace(/0+$/u, '');
  return `${value.basis === 'estimate' ? '估算' : '已结算'} ${value.currency} ${whole}${fraction ? `.${fraction}` : ''}`;
}

function alternativesDisclosure(value: components['schemas']['TaskApprovalAlternativesDisclosure'] | undefined): string {
  if (!value) return '未知';
  if (value.state !== 'known') return value.state === 'not_applicable' ? '不适用' : '未知';
  return value.options.length ? value.options.join('；') : '服务端标记为已知，但未提供方案';
}

export function TaskApprovalCard({ approval, submitting = false, onDecision }: {
  approval: ApprovalView;
  submitting?: boolean;
  onDecision: (decision: TaskApprovalDecision) => void;
}) {
  const [now, setNow] = useState(Date.now);
  useEffect(() => {
    if (!approval.expires_at) return;
    const expiresAt = Date.parse(approval.expires_at);
    if (!Number.isFinite(expiresAt)) return;
    const timer = window.setTimeout(() => setNow(Date.now()), Math.max(0, expiresAt - Date.now() + 1));
    return () => window.clearTimeout(timer);
  }, [approval.expires_at]);
  const expiresAt = approval.expires_at === null ? null : Date.parse(approval.expires_at);
  const invalidExpiry = expiresAt !== null && !Number.isFinite(expiresAt);
  const locallyExpired = expiresAt !== null && Number.isFinite(expiresAt) && expiresAt <= now;
  const actionable = approval.status === 'pending' && !invalidExpiry && !locallyExpired;
  const status = locallyExpired && approval.status === 'pending' ? '到期，正在核对状态'
    : ({ pending: '待审核', approved: '已允许', rejected: '已拒绝', cancelled: '已取消', expired: '已过期' } as const)[approval.status];
  const label = approval.status === 'pending' && !locallyExpired ? '需要批准的操作'
    : approval.status === 'expired' || locallyExpired ? '审批已到期' : '已处理的审批';

  return <article className="chat-approval task-approval-card" role="region" aria-label={label} data-approval-id={approval.id}>
    <header className="task-approval-card-heading"><h3>{approval.title}</h3><span>{status}</span></header>
    <p className="task-approval-card-object">{approval.object}</p>
    <p>{approval.content}</p>
    <p>可能影响：{approval.impact}</p>
    <dl className="task-approval-disclosures" aria-label="风险、费用和其他审批信息">
      <dt>风险</dt><dd>{textDisclosure(approval.disclosures?.risk)}</dd>
      <dt>本次费用</dt><dd>{costDisclosure(approval.disclosures?.this_cost)}</dd>
      <dt>累计费用</dt><dd>{costDisclosure(approval.disclosures?.cumulative_cost)}</dd>
      <dt>替代方案</dt><dd>{alternativesDisclosure(approval.disclosures?.alternatives)}</dd>
      <dt>撤销方式</dt><dd>{textDisclosure(approval.disclosures?.revocation)}</dd>
    </dl>
    {approval.expires_at && <p>审批到期：{formatDate(approval.expires_at)}</p>}
    {approval.responded_at && <p>处理时间：{formatDate(approval.responded_at)}</p>}
    {approval.status === 'expired' ? <p role="status">审批已到期；该操作已被拒绝并未执行。</p>
      : locallyExpired && approval.status === 'pending' ? <p role="status">审批已到期，正在核对服务端状态；当前不能继续操作。</p>
        : invalidExpiry && approval.status === 'pending' ? <p role="status">审批到期信息无效，当前不能继续操作。</p>
          : approval.status === 'cancelled' ? <p role="status">此审批已取消。</p>
            : approval.status === 'approved' || approval.status === 'rejected' ? <p role="status">审批已处理；任务后续状态等待更新。</p>
              : actionable && <div className="chat-approval-actions">
                <button type="button" className="button button-primary" disabled={submitting} onClick={() => onDecision('approve')}>允许</button>
                <button type="button" className="button button-secondary" disabled={submitting} onClick={() => onDecision('reject')}>拒绝</button>
              </div>}
  </article>;
}
