import { useState } from 'react';
import type { FormEvent } from 'react';
import { Link, useParams } from 'react-router';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { errorDetails, portalApi } from '../../app/api';
import { formatCpu, formatDate, formatMemory, hostStateLabel } from '../../app/format';
import { useToast } from '../../app/toast';
import { FloatingError } from '../../components/FloatingError';

function HostResourceEditor({ id }: { id: string }) {
  const client = useQueryClient();
  const { notify } = useToast();
  const query = useQuery({ queryKey: ['admin', 'host-resources', id], queryFn: () => portalApi.adminHostResources(id) });
  const current = query.data;
  const [cpu, setCPU] = useState<string | null>(null);
  const [memory, setMemory] = useState<string | null>(null);
  const save = useMutation({
    mutationFn: () => portalApi.updateAdminHostResources(id, {
      cpu_milli: Number(cpu ?? current?.resources.cpu_milli), mem_mib: Number(memory ?? current?.resources.mem_mib), expected_version: current!.version,
    }),
    onSuccess: (value) => { notify(value.state === 'running' ? '电脑规格已在线调整并核验。' : '电脑规格已保存，下次开机生效。', 'success'); void client.invalidateQueries({ queryKey: ['admin', 'host-resources', id] }); void client.invalidateQueries({ queryKey: ['admin', 'hosts'] }); },
    onError: (error) => { notify(errorDetails(error).message, 'error'); void client.invalidateQueries({ queryKey: ['admin', 'host-resources', id] }); },
  });
  const submit = (event: FormEvent<HTMLFormElement>) => { event.preventDefault(); save.mutate(); };
  return <section className="admin-detail-section"><h2>电脑规格</h2>
    {query.isPending ? <div className="loading-card" role="status">正在读取规格…</div> : query.isError ? <FloatingError className="error-card" role="alert">{errorDetails(query.error).message}<button className="button button-secondary" type="button" onClick={() => void query.refetch()}>重试</button></FloatingError> : current?.update_state !== 'idle' ? <div className="notice notice-warning">上次规格调整仍在核验中，请联系运维核对运行时资源后再调整。</div> : current?.state !== 'stopped' && current?.state !== 'running' ? <div className="notice notice-warning">当前电脑状态不能调整规格。</div> : <form className="form-card form-stack" onSubmit={submit}>
      <label htmlFor="host-resource-cpu">CPU（毫核）</label><input id="host-resource-cpu" type="number" min="1" required value={cpu ?? String(current.resources.cpu_milli)} onChange={(event) => setCPU(event.target.value)} />
      <label htmlFor="host-resource-memory">内存（MiB）</label><input id="host-resource-memory" type="number" min="1" required value={memory ?? String(current.resources.mem_mib)} onChange={(event) => setMemory(event.target.value)} />
      <div className="form-actions"><button className="button button-primary" disabled={save.isPending} type="submit">{save.isPending ? '保存中…' : '保存规格'}</button></div>
    </form>}
  </section>;
}

function HostRecovery({ id, generation, failed }: { id: string; generation: number; failed: boolean }) {
  const client = useQueryClient();
  const { notify } = useToast();
  const [confirmed, setConfirmed] = useState(false);
  const recover = useMutation({
    mutationFn: () => portalApi.adminRecoverHost(id, { runtime_generation: generation, confirm_retry: confirmed }),
    onSuccess: (result) => {
      notify(result.action === 'adopt_existing' ? `已找到 ${result.sandbox_count} 个沙箱，正在接管恢复。` : '已确认并重新创建电脑，电脑数据目录已保留。', 'success');
      setConfirmed(false);
      void client.invalidateQueries({ queryKey: ['admin', 'hosts'] });
    },
    onError: (error) => { notify(errorDetails(error).message, 'error'); void client.invalidateQueries({ queryKey: ['admin', 'hosts'] }); },
  });
  return <section className="admin-detail-section">
    <div className="dashboard-section-heading"><div><h2>异常恢复</h2><p>修复镜像、网络或沙箱服务等外部问题后，可在这里安全恢复。</p></div></div>
    <div className="notice notice-warning">{failed ? '该电脑的恢复已失败。系统会核对当前代沙箱：运行中的沙箱可接管；失败的沙箱会在确认后移除并创建新代。个人配置与电脑数据目录会保留。' : '上次恢复结果不确定，系统已暂停自动重试。系统会核对远端沙箱：找到可用沙箱就接管；没有可用沙箱时，仅在确认问题已修复后重新创建。电脑数据目录会保留。'}</div>
    <label className="checkbox-row"><input type="checkbox" checked={confirmed} onChange={(event) => setConfirmed(event.target.checked)} />我已修复外部问题，并确认必要时可以删除失败沙箱并重建这台电脑；数据目录需要保留。</label>
    <div className="form-actions"><button className="button button-primary" type="button" disabled={!confirmed || recover.isPending} onClick={() => recover.mutate()}>{recover.isPending ? '正在检查并恢复…' : '检查并恢复'}</button></div>
  </section>;
}

export function AdminHostDetail() {
  const { id = '' } = useParams();
  const hosts = useQuery({ queryKey: ['admin', 'hosts'], queryFn: portalApi.adminHosts });
  const users = useQuery({ queryKey: ['admin', 'users'], queryFn: () => portalApi.adminUsers() });
  if (hosts.isPending || users.isPending) return <div className="loading-card" role="status">正在读取电脑信息…</div>;
  if (hosts.isError || users.isError) {
    const error = hosts.isError ? hosts.error : users.error;
    return <FloatingError className="error-card" role="alert">{errorDetails(error).message}<button className="button button-secondary" type="button" onClick={() => { void hosts.refetch(); void users.refetch(); }}>重试</button></FloatingError>;
  }
  const host = hosts.data.items.find((item) => item.id === id);
  if (!host) return <div className="empty-card"><h2>找不到这台电脑</h2><p>电脑可能已被移除，或当前账号无权查看。</p><Link className="button button-secondary" to="/admin/hosts">返回电脑列表</Link></div>;
  const owner = users.data.items.find((user) => user.id === host.owner_id);

  return <section>
    <section className="design-panel">
    <div className="design-panel-header"><div><h2>{host.name || '未命名主机'}</h2><p className="mono">{host.id} · {host.template_id}</p></div><div className="inline-actions"><span className={`status status-${host.state}`}>{hostStateLabel[host.state] || host.state}</span><Link className="button button-secondary" to="/admin/hosts">← 返回电脑列表</Link></div></div>
    <div className="admin-detail-facts">
      <article><span>所有者</span><strong>{owner ? <Link to={`/admin/users/${encodeURIComponent(owner.id)}`}>{owner.name}</Link> : host.owner_id}</strong></article>
      <article><span>CPU / 内存</span><strong>{formatCpu(host.resources.cpu_limit_milli)} / {formatMemory(host.resources.mem_limit_mib)}</strong></article>
      <article><span>创建时间</span><strong>{formatDate(host.created_at)}</strong></article>
      <article><span>到期时间</span><strong>{formatDate(host.expires_at)}</strong></article>
    </div>
    </section>
    <HostResourceEditor id={host.id} />
    {((host.state === 'creating' && host.state_reason === 'restore_unknown') || (host.state === 'failed' && host.runtime_generation > 1)) && <HostRecovery id={host.id} generation={host.runtime_generation} failed={host.state === 'failed'} />}
    <section className="admin-detail-section">
      <div className="dashboard-section-heading"><div><h2>电脑专属数据目录</h2><p>数据目录随这台电脑持久保存；管理员只核对目录和应用状态。</p></div></div>
      {!host.workspaces.length ? <div className="empty-card compact"><p>当前接口未返回电脑数据目录状态，需要进一步核对。</p></div> : <div className="table-wrap"><table><thead><tr><th>目录</th><th>应用状态</th><th>目标版本</th><th>电脑已应用版本</th><th>同步状态</th></tr></thead><tbody>
        {host.workspaces.map((binding) => <tr key={binding.workspace.id}><td>电脑专属目录</td><td>{binding.state}</td><td>v{binding.desired_revision}</td><td>{binding.applied_revision > 0 ? `v${binding.applied_revision}` : '尚未应用'}</td><td>{binding.sync_state || '未提供'}</td></tr>)}
      </tbody></table></div>}
    </section>
    <p className="notice notice-info admin-detail-note">此页面只查看电脑、所有者和专属数据目录元数据。当前管理接口不提供停止、冻结或隔离能力。</p>
  </section>;
}
