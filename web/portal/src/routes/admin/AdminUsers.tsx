import { useState } from 'react';
import type { FormEvent } from 'react';
import { Link } from 'react-router';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { errorDetails, portalApi } from '../../app/api';
import { formatCpu, formatDate, formatMemory } from '../../app/format';
import { useAuth } from '../../app/auth';
import { useToast } from '../../app/toast';
import { ConfirmDialog } from '../../components/ConfirmDialog';

type AdminUser = Awaited<ReturnType<typeof portalApi.adminUsers>>['items'][number];

export function AdminUsers() {
  const client = useQueryClient();
  const { user: current } = useAuth();
  const { notify } = useToast();
  const query = useQuery({ queryKey: ['admin', 'users'], queryFn: portalApi.adminUsers });
  const [name, setName] = useState('');
  const [password, setPassword] = useState('');
  const [role, setRole] = useState<AdminUser['role']>('user');
  const [selected, setSelected] = useState<AdminUser | null>(null);
  const create = useMutation({
    mutationFn: portalApi.createUser,
    onSuccess: () => { setName(''); setPassword(''); notify('用户已创建。', 'success'); void client.invalidateQueries({ queryKey: ['admin', 'users'] }); },
    onError: (error) => notify(errorDetails(error).message, 'error'),
  });
  const update = useMutation({
    mutationFn: ({ id, disabled }: { id: string; disabled: boolean }) => portalApi.updateUser(id, { disabled }),
    onSuccess: () => { setSelected(null); notify('用户状态已更新。', 'success'); void client.invalidateQueries({ queryKey: ['admin', 'users'] }); },
    onError: (error) => notify(errorDetails(error).message, 'error'),
  });
  const submit = (event: FormEvent<HTMLFormElement>) => { event.preventDefault(); create.mutate({ name: name.trim(), password, role }); };
  return <section className="admin-content">
    <div className="page-heading"><div><p className="eyebrow">账号与配额</p><h1>用户</h1><p className="muted">创建账号并查看其资源使用情况。禁用操作只控制登录权限。</p></div></div>
    <form className="inline-form form-card" onSubmit={(event) => void submit(event)}>
      <div><label htmlFor="new-user-name">新用户名称</label><input id="new-user-name" autoComplete="off" required minLength={1} value={name} onChange={(event) => setName(event.target.value)} /></div>
      <div><label htmlFor="new-user-password">初始密码</label><input id="new-user-password" type="password" autoComplete="new-password" required value={password} onChange={(event) => setPassword(event.target.value)} /></div>
      <div><label htmlFor="new-user-role">角色</label><select id="new-user-role" value={role} onChange={(event) => setRole(event.target.value === 'admin' ? 'admin' : 'user')}><option value="user">普通用户</option><option value="admin">管理员</option></select></div>
      <button type="submit" className="button button-primary" disabled={create.isPending}>{create.isPending ? '创建中…' : '创建用户'}</button>
    </form>
    <div className="table-wrap"><table><thead><tr><th>用户</th><th>角色 / 状态</th><th>主机使用</th><th>CPU 配额</th><th>内存配额</th><th>创建时间</th><th>操作</th></tr></thead><tbody>
      {query.data?.items.map((item) => <tr key={item.id}><td><Link className="admin-table-link" to={`/admin/users/${encodeURIComponent(item.id)}`}><strong>{item.name}</strong><small className="table-sub mono">{item.id}</small></Link></td><td>{item.role === 'admin' ? '管理员' : '普通用户'} · {item.disabled ? '已禁用登录' : '正常'}</td><td>{item.usage.hosts} / {item.quota.hosts}</td><td>{formatCpu(item.usage.cpu_milli)} / {formatCpu(item.quota.cpu_milli)}</td><td>{formatMemory(item.usage.mem_mib)} / {formatMemory(item.quota.mem_mib)}</td><td>{formatDate(item.created_at)}</td><td><button type="button" className={item.disabled ? 'button button-secondary button-small' : 'button button-danger-quiet button-small'} aria-label={`${item.disabled ? '启用' : '禁用'}用户 ${item.name}`} disabled={item.id === current?.id || update.isPending} title={item.id === current?.id ? '不能在此处禁用当前账号' : undefined} onClick={() => setSelected(item)}>{item.disabled ? '启用' : '禁用登录'}</button></td></tr>)}
    </tbody></table></div>
    {query.isSuccess && !query.data.items.length && <div className="empty-card compact"><p>当前没有用户。</p></div>}
    {query.isPending && <div className="loading-card">正在加载用户…</div>}
    {query.isError && <div className="error-card" role="alert">{errorDetails(query.error).message}<button type="button" className="button button-secondary" onClick={() => void query.refetch()}>重试</button></div>}
    {selected && <ConfirmDialog title={`${selected.disabled ? '启用' : '禁用'}用户`} description={selected.disabled ? `确定恢复用户“${selected.name}”的登录权限吗？` : `确定禁用用户“${selected.name}”吗？禁用后该用户不能再登录。`} confirmLabel={selected.disabled ? '启用用户' : '禁用用户'} danger={!selected.disabled} busy={update.isPending} onClose={() => setSelected(null)} onConfirm={() => update.mutate({ id: selected.id, disabled: !selected.disabled })} />}
  </section>;
}
