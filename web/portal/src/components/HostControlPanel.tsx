import { useEffect, useRef, useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { errorDetails, newIdempotencyKey, portalApi } from '../app/api';
import type { components } from '../api/schema';
import { useAuth } from '../app/auth';
import { formatDate } from '../app/format';
import { useToast } from '../app/toast';
import { useNetworkReadNotice } from '../app/use-network-read-notice';
import { ConfirmDialog } from './ConfirmDialog';
import { FloatingError } from './FloatingError';

type Host = Awaited<ReturnType<typeof portalApi.hosts>>['items'][number];
type ControlActionRequest = components['schemas']['ControlActionRequest'];
type ControlStatus = Awaited<ReturnType<typeof portalApi.hostControl>>;

const controlStateLabels: Record<ControlStatus['control_state'], string> = {
  ai: '云端 AI 控制', taking_over: '正在接管', user: '人工控制', returning: '正在交还 AI',
};
const controlPhaseLabels: Record<components['schemas']['ControlTransition']['phase'], string> = {
  blocking: '阻止新的 AI 操作', draining: '等待在途工具停止', drained: '工具已停止，正在切换控制权', completed: '已完成',
};
const controlFailureLabels: Record<string, string> = {
  operations_pending: '仍有运行中的工具操作，接管会继续等待停止确认。',
  runtime_unavailable: '云端运行环境暂时不可用，请稍后重试。',
  runtime_quiesce_failed: '运行中的工具尚未确认停止，请重试或检查主机状态。',
  runtime_fence_mismatch: '云端控制状态发生变化，正在重新读取。',
  audit_unavailable: '操作记录服务暂时不可用。',
  task_snapshot_failed: '无法确认当前任务状态，请稍后重试。',
  transition_persistence_failed: '控制状态尚未保存，请核对状态后重试。',
  receipt_invalid: '云端停止回执未通过校验，请联系管理员。',
  runtime_control_failed: '云端运行环境未确认切换控制权，请重试。',
  invalid_action: '当前控制操作无效，请刷新主机状态。',
};

export function HostControlPanel({ host, canControl, canReleaseAnyLease, compact = false, actionReady = true, onBeforeAction }: {
  host: { id: string; name?: string; control_state?: Host['control_state'] };
  canControl: boolean;
  canReleaseAnyLease: boolean;
  compact?: boolean;
  actionReady?: boolean;
  onBeforeAction?: () => Promise<boolean>;
}) {
  const queryClient = useQueryClient();
  const { user } = useAuth();
  const { notify } = useToast();
  const [confirmAction, setConfirmAction] = useState<ControlActionRequest | null>(null);
  const [pendingOperationCount, setPendingOperationCount] = useState(0);
  const [compactActionPreparing, setCompactActionPreparing] = useState(false);
  const [controlActionPending, setControlActionPending] = useState(false);
  const compactActionPreparingRef = useRef(false);
  const control = useQuery({
    queryKey: ['host-control', host.id],
    queryFn: () => portalApi.hostControl(host.id),
    refetchInterval: (query) => {
      const value = query.state.data;
      return controlActionPending || (value && (value.active_transition || value.control_state === 'taking_over' || value.control_state === 'returning')) ? 1500 : false;
    },
  });
  useNetworkReadNotice([{ failed: control.isError, error: control.error, retry: control.refetch }], '电脑控制状态读取失败，已保留当前内容。');
  const updateControl = useMutation({
    mutationFn: (body: ControlActionRequest) => portalApi.hostControlAction(host.id, body),
    onMutate: () => setControlActionPending(true),
    onSuccess: async (result, body) => {
      setPendingOperationCount(result.pending_operation_ids.length);
      queryClient.setQueryData<ControlStatus>(['host-control', host.id], {
        host_id: result.host_id,
        control_state: result.control_state,
        control_version: result.control_version,
        control_actor_user_id: result.control_actor_user_id,
        control_lease_expires_at: result.control_lease_expires_at,
        control_lease_active: result.control_lease_active,
        control_lease_held_by_me: result.control_lease_held_by_me,
        active_transition: result.active_transition,
      });
      setConfirmAction(null);
      void queryClient.invalidateQueries({ queryKey: ['host-control', host.id] });
      void queryClient.invalidateQueries({ queryKey: ['hosts'] });
      if (result.active_transition || result.control_state === 'taking_over' || result.control_state === 'returning') {
        notify(body.action === 'take_over' ? '接管请求已受理，等待云端工具停止确认；完成前桌面操作仍受限制。' : '交还请求已受理，等待云端确认控制权切换。', 'info');
      } else if (body.action === 'take_over' && result.control_state === 'user') {
        notify('接管已完成，现在可以操作电脑。', 'success');
      } else if (body.action === 'release' && result.control_state === 'ai') {
        notify('已交还 AI，主机和电脑文件状态已刷新。', 'success');
      } else {
        notify('控制状态已更新，正在核对主机状态。', 'info');
      }
    },
    onSettled: () => setControlActionPending(false),
    onError: (error) => {
      const details = errorDetails(error);
      if (details.code === 'conflict') {
        setConfirmAction(null);
        notify('控制状态已变化，已刷新最新状态，请重新操作。', 'error');
      } else {
        notify(details.message, 'error');
      }
      void queryClient.invalidateQueries({ queryKey: ['host-control', host.id] });
      void queryClient.invalidateQueries({ queryKey: ['hosts'] });
    },
  });
  const status = control.data;
  const refetchControl = control.refetch;
  const state = status?.control_state ?? host.control_state;
  const transition = status?.active_transition;
  const transitioning = state === 'taking_over' || state === 'returning';
  const operator = status?.control_lease_active && status.control_actor_user_id
    ? status.control_actor_user_id === user?.id ? '你' : `账号 ${status.control_actor_user_id}`
    : '未知';
  const leaseHeldByMe = state === 'user' && status?.control_lease_active === true && status.control_lease_held_by_me;
  const canRelease = canControl && !control.isError && state === 'user' && !!status
    && (status.control_lease_held_by_me || (!status.control_lease_active && canReleaseAnyLease));
  const compactLabel = control.isError ? '控制权状态暂不可用'
    : !status ? '正在确认控制权…'
      : state === 'ai' ? 'Morde 拥有控制权'
        : state === 'user' ? status.control_lease_active
          ? status.control_lease_held_by_me ? '你正在控制电脑' : '其他成员正在控制电脑'
          : '控制租约已失效'
        : state === 'taking_over' ? '正在接管电脑' : '正在交还 Morde';

  useEffect(() => {
    if (state !== 'user' || !status?.control_lease_active || !status.control_lease_held_by_me) return;
    let renewing = false;
    const timer = window.setInterval(() => {
      if (renewing) return;
      renewing = true;
      void portalApi.renewHostControlLease(host.id, status.control_version)
        .then((next) => queryClient.setQueryData<ControlStatus>(['host-control', host.id], next))
        .catch(() => void refetchControl())
        .finally(() => { renewing = false; });
    }, 10_000);
    return () => window.clearInterval(timer);
  }, [host.id, queryClient, refetchControl, state, status?.control_lease_active, status?.control_lease_held_by_me, status?.control_version]);

  const startAction = (action: ControlActionRequest['action']) => {
    if (!status) return;
    setConfirmAction({ action, request_id: newIdempotencyKey(), expected_control_version: status.control_version });
  };
  const resumeTransition = () => {
    if (!transition) return;
    updateControl.mutate({
      action: transition.action,
      request_id: transition.request_id,
      expected_control_version: transition.expected_control_version,
    });
  };

  const runCompactAction = async (action: ControlActionRequest['action']) => {
    if (!status || updateControl.isPending || !canControl || compactActionPreparingRef.current) return;
    compactActionPreparingRef.current = true;
    setCompactActionPreparing(true);
    try {
      if (onBeforeAction && !(await onBeforeAction())) return;
      const resumable = transition?.action === action && Boolean(transition.failure_reason);
      setCompactActionPreparing(false);
      await updateControl.mutateAsync(resumable && transition ? {
        action: transition.action,
        request_id: transition.request_id,
        expected_control_version: transition.expected_control_version,
      } : { action, request_id: newIdempotencyKey(), expected_control_version: status.control_version });
    } catch {
      // The mutation reports its failure and refreshes the server state.
    } finally {
      compactActionPreparingRef.current = false;
      setCompactActionPreparing(false);
    }
  };

  if (compact) {
    const resumableTransition = Boolean(transition?.failure_reason && canControl &&
      (transition.action === 'take_over' || status?.control_lease_held_by_me));
    const action = state === 'ai' ? 'take_over' : canRelease ? 'release' : null;
    const nextAction = resumableTransition && transition ? transition.action : action;
    const enabled = actionReady && canControl && !compactActionPreparing && !control.isPending && !control.isFetching && !control.isError && !!status &&
      !updateControl.isPending && Boolean(nextAction) && (!transitioning || resumableTransition) &&
      (resumableTransition || (nextAction === 'take_over' ? state === 'ai' : canRelease));
    const label = compactActionPreparing ? '正在锁定工作台…'
      : updateControl.isPending ? nextAction === 'release' ? '正在归还控制权…' : '正在接管控制权…'
      : control.isPending || (!status && !control.isError) ? '正在确认…'
        : control.isError ? '状态不可用'
            : resumableTransition ? nextAction === 'take_over' ? '重试接管控制权' : '重试归还控制权'
              : transitioning ? state === 'taking_over' ? '正在接管控制权…' : '正在归还控制权…'
                : nextAction === 'release' ? leaseHeldByMe ? '归还控制权' : '恢复 AI 控制' : '接管控制权';
    const reason = control.isError ? '控制权状态暂不可用'
      : !status ? '正在确认控制权状态'
        : !canControl ? '当前账号没有控制权限'
          : !actionReady ? '工作台尚未就绪'
            : transitioning ? '控制权正在切换'
              : state === 'user' && status.control_lease_active && !status.control_lease_held_by_me ? '其他成员正在控制电脑'
                : state === 'user' && !status.control_lease_active ? canReleaseAnyLease ? '控制租约已失效，可恢复 AI 控制' : '控制租约已失效，仅电脑所有者可恢复'
                  : !nextAction ? '当前控制操作不可用' : '';
    return <section className="host-control host-control-compact computer-panel-control" aria-label="云端电脑控制">
      <button type="button" className="button button-primary" disabled={!enabled || !nextAction}
        aria-label={label} title={reason || label} onClick={() => nextAction && void runCompactAction(nextAction)}>
        {label}
      </button>
      {reason && status && !control.isError && <small className="computer-panel-control-reason" role="status">{reason}</small>}
      {control.isError && <FloatingError inline className="error-inline" role="alert">
        {status ? '控制权状态刷新失败；为安全起见，操作已暂停，当前状态仅供参考。' : `无法读取控制权状态：${errorDetails(control.error).message}`}
        <button type="button" className="button button-secondary button-small" onClick={() => void control.refetch()}>重试读取</button>
      </FloatingError>}
    </section>;
  }

  return <section className={`host-control${compact ? ' host-control-compact computer-panel-control' : ''}`} aria-label={`电脑控制状态 ${host.name || host.id}`}>
    <div className="host-control-summary">
      <div>
        <strong>{compact ? compactLabel : `电脑控制：${state ? controlStateLabels[state] : '读取中'}`}</strong>
        {!compact && <>
          <p className="muted">{!state
            ? '正在读取控制状态；状态确认前不会开放人工操作。'
            : state === 'ai'
              ? '当前由云端 AI 控制。工作台仅供查看；接管完成并确认工具停止后，才允许人工操作。'
              : state === 'user'
                ? status?.control_lease_active
                  ? `当前由${operator}人工控制。交还完成后，云端 AI 才能恢复操作。`
                  : '操作租约已失效；工作台保持只读，请重新读取控制状态。'
                : '控制权切换期间，工作台操作会保持受限。'}</p>
          {state === 'user' && status && <p className="muted">操作租约：{status.control_lease_active
            ? `${status.control_lease_held_by_me ? '由你持有' : `由${operator}持有`}，到期时间 ${status.control_lease_expires_at ? formatDate(status.control_lease_expires_at) : '未知'}`
            : '当前无有效租约'}</p>}
          {status && <p className="muted">控制版本：v{status.control_version}</p>}
          <p className="muted">桌面画面可能显示电脑文件，获准查看桌面的共享成员也能看到。</p>
        </>}
      </div>
      {canControl && !control.isPending && !control.isError && status && !transitioning && state === 'ai' && <button type="button" className={`button ${compact ? 'button-primary' : 'button-secondary'}`} disabled={updateControl.isPending} onClick={() => startAction('take_over')}>{compact ? '接管电脑' : '接管操作'}</button>}
      {canRelease && !transitioning && <button type="button" className={`button ${compact ? 'button-primary' : 'button-secondary'}`} disabled={updateControl.isPending} onClick={() => startAction('release')}>{compact ? '交还 Morde' : '交还 AI'}</button>}
      {canControl && !control.isError && transition && <button type="button" className={`button ${compact ? 'button-primary' : 'button-secondary'}`} disabled={updateControl.isPending} onClick={resumeTransition}>{transition.action === 'take_over' ? compact ? '继续接管' : '继续接管确认' : compact ? '继续交还' : '继续交还确认'}</button>}
      {control.isPending && !status && <span className="muted" role="status">正在读取控制状态…</span>}
    </div>
    {!compact && transition && <p className="host-control-transition" role="status">
      {transition.action === 'take_over' ? '接管尚未完成' : '交还尚未完成'}：{controlPhaseLabels[transition.phase]}。
      {transition.failure_reason && ` ${controlFailureLabels[transition.failure_reason] || '云端尚未确认控制切换。'}`}
      {pendingOperationCount > 0 && ` 尚有 ${pendingOperationCount} 项操作等待停止确认。`}
      {transition.failure_reason === 'operations_pending' && pendingOperationCount === 0 && (transition.action === 'take_over' ? ' 云端仍有未完成操作，继续等待停止回执后再接管。' : ' 云端仍有未完成操作，继续等待工具结束后再交还。')}
    </p>}
    {!compact && transitioning && !transition && <p className="host-control-transition" role="status">控制状态正在切换，尚未收到最终确认；请等待刷新。</p>}
    {control.isError && <FloatingError inline className="error-inline" role="alert">
      {status ? '控制权状态刷新失败；为安全起见，操作已暂停，当前状态仅供参考。' : `无法读取控制权状态：${errorDetails(control.error).message}`}
      <button type="button" className="button button-secondary button-small" onClick={() => void control.refetch()}>重试读取</button>
    </FloatingError>}
    {confirmAction && <ConfirmDialog
      title={confirmAction.action === 'take_over' ? `接管主机 ${host.name || host.id}` : `交还主机 ${host.name || host.id} 给 AI`}
      description={confirmAction.action === 'take_over'
        ? '接管会先阻止新的 AI 操作，并等待在途工具确认停止。等待期间工作台保持只读；只有云端确认停止并完成控制权切换后，才允许人工操作。'
        : '交还会把主机控制权恢复给云端 AI。完成后页面会重新读取主机和电脑文件状态。'}
      confirmLabel={confirmAction.action === 'take_over' ? '开始接管' : '交还给 AI'}
      busy={updateControl.isPending}
      onClose={() => setConfirmAction(null)}
      onConfirm={() => updateControl.mutate(confirmAction)}
    />}
  </section>;
}
