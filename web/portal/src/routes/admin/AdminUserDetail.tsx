import { useState } from 'react';
import type { FormEvent } from 'react';
import { Link, useParams } from 'react-router';
import { useInfiniteQuery, useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { errorDetails, portalApi } from '../../app/api';
import { formatBytes, formatCpu, formatDate, formatMemory, hostStateLabel } from '../../app/format';
import { useToast } from '../../app/toast';

type AdminUser = Awaited<ReturnType<typeof portalApi.adminUsers>>['items'][number];
type UnknownModelCall = Awaited<ReturnType<typeof portalApi.adminUnknownModelUsage>>['items'][number];
type ModelAccountBudget = Awaited<ReturnType<typeof portalApi.adminModelAccountBudget>>;
type PersonalDataQuota = Awaited<ReturnType<typeof portalApi.adminPersonalDataQuota>>;
type Installation = Awaited<ReturnType<typeof portalApi.adminUserInstallations>>['items'][number];
type Authorization = Awaited<ReturnType<typeof portalApi.adminUserAuthorizations>>['items'][number];
type ModelCall = Awaited<ReturnType<typeof portalApi.adminUserModelCalls>>['items'][number];

const modelUsageSourceLabel: Record<UnknownModelCall['source'], string> = {
  client: '客户端', chat: '对话', task: '云端任务', probe: '连接检查',
};

function UserResourceEditor({ user }: { user: AdminUser }) {
  const client = useQueryClient();
  const { notify } = useToast();
  const [limit, setLimit] = useState(String(user.quota.hosts));
  const [cpu, setCPU] = useState(String(user.default_host_resources.cpu_milli));
  const [memory, setMemory] = useState(String(user.default_host_resources.mem_mib));
  const save = useMutation({
    mutationFn: () => portalApi.updateUser(user.id, {
      expected_status: user.status,
      quota: { ...user.quota, hosts: Number(limit) },
      default_host_resources: { cpu_milli: Number(cpu), mem_mib: Number(memory) },
    }),
    onSuccess: () => { notify('云端电脑设置已保存。', 'success'); void client.invalidateQueries({ queryKey: ['admin', 'users'] }); },
    onError: (error) => notify(errorDetails(error).message, 'error'),
  });
  const submit = (event: FormEvent<HTMLFormElement>) => { event.preventDefault(); save.mutate(); };
  if (user.role === 'super_admin') return null;
  return <section className="admin-detail-section"><h2>云端电脑设置</h2><form className="form-card form-stack" onSubmit={submit}>
    <label htmlFor="user-host-limit">电脑数量上限</label><input id="user-host-limit" type="number" min="0" max="1000" required value={limit} onChange={(event) => setLimit(event.target.value)} />
    <label htmlFor="user-default-cpu">新电脑 CPU（毫核）</label><input id="user-default-cpu" type="number" min="1" required value={cpu} onChange={(event) => setCPU(event.target.value)} />
    <label htmlFor="user-default-memory">新电脑内存（MiB）</label><input id="user-default-memory" type="number" min="1" required value={memory} onChange={(event) => setMemory(event.target.value)} />
    <p className="muted">默认规格只用于之后创建的电脑；已有电脑请在对应详情页调整。</p>
    <div className="form-actions"><button className="button button-primary" disabled={save.isPending} type="submit">{save.isPending ? '保存中…' : '保存设置'}</button></div>
  </form></section>;
}

function UnknownModelCallEditor({ item, ownerID }: { item: UnknownModelCall; ownerID: string }) {
  const client = useQueryClient();
  const { notify } = useToast();
  const [inputTokens, setInputTokens] = useState('');
  const [outputTokens, setOutputTokens] = useState('');
  const [evidence, setEvidence] = useState('');
  const queryKey = ['admin', 'model-usage', ownerID];
  const reconcile = useMutation({
    mutationFn: () => portalApi.reconcileModelUsage({
      owner_id: item.owner_id,
      source: item.source,
      call_id: item.call_id,
      input_tokens: Number(inputTokens),
      output_tokens: Number(outputTokens),
      evidence,
    }),
    onSuccess: (result) => {
      notify(result.replayed ? '这笔用量已核对过。' : '模型用量已核对并记账。', 'success');
      void client.invalidateQueries({ queryKey });
    },
    onError: (error) => notify(errorDetails(error).message, 'error'),
  });
  const submit = (event: FormEvent<HTMLFormElement>) => { event.preventDefault(); reconcile.mutate(); };
  return <article className="form-card form-stack">
    <div><strong>{item.provider_model}</strong><p className="muted">{modelUsageSourceLabel[item.source]} · {formatDate(item.unknown_at)} · 预留 {item.reserved_tokens.toLocaleString()} tokens</p></div>
    <p className="muted mono">调用 {item.call_id}{item.task_id ? ` · 任务 ${item.task_id}` : ''}</p>
    <form className="form-stack" onSubmit={submit}>
      <label htmlFor={`model-usage-input-${item.call_id}`}>核实的输入 tokens</label>
      <input id={`model-usage-input-${item.call_id}`} type="number" min="0" max={Number.MAX_SAFE_INTEGER} step="1" required value={inputTokens} onChange={(event) => setInputTokens(event.target.value)} />
      <label htmlFor={`model-usage-output-${item.call_id}`}>核实的输出 tokens</label>
      <input id={`model-usage-output-${item.call_id}`} type="number" min="0" max={Number.MAX_SAFE_INTEGER} step="1" required value={outputTokens} onChange={(event) => setOutputTokens(event.target.value)} />
      <label htmlFor={`model-usage-evidence-${item.call_id}`}>供应商记录与核对依据</label>
      <input id={`model-usage-evidence-${item.call_id}`} maxLength={512} minLength={8} required value={evidence} onChange={(event) => setEvidence(event.target.value)} placeholder="填写可关联的供应商记录，不要粘贴密钥或请求内容" />
      <div className="form-actions"><button className="button button-primary" disabled={reconcile.isPending} type="submit">{reconcile.isPending ? '正在核对…' : '保存核对结果'}</button></div>
    </form>
  </article>;
}

function ModelAccountBudgetForm({ ownerID, budget }: { ownerID: string; budget: ModelAccountBudget }) {
  const client = useQueryClient();
  const { notify } = useToast();
  const [tokenLimit, setTokenLimit] = useState(String(budget.token_limit || ''));
  const [maxConcurrent, setMaxConcurrent] = useState(String(budget.max_concurrent || ''));
  const queryKey = ['admin', 'model-usage-budget', ownerID];
  const save = useMutation({
    mutationFn: () => portalApi.setAdminModelAccountBudget(ownerID, {
      token_limit: Number(tokenLimit), max_concurrent: Number(maxConcurrent),
    }),
    onSuccess: (updated) => {
      client.setQueryData(queryKey, updated);
      notify('平台模型预算已保存。', 'success');
    },
    onError: (error) => notify(errorDetails(error).message, 'error'),
  });
  const submit = (event: FormEvent<HTMLFormElement>) => { event.preventDefault(); save.mutate(); };
  return <>
    <div className="admin-detail-facts">
      <article><span>平台模型预算</span><strong>{budget.configured ? `${budget.used_tokens.toLocaleString()} / ${budget.token_limit.toLocaleString()} tokens` : '尚未配置'}</strong></article>
      <article><span>当前并发调用</span><strong>{budget.in_flight} / {budget.max_concurrent || '—'}</strong></article>
      <article><span>未知用量</span><strong>{budget.has_unknown ? '需要核对' : '无'}</strong></article>
    </div>
    <form className="form-card form-stack" onSubmit={submit}>
      <label htmlFor="user-model-token-limit">平台模型 token 上限</label>
      <input id="user-model-token-limit" type="number" min="1" max={Number.MAX_SAFE_INTEGER} step="1" required value={tokenLimit} onChange={(event) => setTokenLimit(event.target.value)} />
      <label htmlFor="user-model-concurrency">同时进行的模型调用数</label>
      <input id="user-model-concurrency" type="number" min="1" max="100" step="1" required value={maxConcurrent} onChange={(event) => setMaxConcurrent(event.target.value)} />
      <p className="muted">预算统计已结算用量、在途预留和未知用量。降低上限不会中止在途调用，只会阻止新的调用，直到用量低于上限。</p>
      <div className="form-actions"><button className="button button-primary" disabled={save.isPending} type="submit">{save.isPending ? '保存中…' : '保存预算'}</button></div>
    </form>
  </>;
}

function ModelAccountBudgetEditor({ ownerID }: { ownerID: string }) {
  const query = useQuery({
    queryKey: ['admin', 'model-usage-budget', ownerID],
    queryFn: () => portalApi.adminModelAccountBudget(ownerID),
    retry: false,
  });
  return <section className="admin-detail-section">
    <div className="dashboard-section-heading"><div><h2>平台模型预算</h2><p>设置此账号的平台模型调用额度和并发数。</p></div>
      {!query.isPending && <button className="button button-secondary" type="button" onClick={() => void query.refetch()}>刷新</button>}
    </div>
    {query.isPending && <div className="loading-card" role="status">正在读取模型预算…</div>}
    {query.isError && <div className="error-card" role="alert">{errorDetails(query.error).message}<button className="button button-secondary" type="button" onClick={() => void query.refetch()}>重试</button></div>}
    {query.data && <ModelAccountBudgetForm key={`${query.data.configured}:${query.data.token_limit}:${query.data.max_concurrent}`} ownerID={ownerID} budget={query.data} />}
  </section>;
}

function PersonalDataQuotaForm({ ownerID, quota }: { ownerID: string; quota: PersonalDataQuota }) {
  const client = useQueryClient();
  const { notify } = useToast();
  const [quotaBytes, setQuotaBytes] = useState(String(quota.quota_bytes));
  const [maxAttachmentBytes, setMaxAttachmentBytes] = useState(String(quota.max_attachment_bytes));
  const queryKey = ['admin', 'personal-data-quota', ownerID];
  const save = useMutation({
    mutationFn: () => portalApi.setAdminPersonalDataQuota(ownerID, {
      expected_version: quota.version,
      quota_bytes: Number(quotaBytes),
      max_attachment_bytes: Number(maxAttachmentBytes),
    }),
    onSuccess: (updated) => {
      client.setQueryData(queryKey, updated);
      notify('个人数据额度已保存。', 'success');
    },
    onError: (error) => {
      notify(errorDetails(error).message, 'error');
      void client.invalidateQueries({ queryKey });
    },
  });
  const submit = (event: FormEvent<HTMLFormElement>) => { event.preventDefault(); save.mutate(); };
  return <>
    <div className="admin-detail-facts">
      <article><span>已用</span><strong>{formatBytes(quota.used_bytes)}</strong></article>
      <article><span>预留</span><strong>{formatBytes(quota.reserved_bytes)}</strong></article>
      <article><span>当前总额度</span><strong>{formatBytes(quota.quota_bytes)}</strong></article>
      <article><span>当前单个附件上限</span><strong>{formatBytes(quota.max_attachment_bytes)}</strong></article>
      <article><span>默认额度</span><strong>{formatBytes(quota.default_quota_bytes)} 总量 · {formatBytes(quota.default_max_attachment_bytes)} / 附件</strong></article>
    </div>
    <form className="form-card form-stack" onSubmit={submit}>
      <label htmlFor="user-personal-data-quota">个人数据总额度（字节）</label>
      <input id="user-personal-data-quota" type="number" min="1" max={Number.MAX_SAFE_INTEGER} step="1" required value={quotaBytes} onChange={(event) => setQuotaBytes(event.target.value)} />
      <label htmlFor="user-personal-data-attachment-limit">单个附件上限（字节）</label>
      <input id="user-personal-data-attachment-limit" type="number" min="1" max={Number.MAX_SAFE_INTEGER} step="1" required value={maxAttachmentBytes} onChange={(event) => setMaxAttachmentBytes(event.target.value)} />
      <p className="muted">版本 v{quota.version}。额度只用于个人附件，与聊天、Workspace 和记录额度分开计算。</p>
      <div className="form-actions"><button className="button button-primary" disabled={save.isPending} type="submit">{save.isPending ? '保存中…' : '保存个人数据额度'}</button></div>
    </form>
  </>;
}

function PersonalDataQuotaEditor({ ownerID }: { ownerID: string }) {
  const query = useQuery({
    queryKey: ['admin', 'personal-data-quota', ownerID],
    queryFn: () => portalApi.adminPersonalDataQuota(ownerID),
    retry: false,
  });
  return <section className="admin-detail-section">
    <div className="dashboard-section-heading"><div><h2>个人数据额度</h2><p>查看个人附件的已用与预留空间，并按版本调整额度。</p></div>
      {!query.isPending && <button className="button button-secondary" type="button" onClick={() => void query.refetch()}>刷新</button>}
    </div>
    {query.isPending && <div className="loading-card" role="status">正在读取个人数据额度…</div>}
    {query.isError && <div className="error-card" role="alert">{errorDetails(query.error).message}<button className="button button-secondary" type="button" onClick={() => void query.refetch()}>重试</button></div>}
    {query.data && <PersonalDataQuotaForm key={`${query.data.version}:${query.data.quota_bytes}:${query.data.max_attachment_bytes}`} ownerID={ownerID} quota={query.data} />}
  </section>;
}

function ModelUsageReconciliation({ ownerID }: { ownerID: string }) {
  const [opened, setOpened] = useState(false);
  const query = useQuery({
    queryKey: ['admin', 'model-usage', ownerID],
    queryFn: () => portalApi.adminUnknownModelUsage(ownerID),
    enabled: opened,
    retry: false,
  });
  return <section className="admin-detail-section">
    <div className="dashboard-section-heading"><div><h2>未知模型用量核对</h2><p>仅处理已确认结束、但供应商没有返回用量的调用。</p></div>
      {!opened && <button className="button button-secondary" type="button" onClick={() => setOpened(true)}>查看待核对记录</button>}
      {opened && <button className="button button-secondary" type="button" onClick={() => void query.refetch()}>刷新</button>}
    </div>
    {opened && query.isPending && <div className="loading-card" role="status">正在读取待核对记录…</div>}
    {opened && query.isError && <div className="error-card" role="alert">{errorDetails(query.error).message}<button className="button button-secondary" type="button" onClick={() => void query.refetch()}>重试</button></div>}
    {opened && query.data && (query.data.items.length === 0
      ? <div className="empty-card compact"><p>没有待核对的模型用量。</p></div>
      : <div className="form-stack">{query.data.items.map((item) => <UnknownModelCallEditor key={`${item.owner_id}:${item.source}:${item.call_id}`} item={item} ownerID={ownerID} />)}</div>)}
    {opened && <p className="notice notice-info">请先在供应商记录中核实准确的输入和输出数量。保存后只更新用量账本，不会恢复丢失的模型响应或重新执行调用。</p>}
  </section>;
}

function InstallationHistory({ ownerID }: { ownerID: string }) {
  const query = useInfiniteQuery({
    queryKey: ['admin', 'user-installations', ownerID],
    queryFn: ({ pageParam }) => portalApi.adminUserInstallations(ownerID, pageParam),
    initialPageParam: 0,
    getNextPageParam: (page) => page.offset + page.items.length < page.total ? page.offset + page.limit : undefined,
  });
  const items = query.data?.pages.flatMap((page) => page.items) ?? [];
  return <section className="admin-detail-section">
    <div className="dashboard-section-heading"><div><h2>Client 安装</h2><p>安装元数据与当前 Portal 进程内的 Hub 在线状态，不含凭证。</p></div></div>
    {query.isPending && <div className="loading-card" role="status">正在读取安装记录…</div>}
    {query.isError && !query.data && <div className="error-card" role="alert">{errorDetails(query.error).message}<button className="button button-secondary" type="button" onClick={() => void query.refetch()}>重试</button></div>}
    {query.isSuccess && !items.length && <div className="empty-card compact"><p>该用户没有 Client 安装记录。</p></div>}
    {!!items.length && <div className="table-wrap"><table><thead><tr><th>安装</th><th>平台</th><th>在线</th><th>创建时间</th><th>最近使用</th><th>撤权时间</th></tr></thead><tbody>
      {items.map((item: Installation) => <tr key={item.id}><td><strong>{item.display_name}</strong><small className="table-sub mono">{item.id}</small></td><td>{item.platform}</td><td>{item.online ? '当前进程在线' : '离线'}</td><td>{formatDate(item.created_at)}</td><td>{item.last_used_at ? formatDate(item.last_used_at) : '无记录'}</td><td>{item.revoked_at ? formatDate(item.revoked_at) : '未撤权'}</td></tr>)}
    </tbody></table></div>}
    {query.isFetchNextPageError && <div className="error-card admin-pagination-error" role="alert">{errorDetails(query.error).message}<button className="button button-secondary" type="button" onClick={() => void query.fetchNextPage()}>重试</button></div>}
    {query.hasNextPage && !query.isFetchNextPageError && <button className="button button-secondary admin-load-more" type="button" disabled={query.isFetchingNextPage} onClick={() => void query.fetchNextPage()}>{query.isFetchingNextPage ? '正在加载…' : '加载更多安装'}</button>}
  </section>;
}

function AuthorizationHistory({ ownerID }: { ownerID: string }) {
  const query = useInfiniteQuery({
    queryKey: ['admin', 'user-authorizations', ownerID],
    queryFn: ({ pageParam }) => portalApi.adminUserAuthorizations(ownerID, pageParam),
    initialPageParam: 0,
    getNextPageParam: (page) => page.offset + page.items.length < page.total ? page.offset + page.limit : undefined,
  });
  const items = query.data?.pages.flatMap((page) => page.items) ?? [];
  return <section className="admin-detail-section">
    <div className="dashboard-section-heading"><div><h2>客户端授权</h2><p>仅显示已关联到此用户的授权记录；用户码和凭证不会展示。</p></div></div>
    {query.isPending && <div className="loading-card" role="status">正在读取授权记录…</div>}
    {query.isError && !query.data && <div className="error-card" role="alert">{errorDetails(query.error).message}<button className="button button-secondary" type="button" onClick={() => void query.refetch()}>重试</button></div>}
    {query.isSuccess && !items.length && <div className="empty-card compact"><p>没有可归属到该用户的客户端授权记录。</p></div>}
    {!!items.length && <div className="table-wrap"><table><thead><tr><th>授权</th><th>平台</th><th>状态</th><th>创建时间</th><th>到期时间</th><th>认领时间</th></tr></thead><tbody>
      {items.map((item: Authorization) => <tr key={item.id}><td><strong>{item.display_name}</strong><small className="table-sub mono">{item.id}</small></td><td>{item.platform}</td><td>{item.state}</td><td>{formatDate(item.created_at)}</td><td>{formatDate(item.expires_at)}</td><td>{item.claimed_at ? formatDate(item.claimed_at) : '未认领'}</td></tr>)}
    </tbody></table></div>}
    {query.isFetchNextPageError && <div className="error-card admin-pagination-error" role="alert">{errorDetails(query.error).message}<button className="button button-secondary" type="button" onClick={() => void query.fetchNextPage()}>重试</button></div>}
    {query.hasNextPage && !query.isFetchNextPageError && <button className="button button-secondary admin-load-more" type="button" disabled={query.isFetchingNextPage} onClick={() => void query.fetchNextPage()}>{query.isFetchingNextPage ? '正在加载…' : '加载更多授权'}</button>}
  </section>;
}

function ModelLedgerHistory({ ownerID }: { ownerID: string }) {
  const query = useInfiniteQuery({
    queryKey: ['admin', 'user-model-ledger', ownerID],
    queryFn: ({ pageParam }) => portalApi.adminUserModelCalls(ownerID, pageParam),
    initialPageParam: 0,
    getNextPageParam: (page) => page.offset + page.items.length < page.total ? page.offset + page.limit : undefined,
  });
  const items = query.data?.pages.flatMap((page) => page.items) ?? [];
  return <section className="admin-detail-section">
    <div className="dashboard-section-heading"><div><h2>模型用量账本</h2><p>读取既有 model_call_ledger；只展示模型和计量元数据，不打开请求或响应正文。</p></div></div>
    {query.isPending && <div className="loading-card" role="status">正在读取模型账本…</div>}
    {query.isError && !query.data && <div className="error-card" role="alert">{errorDetails(query.error).message}<button className="button button-secondary" type="button" onClick={() => void query.refetch()}>重试</button></div>}
    {query.isSuccess && !items.length && <div className="empty-card compact"><p>该用户没有模型账本记录。</p></div>}
    {!!items.length && <div className="table-wrap"><table><thead><tr><th>时间</th><th>来源</th><th>模型</th><th>状态</th><th>计量</th></tr></thead><tbody>
      {items.map((item: ModelCall) => <tr key={`${item.source}:${item.call_id}`}><td>{formatDate(item.created_at)}</td><td>{item.source}</td><td><strong>{item.model_id}</strong><small className="table-sub">{item.provider_model}</small></td><td>{item.state}</td><td>{item.input_tokens !== null && item.output_tokens !== null ? `${item.input_tokens} 输入 / ${item.output_tokens} 输出 tokens` : `预留 ${item.reserved_tokens} tokens`}</td></tr>)}
    </tbody></table></div>}
    {query.isFetchNextPageError && <div className="error-card admin-pagination-error" role="alert">{errorDetails(query.error).message}<button className="button button-secondary" type="button" onClick={() => void query.fetchNextPage()}>重试</button></div>}
    {query.hasNextPage && !query.isFetchNextPageError && <button className="button button-secondary admin-load-more" type="button" disabled={query.isFetchingNextPage} onClick={() => void query.fetchNextPage()}>{query.isFetchingNextPage ? '正在加载…' : '加载更多账本记录'}</button>}
  </section>;
}

function UserEvents({ ownerID }: { ownerID: string }) {
  const params = new URLSearchParams({ target: ownerID, limit: '100' });
  const query = useQuery({ queryKey: ['admin', 'user-events', ownerID], queryFn: () => portalApi.audit(params) });
  return <section className="admin-detail-section">
    <div className="dashboard-section-heading"><div><h2>账号事件</h2><p>最近 100 条以用户 ID 为目标的审计记录；不展示事件正文。</p></div></div>
    {query.isPending && <div className="loading-card" role="status">正在读取账号事件…</div>}
    {query.isError && <div className="error-card" role="alert">{errorDetails(query.error).message}<button className="button button-secondary" type="button" onClick={() => void query.refetch()}>重试</button></div>}
    {query.isSuccess && !query.data.items.length && <div className="empty-card compact"><p>暂无账号审计事件。</p></div>}
    {!!query.data?.items.length && <div className="table-wrap"><table><thead><tr><th>时间</th><th>事件</th><th>操作者</th></tr></thead><tbody>
      {query.data.items.map((item) => <tr key={item.id}><td>{formatDate(item.at)}</td><td>{item.action}</td><td className="mono">{item.actor_id || item.actor_kind}</td></tr>)}
    </tbody></table></div>}
  </section>;
}

export function AdminUserDetail() {
  const { id = '' } = useParams();
  const userQuery = useQuery({ queryKey: ['admin', 'user', id], queryFn: () => portalApi.adminUser(id), enabled: !!id });
  const hosts = useQuery({ queryKey: ['admin', 'hosts'], queryFn: portalApi.adminHosts });
  const workspaces = useInfiniteQuery({
    queryKey: ['admin', 'workspaces', 'user', id],
    queryFn: ({ pageParam }) => portalApi.adminWorkspaces(pageParam),
    initialPageParam: undefined as string | undefined,
    getNextPageParam: (page) => page.next_cursor ?? undefined,
  });
  if (userQuery.isPending || hosts.isPending || workspaces.isPending) return <div className="loading-card" role="status">正在读取用户信息…</div>;
  if (userQuery.isError || hosts.isError || (workspaces.isError && !workspaces.data)) {
    const error = userQuery.isError ? userQuery.error : hosts.isError ? hosts.error : workspaces.error;
    return <div className="error-card" role="alert">{errorDetails(error).message}<button className="button button-secondary" type="button" onClick={() => { void userQuery.refetch(); void hosts.refetch(); void workspaces.refetch(); }}>重试</button></div>;
  }
  const user = userQuery.data;
  if (!user) return <div className="empty-card"><h2>找不到这个用户</h2><p>用户可能已被移除，或当前账号无权查看。</p><Link className="button button-secondary" to="/admin/users">返回用户列表</Link></div>;
  const ownedHosts = hosts.data.items.filter((host) => host.owner_id === user.id);
  const ownedWorkspaces = workspaces.data.pages.flatMap((page) => page.items).filter((workspace) => workspace.owner_id === user.id);

  return <section>
    <Link className="back-link" to="/admin/users">‹ 返回用户列表</Link>
    <div className="page-heading detail-heading"><div><p className="eyebrow">用户详情</p><h2>{user.name}</h2><p className="muted mono">{user.id}</p></div><div className="inline-actions"><Link className="button button-secondary" to={`/admin/users/${encodeURIComponent(user.id)}/records`}>记录与样本</Link><span className={`status status-${user.status === 'approved' ? 'ready' : 'disabled'}`}>{user.status === 'approved' ? '登录正常' : user.status === 'disabled' ? '已禁用登录' : user.status === 'pending' ? '等待审核' : '审核未通过'}</span></div></div>
    <div className="admin-detail-facts">
      <article><span>角色</span><strong>{user.role === 'super_admin' ? '超级管理员' : user.role === 'admin' ? '管理员' : '普通用户'}</strong></article>
      <article><span>云端电脑 / 上限</span><strong>{ownedHosts.length} / {user.quota.hosts} 台</strong></article>
      <article><span>CPU 使用 / 配额</span><strong>{user.usage && user.quota ? `${formatCpu(user.usage.cpu_milli)} / ${formatCpu(user.quota.cpu_milli)}` : '未提供'}</strong></article>
      <article><span>内存使用 / 配额</span><strong>{user.usage && user.quota ? `${formatMemory(user.usage.mem_mib)} / ${formatMemory(user.quota.mem_mib)}` : '未提供'}</strong></article>
      <article><span>创建时间</span><strong>{formatDate(user.created_at)}</strong></article>
    </div>
    <UserResourceEditor key={`${user.id}:${user.quota.hosts}:${user.default_host_resources.cpu_milli}:${user.default_host_resources.mem_mib}`} user={user} />
    <PersonalDataQuotaEditor ownerID={user.id} />
    <ModelAccountBudgetEditor ownerID={user.id} />
    <ModelUsageReconciliation ownerID={user.id} />
    <InstallationHistory ownerID={user.id} />
    <AuthorizationHistory ownerID={user.id} />
    <ModelLedgerHistory ownerID={user.id} />
    <UserEvents ownerID={user.id} />
    <section className="admin-detail-section">
      <div className="dashboard-section-heading"><div><h2>所属云端电脑</h2><p>这里显示该用户拥有的电脑及当前运行状态。</p></div><Link to="/admin/hosts">全部电脑</Link></div>
      {!ownedHosts.length ? <div className="empty-card compact"><p>该用户还没有云端电脑。</p></div> : <div className="table-wrap"><table><thead><tr><th>电脑</th><th>运行状态</th><th>规格</th><th>绑定工作区</th><th>创建时间</th></tr></thead><tbody>
        {ownedHosts.map((host) => <tr key={host.id}><td><Link className="admin-table-link" to={`/admin/hosts/${encodeURIComponent(host.id)}`}><strong>{host.name || host.id}</strong><small className="table-sub mono">{host.id}</small></Link></td><td><span className={`status status-${host.state}`}>{hostStateLabel[host.state] || host.state}</span></td><td>{formatCpu(host.resources.cpu_limit_milli)} / {formatMemory(host.resources.mem_limit_mib)}</td><td>{host.workspaces.length ? host.workspaces.map((binding) => binding.workspace.name || binding.workspace.id).join('、') : '未绑定'}</td><td>{formatDate(host.created_at)}</td></tr>)}
      </tbody></table></div>}
    </section>
    <section className="admin-detail-section">
      <div className="dashboard-section-heading"><div><h2>所属工作区</h2><p>查看此用户的工作区版本、用量和绑定状态。</p></div><Link to={`/admin/workspaces?owner=${encodeURIComponent(user.id)}`}>工作区列表</Link></div>
      {!ownedWorkspaces.length ? <div className="empty-card compact"><p>{workspaces.hasNextPage ? '已加载的记录中暂未找到该用户的工作区，可继续查找。' : '该用户还没有工作区。'}</p></div> : <div className="table-wrap"><table><thead><tr><th>工作区</th><th>状态</th><th>用量</th><th>版本</th><th>绑定电脑</th><th>最近更新</th></tr></thead><tbody>
        {ownedWorkspaces.map((workspace) => <tr key={workspace.id}><td><Link className="admin-table-link" to={`/admin/workspaces/${encodeURIComponent(workspace.id)}`}><strong>{workspace.name || workspace.id}</strong><small className="table-sub mono">{workspace.id}</small></Link></td><td><span className={`status status-${workspace.state}`}>{workspace.state === 'ready' ? '可用' : '删除中'}</span></td><td>{formatBytes(workspace.used_bytes)} / {formatBytes(workspace.quota_bytes)}</td><td>v{workspace.revision}</td><td>{workspace.bindings.length ? workspace.bindings.map((binding) => <Link className="admin-binding-link" to={`/admin/hosts/${encodeURIComponent(binding.host_id)}`} key={binding.host_id}>{binding.host_name || binding.host_id}<small>{binding.state === 'active' && binding.applied_revision === binding.desired_revision ? '已应用' : binding.state === 'preparing' ? '正在应用' : binding.state}</small></Link>) : '未绑定'}</td><td>{formatDate(workspace.updated_at)}</td></tr>)}
      </tbody></table></div>}
      {workspaces.isFetchNextPageError && <div className="error-card admin-pagination-error" role="alert">{errorDetails(workspaces.error).message}<button className="button button-secondary" type="button" onClick={() => void workspaces.fetchNextPage()}>重试</button></div>}
      {workspaces.hasNextPage && !workspaces.isFetchNextPageError && <button className="button button-secondary admin-load-more" type="button" disabled={workspaces.isFetchingNextPage} onClick={() => void workspaces.fetchNextPage()}>{workspaces.isFetchingNextPage ? '正在加载…' : ownedWorkspaces.length ? '加载更多工作区' : '继续查找工作区'}</button>}
    </section>
    <p className="notice notice-info admin-detail-note">禁用登录不会自动关闭已有云端电脑。</p>
  </section>;
}
