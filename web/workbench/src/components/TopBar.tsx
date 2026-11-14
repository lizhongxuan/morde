import { useEffect, useState } from 'react';
import type { GatewayControl, GatewayHost } from '../api/gateway';
import { returnToPreviousPage } from '../app/navigation';
import FloatingError from './FloatingError';

function formatRemaining(expiresAt: string | null, now: number): string {
  if (!expiresAt) return '到期时间未知';
  const expires = Date.parse(expiresAt);
  if (!Number.isFinite(expires)) return '到期时间未知';
  const seconds = Math.floor((expires - now) / 1000);
  if (seconds <= 0) return '已到期';
  const hours = Math.floor(seconds / 3600);
  const minutes = Math.floor((seconds % 3600) / 60);
  const rest = seconds % 60;
  return `剩余 ${hours.toString().padStart(2, '0')}:${minutes.toString().padStart(2, '0')}:${rest.toString().padStart(2, '0')}`;
}

type Props = {
  embedded?: boolean;
  host: GatewayHost | null;
  renewing: boolean;
  onRenew: () => void;
  control: GatewayControl | null;
  controlLoading: boolean;
  controlError: string;
  controlActionError: string;
  controlActionLabel: string;
  controlActionPending: boolean;
  notice?: string;
  noticeKind?: 'success' | 'error';
  onControlAction: () => void;
  onRetryControl: () => void;
};

const stateNames: Record<string, string> = {
  creating: '创建中',
  running: '运行中',
  deleting: '删除中',
  failed: '启动失败',
};

export default function TopBar({
  embedded = false,
  host,
  renewing,
  onRenew,
  control,
  controlLoading,
  controlError,
  controlActionError,
  controlActionLabel,
  controlActionPending,
  notice,
  noticeKind = 'success',
  onControlAction,
  onRetryControl,
}: Props) {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const timer = window.setInterval(() => setNow(Date.now()), 1000);
    return () => window.clearInterval(timer);
  }, []);

  const transitionFailure = control?.active_transition?.failure_reason;
  const controlLabel = controlError ? '控制状态暂时不可用'
    : controlLoading && !control ? '正在读取控制状态…'
      : transitionFailure ? control?.active_transition?.action === 'take_over' ? '接管未完成，可重试' : '交回未完成，可重试'
        : control?.control_state === 'ai' ? '当前控制者：AI · 桌面只读'
          : control?.control_state === 'user' ? control.control_lease_active
            ? control.control_lease_held_by_me ? '你持有电脑操作权' : control.control_actor_user_id ? `当前操作账号：${control.control_actor_user_id}` : '其他用户持有电脑操作权'
            : '操作租约已失效 · 桌面只读'
            : control?.control_state === 'taking_over' ? '正在接管电脑，请稍候…'
              : control?.control_state === 'returning' ? '正在交回 AI，请稍候…' : '控制状态未知';
  const compactControlLabel = controlError ? '控制不可用'
    : controlLoading && !control ? '读取状态'
      : transitionFailure ? control?.active_transition?.action === 'take_over' ? '接管失败' : '交还失败'
        : control?.control_state === 'ai' ? 'AI 控制中 · 只读'
          : control?.control_state === 'user' ? control.control_lease_active
            ? control.control_lease_held_by_me ? '你正在控制' : '他人控制中'
            : '租约已失效'
            : control?.control_state === 'taking_over' ? '接管中'
              : control?.control_state === 'returning' ? '交还中' : '状态未知';

  return (
    <header className={`top-bar${embedded ? ' top-bar-embedded' : ''}`}>
      <div className="host-summary">
        {!embedded && <>
          <strong className="host-name">{host?.name || (host ? host.id : '正在读取主机…')}</strong>
          <span className="host-state" data-state={host?.state || 'loading'}>
            {host ? stateNames[host.state] || host.state : '连接中'}
          </span>
          <span className="host-expiry" aria-live="off">
            {host ? formatRemaining(host.expires_at, now) : ''}
          </span>
        </>}
        <span className="control-state" data-state={control?.control_state || 'loading'} role="status">
          <span className="control-state-full">{controlLabel}</span>
          <span className="control-state-compact">{compactControlLabel}</span>
        </span>
        {control?.control_state === 'user' && <span className="control-lease">
          {control.control_lease_active
            ? `操作租约${control.control_lease_held_by_me ? '由你持有' : '有效'} · ${formatRemaining(control.control_lease_expires_at, now)}`
            : '当前没有有效操作租约'}
        </span>}
        {notice && <span className="workbench-status" data-kind={noticeKind} role={noticeKind === 'error' ? 'alert' : 'status'}>{notice}</span>}
      </div>
      <div className="top-actions">
        {!embedded && <button className="button button-secondary host-return-button" type="button" onClick={returnToPreviousPage}>返回云端电脑</button>}
        {controlActionError && <FloatingError>{controlActionError}</FloatingError>}
        {controlError && <button className="button button-secondary" type="button" onClick={onRetryControl} disabled={controlLoading}>重试读取</button>}
        {controlActionLabel && (
          <button className={`button${controlActionLabel.includes('接管') ? '' : ' button-secondary'}`} type="button" onClick={onControlAction} disabled={controlActionPending}>
            {controlActionLabel}
          </button>
        )}
        {!embedded && <button className="button host-renew-button" type="button" onClick={onRenew} disabled={!host || renewing}>
          {renewing ? '续期中…' : '续期'}
        </button>}
      </div>
    </header>
  );
}
