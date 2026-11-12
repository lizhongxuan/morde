import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';
import { errorDetails, portalApi } from '../app/api';
import { formatDate } from '../app/format';
import { useToast } from '../app/toast';
import { ConfirmDialog } from '../components/ConfirmDialog';
import { ScreenPagination } from '../components/ScreenPagination';
import { Link } from 'react-router';
import { FloatingError } from '../components/FloatingError';

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
    {expiresSoon && !client.revoked && <p className="muted" role="status">登录状态将在 7 天内到期，届时请在此客户端重新登录。</p>}
    {!client.revoked && <div className="device-manager-actions"><button type="button" className="button button-danger-quiet" onClick={() => setConfirmRevoke(true)}>撤销客户端</button></div>}
    {confirmRevoke && <ConfirmDialog title="撤销客户端" description={`撤销“${client.display_name || client.id}”后，这个安装的云端电脑和文件同步权限都会失效。本机文件不会删除。`} confirmLabel="撤销客户端" danger busy={revoke.isPending} onClose={() => setConfirmRevoke(false)} onConfirm={() => revoke.mutate(client.id)} />}
  </article>;
}

export function Clients() {
  const queryClient = useQueryClient();
  const clients = useQuery({ queryKey: ['clients'], queryFn: portalApi.clients });
  const refresh = () => { void queryClient.invalidateQueries({ queryKey: ['clients'] }); };
  return <section className="design-panel">
    <div className="design-panel-header"><div><h2>已登录设备</h2><p>管理已登录的电脑和手机。</p></div><Link className="button button-secondary" to="/download">下载客户端</Link></div>
    {clients.isPending ? <div className="loading-card" role="status">正在加载客户端…</div>
      : clients.isError ? <FloatingError className="error-card" role="alert">{errorDetails(clients.error).message}<button className="button button-secondary" type="button" onClick={() => void clients.refetch()}>重试</button></FloatingError>
        : !clients.data.items.length ? <div className="empty-card compact"><p>还没有已登录的客户端。请在 Morde 电脑或手机应用使用账号密码登录。</p></div>
          : <ScreenPagination
            items={clients.data.items}
            getKey={(client) => client.id}
            estimatedItemHeight={148}
            height="min(56dvh, 560px)"
            ariaLabel="已登录客户端"
            renderItem={(client) => <ClientCard client={client} refresh={refresh} />}
          />}
  </section>;
}
