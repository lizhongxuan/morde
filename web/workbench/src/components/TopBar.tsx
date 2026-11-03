import { useEffect, useState } from 'react';
import type { GatewayControl, GatewayHost } from '../api/gateway';

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
  host: GatewayHost | null;
  renewing: boolean;
  onRenew: () => void;
  control: GatewayControl | null;
  controlLoading: boolean;
  controlError: string;
  controlActionError: string;
  controlActionLabel: string;
  controlActionPending: boolean;
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
  host,
  renewing,
  onRenew,
  control,
  controlLoading,
  controlError,
  controlActionError,
  controlActionLabel,
  controlActionPending,
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

  return (
    <header className="top-bar">
      <div className="host-summary">
        <strong className="host-name">{host?.name || (host ? host.id : '正在读取主机…')}</strong>
        <span className="host-state" data-state={host?.state || 'loading'}>
          {host ? stateNames[host.state] || host.state : '连接中'}
        </span>
        <span className="host-expiry" aria-live="off">
          {host ? formatRemaining(host.expires_at, now) : ''}
        </span>
        <span className="control-state" data-state={control?.control_state || 'loading'} role="status">
          {controlLabel}
        </span>
        {control && <span className="control-version">控制版本 {control.control_version}</span>}
        {control?.control_state === 'user' && <span className="control-lease">
          {control.control_lease_active
            ? `操作租约${control.control_lease_held_by_me ? '由你持有' : '有效'} · ${formatRemaining(control.control_lease_expires_at, now)}`
            : '当前没有有效操作租约'}
        </span>}
        <p className="desktop-sharing-notice">桌面画面可能显示此电脑上的文件，获准查看桌面的成员也能看到。</p>
      </div>
      <div className="top-actions">
        {controlActionError && <span className="control-error" role="alert">{controlActionError}</span>}
        {controlError && <button className="button button-secondary" type="button" onClick={onRetryControl} disabled={controlLoading}>重试读取</button>}
        {controlActionLabel && (
          <button className={`button${controlActionLabel.includes('接管') ? '' : ' button-secondary'}`} type="button" onClick={onControlAction} disabled={controlActionPending}>
            {controlActionLabel}
          </button>
        )}
        <button className="button" type="button" onClick={onRenew} disabled={!host || renewing}>
          {renewing ? '续期中…' : '续期'}
        </button>
      </div>
    </header>
  );
}
