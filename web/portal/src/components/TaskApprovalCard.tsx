import { useEffect, useRef, useState } from 'react';
import { formatDate } from '../app/format';
import type { components } from '../api/schema';
import { TaskApprovalMcpApp, type McpApprovalAppData } from './TaskApprovalMcpApp';

type ApprovalView = Pick<components['schemas']['TaskPendingRequest'],
  'id' | 'version' | 'status' | 'kind' | 'title' | 'object' | 'content' | 'impact' | 'disclosures' |
  'expires_at' | 'media_preview_operation_id'> &
  Partial<Pick<components['schemas']['TaskApprovalSnapshot'], 'decision' | 'responded_at'>>;

export type TaskApprovalDecision = 'approve' | 'reject';

const decisionNotice = '审批范围仅限本次显示的操作。允许后任务会继续尝试执行，最终结果以任务状态为准；拒绝后本次操作不会执行。';

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

export function TaskApprovalCard({ approval, submitting = false, compact = false, reviewInbox = false, onDecision }: {
  approval: ApprovalView;
  submitting?: boolean;
  compact?: boolean;
  reviewInbox?: boolean;
  onDecision: (decision: TaskApprovalDecision) => void | Promise<unknown>;
}) {
  const [now, setNow] = useState(Date.now);
  const [detailsOpen, setDetailsOpen] = useState(false);
  const detailsRef = useRef<HTMLDialogElement>(null);
  const closeDetails = () => {
    const dialog = detailsRef.current;
    if (dialog && typeof dialog.close === 'function' && dialog.open) dialog.close();
    else {
      dialog?.removeAttribute('open');
      setDetailsOpen(false);
    }
  };
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
  const actionable = approval.status === 'pending' && !invalidExpiry && !locallyExpired && !approval.media_preview_operation_id;
  const status = locallyExpired && approval.status === 'pending' ? '到期，正在核对状态'
    : ({ pending: '待审核', approved: '已允许', rejected: '已拒绝', cancelled: '已取消', expired: '已过期' } as const)[approval.status];
  const reviewStatus = approval.status === 'pending' && approval.media_preview_operation_id ? '媒体预览暂不可用，无法处理此审批。'
    : invalidExpiry && approval.status === 'pending' ? '到期信息无效，正在核对状态' : status;
  const label = approval.status === 'pending' && !locallyExpired ? '需要批准的操作'
    : approval.status === 'expired' || locallyExpired ? '审批已到期' : '已处理的审批';

  useEffect(() => {
    const dialog = detailsRef.current;
    if (!dialog) return;
    if (detailsOpen && !dialog.open) {
      if (typeof dialog.showModal === 'function') dialog.showModal();
      else dialog.setAttribute('open', '');
    } else if (!detailsOpen && dialog.open) {
      if (typeof dialog.close === 'function') dialog.close();
      else dialog.removeAttribute('open');
    }
  }, [detailsOpen]);

  if (actionable) {
    const appApproval: McpApprovalAppData = {
      id: approval.id,
      version: approval.version,
      title: approval.title,
      object: approval.object,
      content: approval.content,
      impact: approval.impact,
      risk: textDisclosure(approval.disclosures?.risk),
      thisCost: costDisclosure(approval.disclosures?.this_cost),
      cumulativeCost: costDisclosure(approval.disclosures?.cumulative_cost),
      alternatives: alternativesDisclosure(approval.disclosures?.alternatives),
      revocation: textDisclosure(approval.disclosures?.revocation),
      expiresAt: approval.expires_at,
    };
    return <article className={`chat-approval task-approval-card${reviewInbox ? ' task-approval-review-card' : ''}`}
      role={reviewInbox ? 'group' : 'region'} aria-label={reviewInbox ? approval.title : label}
      data-approval-id={approval.id} aria-busy={submitting || undefined}>
      <TaskApprovalMcpApp key={`${approval.id}:${approval.version}`} approval={appApproval} onDecision={onDecision} />
    </article>;
  }

  if (reviewInbox) {
    return <article className="chat-approval task-approval-card task-approval-review-card" role="group"
      aria-label={approval.title} data-approval-id={approval.id} aria-busy={submitting || undefined}>
      <button type="button" className="task-approval-review-main" aria-haspopup="dialog"
        aria-label={`查看操作详情：${approval.title}`} onClick={() => setDetailsOpen(true)}>
        <span className="task-approval-review-copy">
          <span className="task-approval-review-kind"><span aria-hidden="true" />操作确认</span>
          <span className="task-approval-review-title">{approval.title}</span>
          <span className="task-approval-review-object">
            {approval.object}{approval.content ? ` · ${approval.content}` : ''}
          </span>
          <span className="task-approval-review-decision-note">{decisionNotice}</span>
        </span>
      </button>
      <div className="task-approval-review-actions">
        {actionable ? <>
          <button type="button" className="button button-secondary" disabled={submitting}
            aria-label={`拒绝：${approval.title}`} onClick={() => onDecision('reject')}>拒绝</button>
          <button type="button" className="button button-primary" disabled={submitting}
            aria-label={`允许：${approval.title}`} onClick={() => onDecision('approve')}>允许</button>
        </> : <span className="task-approval-review-status" role="status">{reviewStatus}</span>}
      </div>
      <dialog ref={detailsRef} className="task-approval-review-dialog" aria-labelledby={`approval-title-${approval.id}`}
        onClose={() => setDetailsOpen(false)}
        onClick={(event) => { if (event.target === detailsRef.current) closeDetails(); }}>
        <header className="task-approval-review-dialog-heading">
          <div><p>操作详情</p><h2 id={`approval-title-${approval.id}`}>{approval.title}</h2></div>
          <button type="button" className="task-approval-review-dialog-close" aria-label="关闭"
            onClick={closeDetails}>×</button>
        </header>
        <dl className="task-approval-review-details">
          <dt>操作对象</dt><dd>{approval.object}</dd>
          <dt>操作内容</dt><dd>{approval.content}</dd>
          <dt>可能影响</dt><dd>{approval.impact}</dd>
          <dt>风险</dt><dd>{textDisclosure(approval.disclosures?.risk)}</dd>
          <dt>本次费用</dt><dd>{costDisclosure(approval.disclosures?.this_cost)}</dd>
          <dt>累计费用</dt><dd>{costDisclosure(approval.disclosures?.cumulative_cost)}</dd>
          <dt>替代方案</dt><dd>{alternativesDisclosure(approval.disclosures?.alternatives)}</dd>
          <dt>撤销方式</dt><dd>{textDisclosure(approval.disclosures?.revocation)}</dd>
          {approval.expires_at && <><dt>审批到期</dt><dd>{formatDate(approval.expires_at)}</dd></>}
        </dl>
        {actionable && <p>{decisionNotice}</p>}
        {actionable && <div className="chat-approval-actions">
          <button type="button" className="button button-secondary" disabled={submitting}
            aria-label={`拒绝：${approval.title}`} onClick={() => onDecision('reject')}>拒绝本次操作</button>
          <button type="button" className="button button-primary" disabled={submitting}
            aria-label={`允许：${approval.title}`} onClick={() => onDecision('approve')}>允许本次操作</button>
        </div>}
      </dialog>
    </article>;
  }

  return <article className="chat-approval task-approval-card" role="region" aria-label={label} data-approval-id={approval.id}>
    <header className="task-approval-card-heading"><h3>{approval.title}</h3><span>{status}</span></header>
    <p className="task-approval-card-object">{approval.object}</p>
    <p>{approval.content}</p>
    <p>可能影响：{approval.impact}</p>
    <dl className="task-approval-disclosures" aria-label={compact ? '风险和本次费用' : '风险、费用和其他审批信息'}>
      <dt>风险</dt><dd>{textDisclosure(approval.disclosures?.risk)}</dd>
      <dt>本次费用</dt><dd>{costDisclosure(approval.disclosures?.this_cost)}</dd>
      {!compact && <><dt>累计费用</dt><dd>{costDisclosure(approval.disclosures?.cumulative_cost)}</dd>
        <dt>替代方案</dt><dd>{alternativesDisclosure(approval.disclosures?.alternatives)}</dd>
        <dt>撤销方式</dt><dd>{textDisclosure(approval.disclosures?.revocation)}</dd></>}
    </dl>
    {actionable && <p>{decisionNotice}</p>}
    {compact && <details className="task-approval-more"><summary>查看操作详情</summary>
      <dl className="task-approval-disclosures" aria-label="其他审批信息">
        <dt>累计费用</dt><dd>{costDisclosure(approval.disclosures?.cumulative_cost)}</dd>
        <dt>替代方案</dt><dd>{alternativesDisclosure(approval.disclosures?.alternatives)}</dd>
        <dt>撤销方式</dt><dd>{textDisclosure(approval.disclosures?.revocation)}</dd>
      </dl>
    </details>}
    {approval.expires_at && <p>审批到期：{formatDate(approval.expires_at)}</p>}
    {approval.responded_at && <p>处理时间：{formatDate(approval.responded_at)}</p>}
    {approval.status === 'expired' ? <p role="status">审批已到期；该操作已被拒绝并未执行。</p>
      : locallyExpired && approval.status === 'pending' ? <p role="status">审批已到期，正在核对服务端状态；当前不能继续操作。</p>
        : invalidExpiry && approval.status === 'pending' ? <p role="status">审批到期信息无效，当前不能继续操作。</p>
          : approval.status === 'pending' && approval.media_preview_operation_id ? <p role="status">媒体预览暂不可用，无法处理此审批。</p>
          : approval.status === 'cancelled' ? <p role="status">此审批已取消。</p>
            : approval.status === 'approved' || approval.status === 'rejected' ? <p role="status">审批已处理；任务后续状态等待更新。</p>
              : actionable && <div className="chat-approval-actions">
                <button type="button" className="button button-primary" disabled={submitting} onClick={() => onDecision('approve')}>允许本次操作</button>
                <button type="button" className="button button-secondary" disabled={submitting} onClick={() => onDecision('reject')}>拒绝</button>
              </div>}
  </article>;
}
