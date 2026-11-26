import { useState, type FormEvent } from 'react';
import { Link } from 'react-router';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { errorDetails, portalApi } from '../../app/api';
import { formatCpu, formatDate, formatMemory } from '../../app/format';
import { useAuth } from '../../app/auth';
import { useToast } from '../../app/toast';
import { useNetworkReadNotice } from '../../app/use-network-read-notice';
import { FloatingError } from '../../components/FloatingError';
import { Modal } from '../../components/Modal';

type AccountStatus = 'pending' | 'rejected' | 'approved' | 'disabled';
type AdminUser = Awaited<ReturnType<typeof portalApi.adminUsers>>['items'][number];

const statusLabel: Record<AccountStatus, string> = {
  pending: '待审核', rejected: '已拒绝', approved: '已通过', disabled: '已禁用',
};

function nextStatusActions(status: AccountStatus): { status: AccountStatus; label: string; danger?: boolean }[] {
  switch (status) {
    case 'pending': return [
      { status: 'approved', label: '通过审核' },
      { status: 'rejected', label: '拒绝申请', danger: true },
    ];
    case 'rejected': return [{ status: 'approved', label: '重新通过' }];
    case 'approved': return [{ status: 'disabled', label: '禁用账号', danger: true }];
    case 'disabled': return [{ status: 'approved', label: '恢复账号' }];
  }
}

export function AdminUsers() {
  const client = useQueryClient();
  const { user: current } = useAuth();
  const { notify } = useToast();
  const [filter, setFilter] = useState<AccountStatus | 'all'>('all');
  const [offset, setOffset] = useState(0);
  const [passwordTarget, setPasswordTarget] = useState<AdminUser | null>(null);
  const [newPassword, setNewPassword] = useState('');
  const [confirmPassword, setConfirmPassword] = useState('');
  const query = useQuery({
    queryKey: ['admin', 'users', filter, offset],
    queryFn: () => portalApi.adminUsers(filter === 'all' ? undefined : filter, offset),
  });
  const networkError = useNetworkReadNotice([{ failed: query.isError, error: query.error, retry: query.refetch }], '网络异常，用户列表暂时无法读取。');
  const update = useMutation({
    mutationFn: ({ id, body }: { id: string; body: { expected_status: AccountStatus; status?: AccountStatus; role?: 'user' | 'admin' } }) => portalApi.updateUser(id, body),
    onSuccess: () => {
      notify('账号信息已更新。', 'success');
      void client.invalidateQueries({ queryKey: ['admin', 'users'] });
    },
    onError: (error) => notify(errorDetails(error).message, 'error'),
  });
  const resetPassword = useMutation({
    mutationFn: ({ item, password }: { item: AdminUser; password: string }) => portalApi.resetAdminUserPassword(item.id, {
      expected_status: item.status,
      new_password: password,
    }),
    onSuccess: (_result, variables) => {
      setPasswordTarget(null);
      setNewPassword('');
      setConfirmPassword('');
      if (variables.item.id === current?.id) {
        window.location.assign('/admin/login');
        return;
      }
      notify('密码已重置。用户下次登录后需要先修改密码。', 'success');
      void client.invalidateQueries({ queryKey: ['admin', 'users'] });
    },
    onError: (error) => notify(errorDetails(error).message, 'error'),
  });

  const changeStatus = (item: AdminUser, status: AccountStatus) => update.mutate({
    id: item.id,
    body: { expected_status: item.status, status },
  });
  const changeRole = (item: AdminUser) => update.mutate({
    id: item.id,
    body: { expected_status: item.status, role: item.role === 'admin' ? 'user' : 'admin' },
  });
  const submitPasswordReset = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (!passwordTarget || resetPassword.isPending) return;
    const bytes = new TextEncoder().encode(newPassword).length;
    if (bytes < 8 || bytes > 72) {
      notify('密码长度必须为 8 至 72 字节。', 'error');
      return;
    }
    if (newPassword !== confirmPassword) {
      notify('两次输入的密码不一致。', 'error');
      return;
    }
    resetPassword.mutate({ item: passwordTarget, password: newPassword });
  };
  const closePasswordReset = () => {
    if (resetPassword.isPending) return;
    setPasswordTarget(null);
    setNewPassword('');
    setConfirmPassword('');
  };

  return <section className="admin-content design-panel">
    <div className="design-panel-header"><div><h2>用户列表</h2><p>审核账号并管理权限。</p></div><button type="button" className="button button-secondary" disabled={query.isFetching} onClick={() => void query.refetch()}>刷新数据</button></div>
    <div className="device-manager-actions">
      <label htmlFor="user-status-filter">账号状态</label>
      <select id="user-status-filter" value={filter} onChange={(event) => { setFilter(event.target.value as AccountStatus | 'all'); setOffset(0); }}>
        <option value="all">全部状态</option>
        {Object.entries(statusLabel).map(([value, label]) => <option key={value} value={value}>{label}</option>)}
      </select>
    </div>
    <div className="table-wrap"><table><thead><tr><th>用户</th><th>角色 / 状态</th><th>云端电脑数量</th><th>CPU 配额</th><th>内存配额</th><th>创建时间</th><th>操作</th></tr></thead><tbody>
      {query.data?.items.map((item) => {
        const protectedAdmin = item.role === 'super_admin';
        const self = item.id === current?.id;
        const canResetPassword = current?.role === 'super_admin';
        const canChangeRole = item.status === 'approved' && !protectedAdmin && !self;
        return <tr key={item.id}>
          <td><Link className="admin-table-link" to={`/admin/users/${encodeURIComponent(item.id)}`}><strong>{item.name}</strong><small className="table-sub mono">{item.id}</small></Link></td>
          <td>{item.role === 'super_admin' ? '超级管理员' : item.role === 'admin' ? '管理员' : '普通用户'} · {statusLabel[item.status]}</td>
          <td>{item.usage.hosts}</td>
          <td>{formatCpu(item.usage.cpu_milli)} / {formatCpu(item.quota.cpu_milli)}</td>
          <td>{formatMemory(item.usage.mem_mib)} / {formatMemory(item.quota.mem_mib)}</td>
          <td>{formatDate(item.created_at)}</td>
          <td>
            <div className="inline-actions">
              {protectedAdmin ? <span className="muted" aria-label="admin 账号受保护">受保护的超级管理员</span> : self ? <span className="muted">当前账号</span> : <>
                {nextStatusActions(item.status).map((action) => <button key={action.status} type="button" className={`button ${action.danger ? 'button-danger-quiet' : 'button-secondary'} button-small`} disabled={update.isPending || resetPassword.isPending} onClick={() => changeStatus(item, action.status)}>{action.label}</button>)}
                {canChangeRole && <button type="button" className="button button-secondary button-small" disabled={update.isPending || resetPassword.isPending} onClick={() => changeRole(item)}>{item.role === 'admin' ? '撤销管理员' : '设为管理员'}</button>}
              </>}
              {canResetPassword && <button type="button" className="button button-secondary button-small" disabled={resetPassword.isPending || update.isPending} onClick={() => { setPasswordTarget(item); setNewPassword(''); setConfirmPassword(''); resetPassword.reset(); }}>修改密码</button>}
            </div>
          </td>
        </tr>;
      })}
    </tbody></table></div>
    {query.isSuccess && !query.data.items.length && <div className="empty-card compact"><p>当前筛选下没有用户。</p></div>}
    {query.isSuccess && query.data.total > 0 && <nav className="admin-pagination" aria-label="用户列表分页">
      <span>显示 {offset + 1}–{Math.min(offset + query.data.items.length, query.data.total)} / {query.data.total}</span>
      <div className="inline-actions"><button className="button button-secondary button-small" type="button" disabled={offset === 0 || query.isFetching} onClick={() => setOffset(Math.max(0, offset - query.data.limit))}>上一页</button>
        <button className="button button-secondary button-small" type="button" disabled={offset + query.data.items.length >= query.data.total || query.isFetching} onClick={() => setOffset(offset + query.data.limit)}>下一页</button></div>
    </nav>}
    {(query.isPending || (networkError && !query.data)) && <div className="loading-card" role="status">正在加载用户…</div>}
    {query.isError && !networkError && <FloatingError className="error-card" role="alert">{errorDetails(query.error).message}<button type="button" className="button button-secondary" onClick={() => void query.refetch()}>重试</button></FloatingError>}
    {passwordTarget && <Modal title={`修改 ${passwordTarget.name} 的密码`} onClose={closePasswordReset}>
      <form className="form-stack" onSubmit={submitPasswordReset}>
        <div className="notice notice-warning" role="note">
          <p>新密码会立即生效，并使该账号现有登录和客户端凭据失效。用户下次需先登录 Portal 修改密码，之后才能使用 PC 或 iOS 客户端。</p>
          {passwordTarget.id === current?.id && <p>这是当前超级管理员账号；保存后会退出登录，需要使用新密码重新登录并完成修改。</p>}
        </div>
        <label htmlFor="admin-reset-password">新密码</label>
        <input id="admin-reset-password" type="password" autoComplete="new-password" minLength={8} maxLength={72} required value={newPassword} onChange={(event) => setNewPassword(event.target.value)} />
        <label htmlFor="admin-reset-password-confirm">再次输入新密码</label>
        <input id="admin-reset-password-confirm" type="password" autoComplete="new-password" minLength={8} maxLength={72} required value={confirmPassword} onChange={(event) => setConfirmPassword(event.target.value)} />
        <div className="form-actions">
          <button type="button" className="button button-secondary" disabled={resetPassword.isPending} onClick={closePasswordReset}>取消</button>
          <button type="submit" className="button button-primary" disabled={resetPassword.isPending || !newPassword || !confirmPassword}>
            {resetPassword.isPending ? '正在保存…' : '重置密码'}
          </button>
        </div>
      </form>
    </Modal>}
  </section>;
}
