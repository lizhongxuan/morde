import { useEffect, useState } from 'react';
import type { FormEvent } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { errorDetails, portalApi } from '../app/api';
import { useToast } from '../app/toast';
import { ConfirmDialog } from '../components/ConfirmDialog';
import { ScreenPagination } from '../components/ScreenPagination';
import { FloatingError } from '../components/FloatingError';

type Connector = Awaited<ReturnType<typeof portalApi.mcpConnectors>>['items'][number];
type ConnectorDraft = { name: string; endpointURL: string; credentialJSON: string };
type OAuthSetup = { connector: Connector; developerURL: string; redirectURI: string; clientID: string; clientSecret: string };
type OAuthFlow = { connector: Connector; popup: Window; redirectURI: string; credentialVersion: number };
const blankDraft: ConnectorDraft = { name: '', endpointURL: '', credentialJSON: '' };
function connectorListHeight(itemCount: number) {
  return itemCount <= 2 ? 'min(38dvh, 440px)' : 'min(56dvh, 560px)';
}
const connectorPresets = {
  custom: { name: '', endpointURL: '', kind: 'custom' },
  github: { name: 'GitHub', endpointURL: 'https://api.githubcopilot.com/mcp/', kind: 'github' },
  google_gmail: { name: 'Gmail', endpointURL: 'https://gmailmcp.googleapis.com/mcp/v1', kind: 'google_workspace' },
  google_calendar: { name: 'Google Calendar', endpointURL: 'https://calendarmcp.googleapis.com/mcp/v1', kind: 'google_workspace' },
  google_drive: { name: 'Google Drive', endpointURL: 'https://drivemcp.googleapis.com/mcp/v1', kind: 'google_workspace' },
  google_docs: { name: 'Google Docs', endpointURL: 'https://docsmcp.googleapis.com/mcp/v1', kind: 'google_workspace' },
  google_sheets: { name: 'Google Sheets', endpointURL: 'https://sheetsmcp.googleapis.com/mcp/v1', kind: 'google_workspace' },
  google_slides: { name: 'Google Slides', endpointURL: 'https://slidesmcp.googleapis.com/mcp/v1', kind: 'google_workspace' },
  google_chat: { name: 'Google Chat', endpointURL: 'https://chatmcp.googleapis.com/mcp/v1', kind: 'google_workspace' },
  google_people: { name: 'Google People', endpointURL: 'https://people.googleapis.com/mcp/v1', kind: 'google_workspace' },
  google_classroom: { name: 'Google Classroom', endpointURL: 'https://classroommcp.googleapis.com/mcp/v1', kind: 'google_workspace' },
} as const;

function connectorAddressLabel(value: string) {
  try {
    const url = new URL(value);
    if (url.protocol !== 'https:' || url.username || url.password) return '服务地址待核对';
    const address = `${url.host}${url.pathname === '/' ? '' : url.pathname}`;
    return address.length > 100 ? `${address.slice(0, 97)}…` : address;
  } catch {
    return '服务地址待核对';
  }
}

function requireHTTPSURL(value: string, label: string): string {
  try {
    const url = new URL(value);
    if (url.protocol !== 'https:' || url.username || url.password) throw new Error();
    return url.toString();
  } catch {
    throw new Error(`${label} 必须是有效的 HTTPS 地址。`);
  }
}

function parseCredential(text: string): Record<string, unknown> | undefined {
  if (!text.trim()) return undefined;
  let parsed: unknown;
  try { parsed = JSON.parse(text); } catch { throw new Error('凭据 JSON 格式无效。'); }
  if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) throw new Error('凭据必须是 JSON 对象。');
  return parsed as Record<string, unknown>;
}

export function MCPConnectorsPanel() {
  const client = useQueryClient();
  const { notify } = useToast();
  const connectors = useQuery({ queryKey: ['mcp-connectors'], queryFn: portalApi.mcpConnectors, retry: false });
  const [showCreate, setShowCreate] = useState(false);
  const [draft, setDraft] = useState<ConnectorDraft>(blankDraft);
  const [presetID, setPresetID] = useState<keyof typeof connectorPresets>('custom');
  const [githubToken, setGithubToken] = useState('');
  const [editing, setEditing] = useState<Connector | null>(null);
  const [editDraft, setEditDraft] = useState({ name: '', endpointURL: '' });
  const [rotating, setRotating] = useState<Connector | null>(null);
  const [credentialJSON, setCredentialJSON] = useState('');
  const [creating, setCreating] = useState(false);
  const [savingCredential, setSavingCredential] = useState(false);
  const [removing, setRemoving] = useState<Connector | null>(null);
  const [oauthSetup, setOAuthSetup] = useState<OAuthSetup | null>(null);
  const [oauthFlow, setOAuthFlow] = useState<OAuthFlow | null>(null);
  const [oauthStartingID, setOAuthStartingID] = useState<string | null>(null);
  const refresh = () => client.invalidateQueries({ queryKey: ['mcp-connectors'] });
  const update = useMutation({
    mutationFn: ({ id, body }: { id: string; body: Parameters<typeof portalApi.updateMCPConnector>[1] }) => portalApi.updateMCPConnector(id, body),
    onSuccess: async (saved) => { setEditing(null); await refresh(); notify(saved.has_credential ? 'MCP 工具连接已更新。' : '连接已更新；服务器地址变化后需要重新填写凭据。', 'success'); },
    onError: (error) => notify(errorDetails(error).message, 'error'),
  });
  const setEnabled = useMutation({
    mutationFn: ({ connector, enabled }: { connector: Connector; enabled: boolean }) => portalApi.setMCPConnectorEnabled(connector.id, { enabled, expected_revision: connector.revision }),
    onSuccess: async (_saved, { enabled }) => { await refresh(); notify(enabled ? '连接已启用，可用于新任务。' : '连接已停用。', 'success'); },
    onError: (error) => notify(errorDetails(error).message, 'error'),
  });
  const remove = useMutation({
    mutationFn: (connector: Connector) => portalApi.deleteMCPConnector(connector.id, connector.revision),
    onSuccess: async () => { setRemoving(null); await refresh(); notify('MCP 工具连接已删除。', 'success'); },
    onError: (error) => notify(errorDetails(error).message, 'error'),
  });

  const submitCreate = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const submittedGitHubToken = githubToken.trim();
    setGithubToken('');
    try {
      const preset = connectorPresets[presetID];
      const credential = preset.kind === 'github' && submittedGitHubToken
        ? { kind: 'headers', headers: { Authorization: `Bearer ${submittedGitHubToken}` } }
        : preset.kind === 'google_workspace' ? undefined : parseCredential(draft.credentialJSON);
      const body = { name: draft.name.trim(), endpoint_url: draft.endpointURL.trim(), ...(credential ? { credential } : {}) };
      setDraft((current) => ({ ...current, credentialJSON: '' }));
      setCreating(true);
      await portalApi.createMCPConnector(body);
      setDraft(blankDraft);
      setPresetID('custom');
      setShowCreate(false);
      await refresh();
      notify('MCP 工具连接已保存。请启用后再用于新任务。', 'success');
    } catch (error) { notify(errorDetails(error).message, 'error'); }
    finally { setCreating(false); }
  };
  const submitEdit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (!editing) return;
    update.mutate({ id: editing.id, body: { name: editDraft.name.trim(), endpoint_url: editDraft.endpointURL.trim(), expected_revision: editing.revision } });
  };
  const submitCredential = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (!rotating) return;
    try {
      const credential = parseCredential(credentialJSON);
      if (!credential) throw new Error('请输入凭据 JSON 对象。');
      const connector = rotating;
      setCredentialJSON('');
      setSavingCredential(true);
      await portalApi.rotateMCPConnectorCredential(connector.id, { credential, expected_revision: connector.revision });
      setRotating(null);
      await refresh();
      notify('MCP 凭据已加密保存。', 'success');
    } catch (error) { notify(errorDetails(error).message, 'error'); }
    finally { setSavingCredential(false); }
  };
  const startOAuth = async (connector: Connector, client?: { client_id?: string; client_secret?: string }) => {
    const popup = window.open('about:blank', '_blank');
    if (!popup) {
      notify('浏览器拦截了 OAuth 授权窗口，请允许弹窗后重试。', 'error');
      return;
    }
    popup.opener = null;
    setOAuthStartingID(connector.id);
    try {
      const response = await portalApi.beginMCPConnectorOAuth(connector.id, {
        expected_revision: connector.revision,
        ...(client?.client_id ? { client_id: client.client_id } : {}),
        ...(client?.client_secret ? { client_secret: client.client_secret } : {}),
      });
      if (response.kind === 'client_required') {
        popup.close();
        setOAuthSetup({
          connector,
          developerURL: requireHTTPSURL(response.developer_url, 'OAuth 开发者页面'),
          redirectURI: requireHTTPSURL(response.redirect_uri, 'OAuth 回调地址'),
          clientID: client?.client_id ?? '',
          clientSecret: '',
        });
        return;
      }
      const redirectURI = requireHTTPSURL(response.redirect_uri, 'OAuth 回调地址');
      popup.location.href = requireHTTPSURL(response.authorization_url, 'OAuth 授权地址');
      setOAuthSetup(null);
      setOAuthFlow({ connector, popup, redirectURI, credentialVersion: connector.credential_version });
    } catch (error) {
      popup.close();
      notify(errorDetails(error).message, 'error');
    } finally {
      setOAuthStartingID(null);
    }
  };
  const submitOAuthSetup = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (!oauthSetup) return;
    const { connector, clientID, clientSecret } = oauthSetup;
    setOAuthSetup((current) => current ? { ...current, clientSecret: '' } : null);
    void startOAuth(connector, {
      ...(clientID.trim() ? { client_id: clientID.trim() } : {}),
      ...(clientSecret ? { client_secret: clientSecret } : {}),
    });
  };

  useEffect(() => {
    if (!oauthFlow) return;
    let finished = false;
    let checking = false;
    const checkStatus = async () => {
      if (finished || checking) return;
      checking = true;
      try {
        const latest = await client.fetchQuery({
          queryKey: ['mcp-connectors'], queryFn: portalApi.mcpConnectors, staleTime: 0,
        });
        const current = latest.items.find((item) => item.id === oauthFlow.connector.id);
        let callbackReached = false;
        try {
          const location = new URL(oauthFlow.popup.location.href);
          const callback = new URL(oauthFlow.redirectURI);
          callbackReached = location.origin === callback.origin && location.pathname === callback.pathname;
        } catch {
          // The authorization window is normally cross-origin until the provider redirects back.
        }
        if (callbackReached && current && current.credential_version > oauthFlow.credentialVersion) {
          finished = true;
          oauthFlow.popup.close();
          setOAuthFlow(null);
          notify('MCP OAuth 授权完成，凭据已由 Morde 安全保存。', 'success');
        } else if (oauthFlow.popup.closed) {
          finished = true;
          setOAuthFlow(null);
          notify('OAuth 授权未完成或已取消；连接状态已刷新。', 'info');
        }
      } catch {
        if (oauthFlow.popup.closed) {
          finished = true;
          setOAuthFlow(null);
          notify('授权窗口已关闭，但暂时无法确认连接状态；请刷新连接列表。', 'error');
        }
      } finally {
        checking = false;
      }
    };
    const onVisibilityChange = () => { if (document.visibilityState === 'visible') void checkStatus(); };
    const timer = window.setInterval(() => { void checkStatus(); }, 1500);
    window.addEventListener('focus', checkStatus);
    document.addEventListener('visibilitychange', onVisibilityChange);
    void checkStatus();
    return () => {
      finished = true;
      window.clearInterval(timer);
      window.removeEventListener('focus', checkStatus);
      document.removeEventListener('visibilitychange', onVisibilityChange);
    };
  }, [client, notify, oauthFlow]);

  const renderConnector = (connector: Connector) => <article className="model-connection-card">
    <div className="model-connection-main"><div className="model-connection-title"><h3>{connector.name}</h3><span className={`capability ${connector.enabled ? 'capability-yes' : 'capability-no'}`}>{connector.enabled ? '已启用' : '已停用'}</span><span className="capability capability-no">{connector.has_credential ? '凭据已保存' : '无凭据'}</span></div>
      <p className="muted" title={connectorAddressLabel(connector.endpoint_url)}>{connectorAddressLabel(connector.endpoint_url)}</p><small className="muted">仅用于之后创建的任务；已开始的任务权限保持不变。</small>
    </div>
    <div className="model-connection-actions">
      <button type="button" className="button button-secondary button-small" disabled={Boolean(oauthFlow) || oauthStartingID !== null} onClick={() => { setOAuthSetup(null); void startOAuth(connector); }}>{connector.has_credential ? '重新授权 OAuth' : 'OAuth 授权'}</button>
      <button type="button" className="button button-secondary button-small" disabled={setEnabled.isPending} onClick={() => setEnabled.mutate({ connector, enabled: !connector.enabled })}>{connector.enabled ? '停用' : '启用'}</button>
      <button type="button" className="button button-secondary button-small" onClick={() => { setEditing(connector); setEditDraft({ name: connector.name, endpointURL: connector.endpoint_url }); }}>编辑</button>
      <button type="button" className="button button-secondary button-small" onClick={() => { setRotating(connector); setCredentialJSON(''); }}>更新凭据</button>
      <button type="button" className="button button-danger-quiet button-small" onClick={() => setRemoving(connector)}>删除</button>
    </div>
  </article>;

  return <section className="connection-section" aria-labelledby="mcp-connectors-title">
    <div className="connection-section-heading"><div><h2 id="mcp-connectors-title">MCP 工具连接</h2><p className="muted">连接你信任的 MCP 服务；启用的连接只会授权给你之后新建的任务。地址仅支持 HTTPS 公网服务。</p></div>
      <button type="button" className="button button-primary" aria-expanded={showCreate} disabled={creating} onClick={() => setShowCreate((open) => !open)}>{showCreate ? '收起' : '＋ 添加工具连接'}</button>
    </div>
    {showCreate && <form className="connection-form form-card" onSubmit={submitCreate}>
      <div className="connection-form-grid">
        <div className="connection-form-wide"><label htmlFor="mcp-connector-preset">连接模板</label><select id="mcp-connector-preset" value={presetID} onChange={(event) => {
          const selected = event.target.value as keyof typeof connectorPresets;
          setPresetID(selected);
          setGithubToken('');
          setDraft((current) => ({ ...current, name: connectorPresets[selected].name, endpointURL: connectorPresets[selected].endpointURL, credentialJSON: '' }));
        }}><option value="custom">自定义 MCP 服务</option><option value="github">GitHub 官方 MCP Server</option>
          <optgroup label="Google Workspace 官方 MCP">{Object.entries(connectorPresets).filter(([id]) => id.startsWith('google_'))
            .map(([id, preset]) => <option key={id} value={id}>{preset.name}</option>)}</optgroup></select>
          {presetID === 'github' && <small className="muted">使用 GitHub 官方远程 MCP 服务。账号或组织策略可能限制访问；也可保存连接后再使用 OAuth 授权。工具权限仍按每个任务的授权和确认规则执行。<a href="https://github.com/github/github-mcp-server" target="_blank" rel="noopener noreferrer">查看项目说明</a></small>}
          {connectorPresets[presetID].kind === 'google_workspace' && <small className="muted">使用 Google 官方托管的 Workspace MCP 服务。先在 Google Cloud 启用对应产品 API 与 MCP API；保存后选择 OAuth 授权，并按提示配置 OAuth 客户端及 Morde 回调地址。授权范围由 Google 在登录时展示，工具仍受每个任务的 Morde 授权与确认规则约束。<a href="https://developers.google.com/workspace/guides/configure-mcp-servers" target="_blank" rel="noopener noreferrer">查看 Google 配置说明</a></small>}
        </div>
        <div><label htmlFor="mcp-connector-name">连接名称</label><input id="mcp-connector-name" required maxLength={80} value={draft.name} onChange={(event) => setDraft((current) => ({ ...current, name: event.target.value }))} placeholder="例如：日历" /></div>
        <div><label htmlFor="mcp-connector-url">MCP 服务地址</label><input id="mcp-connector-url" type="url" pattern="https://.*" required readOnly={presetID !== 'custom'} value={draft.endpointURL} onChange={(event) => setDraft((current) => ({ ...current, endpointURL: event.target.value }))} placeholder="https://mcp.example.com/mcp" /></div>
        {presetID === 'github' ? <div className="connection-form-wide"><label htmlFor="mcp-github-token">GitHub Personal Access Token（可选）</label><input id="mcp-github-token" type="password" autoComplete="new-password" spellCheck={false} value={githubToken} onChange={(event) => setGithubToken(event.target.value)} placeholder="仅授予任务所需的最小权限" /><small className="muted">提交后立即清空，并由 Morde 加密保存为 Authorization Bearer 凭据；令牌只发送到上方 GitHub MCP 服务。也可以留空，保存后使用 OAuth。</small></div>
          : connectorPresets[presetID].kind === 'google_workspace' ? <p className="connection-form-wide muted">此服务使用 Google OAuth。连接保存后，在连接卡片中启动授权；Morde 不要求你粘贴 Google 访问令牌。</p>
            : <div className="connection-form-wide"><label htmlFor="mcp-connector-credential">认证凭据 JSON（可选）</label><textarea id="mcp-connector-credential" rows={3} maxLength={32768} autoComplete="off" spellCheck={false} value={draft.credentialJSON} onChange={(event) => setDraft((current) => ({ ...current, credentialJSON: event.target.value }))} placeholder={'{"kind":"headers","headers":{"Authorization":"Bearer …"}}'} /><small className="muted">仅接受服务端支持的请求头凭据格式。提交后立即清空并由 Morde 加密保存；不要粘贴共享账号的密钥。</small></div>}
      </div>
      <div className="form-actions"><button type="button" className="button button-secondary" disabled={creating} onClick={() => { setShowCreate(false); setDraft(blankDraft); setPresetID('custom'); setGithubToken(''); }}>取消</button><button type="submit" className="button button-primary" disabled={creating}>{creating ? '正在保存…' : '保存连接'}</button></div>
    </form>}
    {connectors.isPending ? <div className="loading-card" role="status">正在读取工具连接…</div>
      : connectors.isError ? <FloatingError className="error-card" role="alert">{errorDetails(connectors.error).message}<button type="button" className="button button-secondary" onClick={() => void connectors.refetch()}>重试</button></FloatingError>
        : !connectors.data.items.length ? <div className="empty-card connection-empty"><h3>尚未添加 MCP 工具</h3><p>连接器可在发送任务时按需授权，未选择的连接不会提供给任务。</p></div>
        : connectors.data.items.length === 1 ? <div className="model-connection-list">{renderConnector(connectors.data.items[0]!)}</div>
          : <ScreenPagination
            items={connectors.data.items}
            getKey={(connector) => connector.id}
            estimatedItemHeight={150}
            height={connectorListHeight(connectors.data.items.length)}
            ariaLabel="MCP 工具连接列表"
            className="model-connection-list"
            renderItem={renderConnector}
          />}
    {oauthFlow && <div className="loading-card" role="status">正在等待 {oauthFlow.connector.name} 的 OAuth 授权；完成后会自动刷新连接状态。关闭授权窗口可取消。</div>}
    {oauthSetup && <form className="connection-form form-card" aria-label={`配置 ${oauthSetup.connector.name} OAuth 客户端`} onSubmit={submitOAuthSetup}>
      <p>此 MCP 服务需要先创建 OAuth 客户端。请在 <a href={oauthSetup.developerURL} target="_blank" rel="noopener noreferrer">服务商开发者页面</a> 创建后填写客户端信息。</p>
      <div><label htmlFor="mcp-oauth-client-id">OAuth 客户端 ID</label><input id="mcp-oauth-client-id" autoComplete="off" maxLength={2048} value={oauthSetup.clientID} onChange={(event) => setOAuthSetup((current) => current ? { ...current, clientID: event.target.value } : null)} /></div>
      <div><label htmlFor="mcp-oauth-client-secret">OAuth 客户端密钥（可选）</label><input id="mcp-oauth-client-secret" type="password" autoComplete="new-password" maxLength={4096} value={oauthSetup.clientSecret} onChange={(event) => setOAuthSetup((current) => current ? { ...current, clientSecret: event.target.value } : null)} /></div>
      <p className="muted">请在 OAuth 应用中配置此回调地址：<code>{oauthSetup.redirectURI}</code></p>
      <small className="muted">客户端密钥只随本次请求发送给 Morde 服务端；不会显示在授权地址或连接列表中。</small>
      <div className="form-actions"><button type="button" className="button button-secondary" onClick={() => setOAuthSetup(null)}>取消</button><button type="submit" className="button button-primary" disabled={oauthStartingID !== null}>{oauthStartingID === oauthSetup.connector.id ? '正在准备授权…' : '继续授权'}</button></div>
    </form>}
    {editing && <form className="connection-form form-card" aria-label="编辑 MCP 工具连接" onSubmit={submitEdit}>
      <div className="connection-form-grid"><div><label htmlFor="mcp-edit-name">连接名称</label><input id="mcp-edit-name" required maxLength={80} value={editDraft.name} onChange={(event) => setEditDraft((current) => ({ ...current, name: event.target.value }))} /></div>
        <div><label htmlFor="mcp-edit-url">MCP 服务地址</label><input id="mcp-edit-url" type="url" pattern="https://.*" required value={editDraft.endpointURL} onChange={(event) => setEditDraft((current) => ({ ...current, endpointURL: event.target.value }))} /><small className="muted">修改地址会清除已保存的认证凭据。</small></div></div>
      <div className="form-actions"><button type="button" className="button button-secondary" onClick={() => setEditing(null)}>取消</button><button type="submit" className="button button-primary" disabled={update.isPending}>{update.isPending ? '正在保存…' : '保存修改'}</button></div>
    </form>}
    {rotating && <form className="connection-form form-card" aria-label={`更新 ${rotating.name} 凭据`} onSubmit={submitCredential}>
      <div><label htmlFor="mcp-rotate-credential">{rotating.name} 的凭据 JSON</label><textarea id="mcp-rotate-credential" rows={3} required maxLength={32768} autoComplete="off" spellCheck={false} value={credentialJSON} onChange={(event) => setCredentialJSON(event.target.value)} placeholder={'{"kind":"headers","headers":{"Authorization":"Bearer …"}}'} /><small className="muted">保存后无法再次查看凭据；Morde 会加密保存。</small></div>
      <div className="form-actions"><button type="button" className="button button-secondary" disabled={savingCredential} onClick={() => { setRotating(null); setCredentialJSON(''); }}>取消</button><button type="submit" className="button button-primary" disabled={savingCredential}>{savingCredential ? '正在保存…' : '更新凭据'}</button></div>
    </form>}
    {removing && <ConfirmDialog title="删除 MCP 工具连接" description={`删除“${removing.name}”后，新任务将无法使用此连接。正在运行的任务不受影响。`} confirmLabel="删除连接" danger busy={remove.isPending} onClose={() => setRemoving(null)} onConfirm={() => remove.mutate(removing)} />}
  </section>;
}
