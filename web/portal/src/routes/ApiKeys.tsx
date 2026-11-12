import { useEffect, useState } from 'react';
import type { FormEvent } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { errorDetails, portalApi } from '../app/api';
import { formatDate } from '../app/format';
import { useToast } from '../app/toast';
import { ConfirmDialog } from '../components/ConfirmDialog';
import { OnceSecretDialog } from '../components/OnceSecretDialog';
import { ScreenPagination } from '../components/ScreenPagination';
import { FloatingError } from '../components/FloatingError';

type ApiKey = {
  id: string;
  name: string;
  expires_at: string;
  last_used_at: string | null;
  revoked: boolean;
  created_at: string;
};

const apiKeyGridColumns = 'minmax(140px, 1.2fr) repeat(5, minmax(88px, 1fr)) auto';

export function ApiKeys() {
  const client = useQueryClient();
  const { notify } = useToast();
  const keys = useQuery({
    queryKey: ['apikeys'],
    queryFn: async () => await portalApi.apiKeys() as unknown as { items: ApiKey[] },
  });
  const [name, setName] = useState('');
  const [ttl, setTtl] = useState('');
  const [showCreate, setShowCreate] = useState(false);
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
    mutationFn: async (body: Parameters<typeof portalApi.createApiKey>[0]) => {
      const result = await portalApi.createApiKey(body);
      setSecret({ key: result.key, expires_at: result.expires_at });
    },
    onSuccess: () => {
      setName('');
      setTtl('');
      setShowCreate(false);
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
    <section className="connection-section mcp-connection-section" aria-labelledby="mcp-connection-title">
      <div className="connection-section-heading"><div><h2 id="mcp-connection-title">连接 AI 对话工具</h2><p className="muted">接入后，AI 可以在你的账号权限范围内使用云端电脑。</p></div></div>
      <div className="mcp-endpoint"><div><span>本实例 MCP 地址</span><code>{mcpURL}</code></div><button type="button" className="button button-secondary" onClick={() => void copyEndpoint()}>复制地址</button></div>
      <p className="mcp-setup-summary">创建 API Key 后，在 AI 客户端配置地址和密钥。权限随账号授权变化；密钥仅创建时显示，吊销后后续请求会被拒绝。</p>
    </section>
    <section className="design-panel" aria-labelledby="api-key-list-title">
    <div className="design-panel-header"><div><h2 id="api-key-list-title">我的密钥</h2><p>创建后自动获得当前账号可用的权限。</p></div><button type="button" className="button button-primary" aria-expanded={showCreate} disabled={create.isPending} onClick={() => setShowCreate((open) => !open)}>{showCreate ? '收起' : '＋ 创建 API Key'}</button></div>
    {showCreate && <form className="form-card apikey-form" onSubmit={(event) => void submit(event)}>
      <h2>创建 API Key</h2>
      <div className="inline-form">
        <div><label htmlFor="apikey-name">名称</label><input id="apikey-name" required maxLength={100} value={name} onChange={(event) => setName(event.target.value)} placeholder="例如：个人自动化" /></div>
        <div><label htmlFor="apikey-ttl">有效期（天，可选）</label><input id="apikey-ttl" type="number" min={1} max={365} value={ttl} onChange={(event) => setTtl(event.target.value)} placeholder="最长 365 天" /></div>
      </div>
      <button className="button button-primary" type="submit" disabled={!name.trim() || create.isPending}>
        {create.isPending ? '创建中…' : '创建 API Key'}
      </button>
    </form>}

    {keys.isPending ? <div className="loading-card">正在加载…</div> : keys.isError ? <FloatingError className="error-card" role="alert">{errorDetails(keys.error).message}<button className="button button-secondary" type="button" onClick={() => void keys.refetch()}>重试</button></FloatingError> : !keys.data?.items.length ? <div className="empty-card compact"><p>尚未创建 API Key。</p></div> : <div className="table-wrap" style={{ padding: '0 10px' }}>
      <div aria-hidden="true" style={{ display: 'grid', gridTemplateColumns: apiKeyGridColumns, gap: 12, minWidth: 850, padding: '10px 8px', borderBottom: '1px solid #e9edf2', color: '#8793a4', background: '#fafbfc', fontSize: 10, fontWeight: 700 }}>
        <span>名称</span><span>权限</span><span>创建时间</span><span>最近使用</span><span>过期时间</span><span>状态</span><span>操作</span>
      </div>
      <ScreenPagination
        items={keys.data.items}
        getKey={(key) => key.id}
        estimatedItemHeight={60}
        height="min(22dvh, 160px)"
        gap={0}
        ariaLabel="API Key 列表"
        renderItem={(key) => {
          const expired = now > 0 && Date.parse(key.expires_at) <= now;
          return <article aria-label={`API Key ${key.name || key.id}`} style={{ display: 'grid', gridTemplateColumns: apiKeyGridColumns, gap: 12, alignItems: 'center', minWidth: 850, minHeight: 60, padding: '8px', borderBottom: '1px solid #e9edf2', fontSize: 12 }}>
            <div><span className="sr-only">名称 </span><strong>{key.name || '未命名'}</strong><small className="table-sub mono"><span className="sr-only">ID </span>{key.id}</small></div>
            <span><span className="sr-only">权限 </span>账号当前可用权限</span>
            <span><span className="sr-only">创建时间 </span>{formatDate(key.created_at)}</span>
            <span><span className="sr-only">最近使用 </span>{formatDate(key.last_used_at)}</span>
            <span><span className="sr-only">过期时间 </span>{formatDate(key.expires_at)}</span>
            <span><span className="sr-only">状态 </span>{key.revoked ? '已吊销' : expired ? '已过期' : '有效'}</span>
            <span><span className="sr-only">操作 </span>{key.revoked ? '—' : <button type="button" className="button button-danger-quiet button-small" aria-label={`吊销 API Key ${key.name || key.id}`} onClick={() => setRemoving(key)}>吊销</button>}</span>
          </article>;
        }}
      />
    </div>}
    </section>
    {removing && <ConfirmDialog title="吊销 API Key" description={`“${removing.name || removing.id}”吊销后，后续请求会被拒绝；已经发出的外部操作仍可能完成。此操作不可撤销。`} confirmLabel="吊销 Key" danger busy={remove.isPending} onClose={() => setRemoving(null)} onConfirm={() => remove.mutate(removing.id)} />}
    {secret && <OnceSecretDialog title="API Key 已创建" secret={secret.key} expiresAt={formatDate(secret.expires_at)} onClose={() => { setSecret(null); create.reset(); }} />}
  </section>;
}
