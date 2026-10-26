import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';
import { Link } from 'react-router';
import { errorDetails, portalApi } from '../app/api';
import { formatDate } from '../app/format';
import { useToast } from '../app/toast';
import { ConfirmDialog } from '../components/ConfirmDialog';

type Client = Awaited<ReturnType<typeof portalApi.clients>>['items'][number];
const platformName: Record<string, string> = { desktop: '电脑', android: 'Android', ios: 'iOS' };

function ClientCard({ client, refresh }: { client: Client; refresh: () => void }) {
  const { notify } = useToast();
  const [confirmRevoke, setConfirmRevoke] = useState(false);
  const [renderedAt] = useState(Date.now);
  const revoke = useMutation({ mutationFn: portalApi.revokeClient, onSuccess: () => { setConfirmRevoke(false); notify('客户端已撤销。', 'success'); refresh(); }, onError: (error) => notify(errorDetails(error).message, 'error') });
  const remaining = Date.parse(client.expires_at) - renderedAt;
  const expired = remaining <= 0;
  const expiresSoon = remaining > 0 && remaining <= 7 * 24 * 60 * 60 * 1000;
  return <article className="device-manager-card">
    <div className="host-card-heading"><div><h2>{client.display_name || '未命名客户端'}</h2><p className="muted mono">{client.id}</p></div><span className={`status ${client.revoked || expired ? 'status-stopped' : expiresSoon ? 'status-expiring' : 'status-ready'}`}>{client.revoked ? '已撤销' : expired ? '已过期' : expiresSoon ? '即将到期' : '有效'}</span></div>
    <p className="device-manager-meta">{platformName[client.platform] ?? client.platform} · 创建于 {formatDate(client.created_at)} · 到期于 {formatDate(client.expires_at)}</p>
    {expiresSoon && !client.revoked && <p className="muted" role="status">授权将在 7 天内到期，请在到期前重新配对。</p>}
    {client.platform === 'desktop' && <p className="muted">工作区同步权限：{client.workspace_ids.length ? client.workspace_ids.join('、') : '未授权'}</p>}
    {!client.revoked && <div className="device-manager-actions"><button type="button" className="button button-danger-quiet" onClick={() => setConfirmRevoke(true)}>撤销客户端</button></div>}
    {confirmRevoke && <ConfirmDialog title="撤销客户端" description={`撤销“${client.display_name || client.id}”后，这个安装的任务、云端电脑和文件同步权限都会失效。本机文件不会删除。`} confirmLabel="撤销客户端" danger busy={revoke.isPending} onClose={() => setConfirmRevoke(false)} onConfirm={() => revoke.mutate(client.id)} />}
  </article>;
}

export function Clients() {
  const queryClient = useQueryClient();
  const clients = useQuery({ queryKey: ['clients'], queryFn: portalApi.clients });
  const refresh = () => { void queryClient.invalidateQueries({ queryKey: ['clients'] }); };
  return <section>
    <div className="page-heading"><div><p className="eyebrow">账号安全</p><h1>客户端</h1><p className="muted">管理已经批准的电脑和手机安装。撤销后，该安装需要重新配对。</p></div><Link className="button button-secondary" to="/client-authorize">输入配对码</Link></div>
    {clients.isPending ? <div className="loading-card" role="status">正在加载客户端…</div>
      : clients.isError ? <div className="error-card" role="alert">{errorDetails(clients.error).message}<button className="button button-secondary" type="button" onClick={() => void clients.refetch()}>重试</button></div>
        : !clients.data.items.length ? <div className="empty-card compact"><p>还没有已配对的客户端。请在 Morde 电脑或手机应用发起配对。</p></div>
          : <div className="device-manager-list">{clients.data.items.map((client) => <ClientCard key={client.id} client={client} refresh={refresh} />)}</div>}
  </section>;
}
