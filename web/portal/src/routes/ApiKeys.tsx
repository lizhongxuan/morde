import { useEffect, useMemo, useState } from 'react';
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
  host_ids: string[];
  scopes: string[];
  expires_at: string;
  last_used_at: string | null;
  revoked: boolean;
  created_at: string;
};
type Host = Awaited<ReturnType<typeof portalApi.hosts>>['items'][number];
type SharedHost = Awaited<ReturnType<typeof portalApi.sharedHosts>>['items'][number];
type SelectableHost = { id: string; name: string; state: string; shared: boolean };

const scopeOptions = [
  ['host:read', '查看主机信息'],
  ['host:create', '创建新主机'],
  ['host:manage', '续期或删除主机'],
  ['host:delete_data', '删除主机时删除工作目录数据'],
  ['computer:observe', '查看浏览器、屏幕和文件'],
  ['computer:operate', '操作浏览器、终端和文件'],
  ['task:read', '查看任务'],
  ['task:submit', '提交任务'],
  ['task:control', '重试任务、处理审批'],
] as const;

export function ApiKeys() {
  const client = useQueryClient();
  const { notify } = useToast();
  const keys = useQuery({
    queryKey: ['apikeys'],
    queryFn: async () => await portalApi.apiKeys() as unknown as { items: ApiKey[] },
  });
  const hosts = useQuery({ queryKey: ['hosts'], queryFn: portalApi.hosts });
  const sharedHosts = useQuery({ queryKey: ['shared-hosts'], queryFn: portalApi.sharedHosts });
  const [name, setName] = useState('');
  const [ttl, setTtl] = useState('');
  const [hostIDs, setHostIDs] = useState<string[]>([]);
  const [scopes, setScopes] = useState<string[]>([]);
  const [secret, setSecret] = useState<{ key: string; expires_at: string } | null>(null);
  const [removing, setRemoving] = useState<ApiKey | null>(null);
  const [now, setNow] = useState(0);
  const selectableHosts = useMemo(() => [
    ...(hosts.data?.items ?? []).map((host: Host): SelectableHost => ({ id: host.id, name: host.name, state: host.state, shared: false })),
    ...(sharedHosts.data?.items ?? [])
      .filter((host: SharedHost) => host.permissions.includes('mcp:control'))
      .map((host: SharedHost): SelectableHost => ({ id: host.id, name: host.name, state: host.state, shared: true })),
  ], [hosts.data, sharedHosts.data]);
  const hostNames = useMemo(() => new Map(selectableHosts.map((host) => [host.id, host.shared ? `${host.name}（共享）` : host.name])), [selectableHosts]);
  useEffect(() => {
    const update = () => setNow(Date.now());
    update();
    const timer = window.setInterval(update, 60_000);
    return () => window.clearInterval(timer);
  }, []);
  const create = useMutation({
    mutationFn: (body: { name: string; host_ids: string[]; scopes: string[]; ttl_seconds?: number }) =>
      portalApi.createApiKey(body as Parameters<typeof portalApi.createApiKey>[0]),
    onSuccess: (result) => {
      setSecret(result);
      setName('');
      setTtl('');
      setHostIDs([]);
      setScopes([]);
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
    create.mutate({ name: name.trim(), host_ids: hostIDs, scopes, ...(seconds ? { ttl_seconds: seconds } : {}) });
  };
  const toggle = (values: string[], value: string, checked: boolean, setValues: (next: string[]) => void) => {
    setValues(checked ? [...values, value] : values.filter((item) => item !== value));
  };

  return <section>
    <div className="page-heading"><div><p className="eyebrow">账号安全</p><h1>API Key</h1><p className="muted">为自动化和 MCP 创建权限受限的密钥。密钥明文只显示一次。</p></div></div>
    <form className="form-card" onSubmit={(event) => void submit(event)}>
      <div className="inline-form">
        <div><label htmlFor="apikey-name">名称</label><input id="apikey-name" required maxLength={100} value={name} onChange={(event) => setName(event.target.value)} placeholder="例如：个人自动化" /></div>
        <div><label htmlFor="apikey-ttl">有效期（天，可选）</label><input id="apikey-ttl" type="number" min={1} max={365} value={ttl} onChange={(event) => setTtl(event.target.value)} placeholder="最长 365 天" /></div>
      </div>

      <fieldset className="fieldset">
        <legend>可访问的主机</legend>
        <p className="muted">未选择主机表示不能访问现有主机。共享主机仅显示已授予你 MCP 控制权限的主机；权限被撤回后，此 Key 会立即失去该主机的 MCP 操作权限。勾选“创建新主机”后，此密钥创建的主机也会自动加入授权。</p>
        {hosts.isPending || sharedHosts.isPending ? <p role="status">正在读取主机…</p> : hosts.isError || sharedHosts.isError ? <div className="error-card" role="alert">{errorDetails(hosts.error ?? sharedHosts.error).message}<button className="button button-secondary" type="button" onClick={() => { void hosts.refetch(); void sharedHosts.refetch(); }}>重试</button></div> : !selectableHosts.length ? <p className="muted">当前没有可授权的主机，可以只授予创建新主机权限。</p> : <div>
          {selectableHosts.map((host) => <label className="checkbox-row" key={host.id}>
            <input type="checkbox" checked={hostIDs.includes(host.id)} onChange={(event) => toggle(hostIDs, host.id, event.target.checked, setHostIDs)} />
            <span>{host.name}{host.shared ? ' · 共享' : ''}<small className="table-sub mono">{host.id} · {host.state}{host.shared ? ' · mcp:control' : ''}</small></span>
          </label>)}
        </div>}
      </fieldset>

      <fieldset className="fieldset">
        <legend>权限范围</legend>
        <p className="muted">不预选任何权限。只勾选自动化确实需要的操作。</p>
        <div>
          {scopeOptions.map(([scope, label]) => <label className="checkbox-row" key={scope}>
            <input type="checkbox" checked={scopes.includes(scope)} onChange={(event) => toggle(scopes, scope, event.target.checked, setScopes)} />
            <span>{label}<small className="table-sub mono">{scope}</small></span>
          </label>)}
        </div>
      </fieldset>
      <button className="button button-primary" type="submit" disabled={!name.trim() || scopes.length === 0 || hosts.isPending || hosts.isError || sharedHosts.isPending || sharedHosts.isError || create.isPending}>
        {create.isPending ? '创建中…' : '创建 API Key'}
      </button>
    </form>

    <h2 className="section-title">已创建的 Key</h2>
    {keys.isPending ? <div className="loading-card">正在加载…</div> : keys.isError ? <div className="error-card" role="alert">{errorDetails(keys.error).message}<button className="button button-secondary" type="button" onClick={() => void keys.refetch()}>重试</button></div> : !keys.data?.items.length ? <div className="empty-card compact"><p>尚未创建 API Key。</p></div> : <div className="table-wrap"><table><thead><tr><th>名称 / 主机</th><th>权限</th><th>创建时间</th><th>最近使用</th><th>过期时间</th><th>状态</th><th>操作</th></tr></thead><tbody>
      {keys.data.items.map((key) => {
        const expired = now > 0 && Date.parse(key.expires_at) <= now;
        return <tr key={key.id}>
          <td><strong>{key.name || '未命名'}</strong><small className="table-sub mono">{key.id}</small><small className="table-sub">主机：{key.host_ids.length ? key.host_ids.map((id) => hostNames.get(id) || id).join('、') : key.scopes.includes('host:create') ? '无既有主机（新建主机会获得授权）' : '无主机授权'}</small></td>
          <td>{key.scopes.length ? key.scopes.map((scope) => <small className="table-sub mono" key={scope}>{scope}</small>) : <small className="table-sub">无权限（旧 Key 默认拒绝）</small>}</td>
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
