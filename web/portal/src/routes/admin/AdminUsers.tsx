import { useState } from 'react';
import { Link } from 'react-router';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { errorDetails, portalApi } from '../../app/api';
import { formatCpu, formatDate, formatMemory } from '../../app/format';
import { useAuth } from '../../app/auth';
import { useToast } from '../../app/toast';

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
  const query = useQuery({
    queryKey: ['admin', 'users', filter],
    queryFn: () => portalApi.adminUsers(filter === 'all' ? undefined : filter),
  });
  const update = useMutation({
    mutationFn: ({ id, body }: { id: string; body: { expected_status: AccountStatus; status?: AccountStatus; role?: 'user' | 'admin' } }) => portalApi.updateUser(id, body),
    onSuccess: () => {
      notify('账号信息已更新。', 'success');
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

  return <section className="admin-content">
    <div className="page-heading"><div><p className="eyebrow">账号与权限</p><h1>用户</h1><p className="muted">审核注册申请，管理已通过账号的使用状态和管理员权限。</p></div></div>
    <div className="device-manager-actions">
      <label htmlFor="user-status-filter">账号状态</label>
      <select id="user-status-filter" value={filter} onChange={(event) => setFilter(event.target.value as AccountStatus | 'all')}>
        <option value="all">全部状态</option>
        {Object.entries(statusLabel).map(([value, label]) => <option key={value} value={value}>{label}</option>)}
      </select>
    </div>
    <div className="table-wrap"><table><thead><tr><th>用户</th><th>角色 / 状态</th><th>云端电脑数量</th><th>CPU 配额</th><th>内存配额</th><th>创建时间</th><th>操作</th></tr></thead><tbody>
      {query.data?.items.map((item) => {
        const protectedAdmin = item.role === 'super_admin';
        const self = item.id === current?.id;
        const canChangeRole = item.status === 'approved' && !protectedAdmin && !self;
        return <tr key={item.id}>
          <td><Link className="admin-table-link" to={`/admin/users/${encodeURIComponent(item.id)}`}><strong>{item.name}</strong><small className="table-sub mono">{item.id}</small></Link></td>
          <td>{item.role === 'super_admin' ? '超级管理员' : item.role === 'admin' ? '管理员' : '普通用户'} · {statusLabel[item.status]}</td>
          <td>{item.usage.hosts}</td>
          <td>{formatCpu(item.usage.cpu_milli)} / {formatCpu(item.quota.cpu_milli)}</td>
          <td>{formatMemory(item.usage.mem_mib)} / {formatMemory(item.quota.mem_mib)}</td>
          <td>{formatDate(item.created_at)}</td>
          <td>
            {protectedAdmin ? <span className="muted" aria-label="admin 账号受保护">受保护的超级管理员</span> : self ? <span className="muted">当前账号</span> : <div className="inline-actions">
              {nextStatusActions(item.status).map((action) => <button key={action.status} type="button" className={`button ${action.danger ? 'button-danger-quiet' : 'button-secondary'} button-small`} disabled={update.isPending} onClick={() => changeStatus(item, action.status)}>{action.label}</button>)}
              {canChangeRole && <button type="button" className="button button-secondary button-small" disabled={update.isPending} onClick={() => changeRole(item)}>{item.role === 'admin' ? '撤销管理员' : '设为管理员'}</button>}
            </div>}
          </td>
        </tr>;
      })}
    </tbody></table></div>
    {query.isSuccess && !query.data.items.length && <div className="empty-card compact"><p>当前筛选下没有用户。</p></div>}
    {query.isPending && <div className="loading-card">正在加载用户…</div>}
    {query.isError && <div className="error-card" role="alert">{errorDetails(query.error).message}<button type="button" className="button button-secondary" onClick={() => void query.refetch()}>重试</button></div>}
  </section>;
}
