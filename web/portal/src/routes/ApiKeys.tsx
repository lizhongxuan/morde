import { useEffect, useState } from 'react';
import type { FormEvent } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { errorDetails, portalApi } from '../app/api';
import { formatDate } from '../app/format';
import { useToast } from '../app/toast';
import { ConfirmDialog } from '../components/ConfirmDialog';
import { OnceSecretDialog } from '../components/OnceSecretDialog';

type ApiKey = {
  id: string;
  name: string;
  expires_at: string;
  last_used_at: string | null;
  revoked: boolean;
  created_at: string;
};

export function ApiKeys() {
  const client = useQueryClient();
  const { notify } = useToast();
  const keys = useQuery({
    queryKey: ['apikeys'],
    queryFn: async () => await portalApi.apiKeys() as unknown as { items: ApiKey[] },
  });
  const [name, setName] = useState('');
  const [ttl, setTtl] = useState('');
  const [secret, setSecret] = useState<{ key: string; expires_at: string } | null>(null);
  const [removing, setRemoving] = useState<ApiKey | null>(null);
  const [now, setNow] = useState(0);
  useEffect(() => {
    const update = () => setNow(Date.now());
    update();
    const timer = window.setInterval(update, 60_000);
    return () => window.clearInterval(timer);
  }, []);
  const create = useMutation({
    mutationFn: portalApi.createApiKey,
    onSuccess: (result) => {
      setSecret(result);
      setName('');
      setTtl('');
      void client.invalidateQueries({ queryKey: ['apikeys'] });
    },
    onError: (error) => notify(errorDetails(error).message, 'error'),
  });
  const remove = useMutation({
    mutationFn: portalApi.deleteApiKey,
    onSuccess: () => { setRemoving(null); notify('API Key 已吊销。', 'success'); void client.invalidateQueries({ queryKey: ['apikeys'] }); },
    onError: (error) => notify(errorDetails(error).message, 'error'),
  });
  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const seconds = ttl ? Number(ttl) * 86400 : undefined;
    create.mutate({ name: name.trim(), ...(seconds ? { ttl_seconds: seconds } : {}) });
  };
  const mcpURL = `${window.location.origin}/mcp`;
  const copyEndpoint = async () => {
    try { await navigator.clipboard.writeText(mcpURL); notify('MCP 地址已复制。', 'success'); }
    catch { notify('浏览器未授予剪贴板权限，请手动复制地址。', 'error'); }
  };

  return <section>
    <div className="page-heading"><div><p className="eyebrow">账号安全</p><h1>API Key</h1><p className="muted">创建后自动获得你当前可用的权限。密钥明文只显示一次。</p></div></div>
    <section className="connection-section mcp-connection-section" aria-labelledby="mcp-connection-title">
      <div className="connection-section-heading"><div><h2 id="mcp-connection-title">连接 AI 对话工具</h2><p className="muted">接入后，AI 可以在你的账号权限范围内使用云端电脑。</p></div></div>
      <div className="mcp-endpoint"><div><span>本实例 MCP 地址</span><code>{mcpURL}</code></div><button type="button" className="button button-secondary" onClick={() => void copyEndpoint()}>复制地址</button></div>
      <ol className="mcp-setup-steps"><li><strong>创建 API Key</strong><span>无需逐项选择电脑或权限，访问范围随账号当前授权变化。</span></li><li><strong>把地址和 Key 配到 AI 客户端</strong><span>Key 只在创建时显示一次。请保存在客户端的密钥管理或环境变量中。</span></li></ol>
      <div className="notice notice-info">吊销 API Key 后，后续 MCP 请求会被拒绝。</div>
    </section>
    <form className="form-card apikey-form" onSubmit={(event) => void submit(event)}>
      <h2>创建 API Key</h2>
      <div className="inline-form">
        <div><label htmlFor="apikey-name">名称</label><input id="apikey-name" required maxLength={100} value={name} onChange={(event) => setName(event.target.value)} placeholder="例如：个人自动化" /></div>
        <div><label htmlFor="apikey-ttl">有效期（天，可选）</label><input id="apikey-ttl" type="number" min={1} max={365} value={ttl} onChange={(event) => setTtl(event.target.value)} placeholder="最长 365 天" /></div>
      </div>
      <button className="button button-primary" type="submit" disabled={!name.trim() || create.isPending}>
        {create.isPending ? '创建中…' : '创建 API Key'}
      </button>
    </form>

    <h2 className="section-title">已创建的 Key</h2>
    {keys.isPending ? <div className="loading-card">正在加载…</div> : keys.isError ? <div className="error-card" role="alert">{errorDetails(keys.error).message}<button className="button button-secondary" type="button" onClick={() => void keys.refetch()}>重试</button></div> : !keys.data?.items.length ? <div className="empty-card compact"><p>尚未创建 API Key。</p></div> : <div className="table-wrap"><table><thead><tr><th>名称</th><th>权限</th><th>创建时间</th><th>最近使用</th><th>过期时间</th><th>状态</th><th>操作</th></tr></thead><tbody>
      {keys.data.items.map((key) => {
        const expired = now > 0 && Date.parse(key.expires_at) <= now;
        return <tr key={key.id}>
          <td><strong>{key.name || '未命名'}</strong><small className="table-sub mono">{key.id}</small></td>
          <td>账号当前可用权限</td>
          <td>{formatDate(key.created_at)}</td><td>{formatDate(key.last_used_at)}</td><td>{formatDate(key.expires_at)}</td>
          <td>{key.revoked ? '已吊销' : expired ? '已过期' : '有效'}</td>
          <td>{key.revoked ? '—' : <button type="button" className="button button-danger-quiet button-small" aria-label={`吊销 API Key ${key.name || key.id}`} onClick={() => setRemoving(key)}>吊销</button>}</td>
        </tr>;
      })}
    </tbody></table></div>}
    {removing && <ConfirmDialog title="吊销 API Key" description={`“${removing.name || removing.id}”吊销后，后续请求会被拒绝；已经发出的外部操作仍可能完成。此操作不可撤销。`} confirmLabel="吊销 Key" danger busy={remove.isPending} onClose={() => setRemoving(null)} onConfirm={() => remove.mutate(removing.id)} />}
    {secret && <OnceSecretDialog title="API Key 已创建" secret={secret.key} expiresAt={formatDate(secret.expires_at)} onClose={() => setSecret(null)} />}
  </section>;
}
