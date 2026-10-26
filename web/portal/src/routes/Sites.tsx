import { useRef, useState } from 'react';
import type { FormEvent } from 'react';
import { useInfiniteQuery, useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { errorDetails, newIdempotencyKey, portalApi } from '../app/api';
import { useAuth } from '../app/auth';
import { formatBytes, formatDate } from '../app/format';

type Site = Awaited<ReturnType<typeof portalApi.sites>>['items'][number];
type SiteRelease = Awaited<ReturnType<typeof portalApi.siteReleases>>['items'][number];
type Host = Awaited<ReturnType<typeof portalApi.hosts>>['items'][number];
type Workspace = Awaited<ReturnType<typeof portalApi.workspaces>>['items'][number];
type SiteTarget = { host: Host; workspace: Workspace; key: string };

const siteStateLabel: Record<Site['state'], string> = { active: '可管理', draining: '停止处理中', stopped: '已停止' };
const siteStateClass: Record<Site['state'], string> = { active: 'status-running', draining: 'status-stopping', stopped: 'status-stopped' };
const releaseStateLabel: Record<SiteRelease['state'], string> = {
  candidate: '候选快照', healthy: '健康检查通过', failed: '失败', cancelled: '已取消',
};
const releaseStateClass: Record<SiteRelease['state'], string> = {
  candidate: 'status-queued', healthy: 'status-running', failed: 'status-failed', cancelled: 'status-cancelled',
};

export function Sites() {
  const client = useQueryClient();
  const { user } = useAuth();
  const [selectedSiteID, setSelectedSiteID] = useState<string | null>(null);
  const [selectedTargetKey, setSelectedTargetKey] = useState('');
  const [servicePort, setServicePort] = useState('3000');
  const [createdSiteMessage, setCreatedSiteMessage] = useState('');
  const createAttempt = useRef<{ targetKey: string; key: string } | null>(null);
  const candidateAttempt = useRef<{ requestKey: string; key: string } | null>(null);
  const sites = useInfiniteQuery({
    queryKey: ['sites'],
    queryFn: ({ pageParam }) => portalApi.sites(pageParam),
    initialPageParam: undefined as string | undefined,
    getNextPageParam: (page) => page.next_cursor ?? undefined,
  });
  const hosts = useQuery({ queryKey: ['hosts'], queryFn: portalApi.hosts });
  const workspaces = useQuery({ queryKey: ['workspaces'], queryFn: portalApi.workspaces });
  const siteItems = sites.data?.pages.flatMap((page) => page.items) ?? [];
  const selectedSite = siteItems.find((site) => site.id === selectedSiteID) ?? null;
  const releases = useInfiniteQuery({
    queryKey: ['site-releases', selectedSiteID],
    queryFn: ({ pageParam }) => portalApi.siteReleases(selectedSiteID!, pageParam),
    initialPageParam: undefined as string | undefined,
    getNextPageParam: (page) => page.next_cursor ?? undefined,
    enabled: Boolean(selectedSiteID),
  });
  const releaseItems = releases.data?.pages.flatMap((page) => page.items) ?? [];
  const workspaceByID = new Map((workspaces.data?.items ?? []).map((workspace) => [workspace.id, workspace]));
  const targets: SiteTarget[] = hosts.data?.items.flatMap((host) => host.owner_id === user?.id && host.state === 'running'
    ? host.workspaces.flatMap((binding) => {
      const workspace = workspaceByID.get(binding.workspace.id);
      return binding.state === 'active' && binding.applied_revision === binding.desired_revision && workspace?.state === 'ready'
        ? [{ host, workspace, key: `${host.id}/${workspace.id}` }]
        : [];
    })
    : []) ?? [];
  const selectedTargetKeyValue = selectedTargetKey || (targets.length === 1 ? targets[0]?.key ?? '' : '');
  const selectedTarget = targets.find((target) => target.key === selectedTargetKeyValue);
  const selectedWorkspace = selectedSite ? workspaceByID.get(selectedSite.workspace_id) : undefined;
  const selectedSiteCanCreateCandidate = Boolean(selectedSite?.state === 'active' && hosts.data?.items.some((host) =>
    host.id === selectedSite.host_id && host.owner_id === user?.id && host.state === 'running'
      && host.workspaces.some((binding) => binding.workspace.id === selectedSite.workspace_id && binding.state === 'active')));

  const createSite = useMutation({
    mutationFn: ({ target, idempotencyKey }: { target: SiteTarget; idempotencyKey: string }) => portalApi.createSite({
      host_id: target.host.id,
      workspace_id: target.workspace.id,
    }, idempotencyKey),
    onSuccess: async (site) => {
      createAttempt.current = null;
      setSelectedSiteID(site.id);
      setCreatedSiteMessage(`站点 ${site.id} 已创建。`);
      await Promise.all([
        client.invalidateQueries({ queryKey: ['sites'] }),
        client.invalidateQueries({ queryKey: ['hosts'] }),
        client.invalidateQueries({ queryKey: ['workspaces'] }),
      ]);
    },
  });
  const createCandidate = useMutation({
    mutationFn: ({ siteID, workspaceRevision, port, idempotencyKey }: { siteID: string; workspaceRevision: number; port: number; idempotencyKey: string }) =>
      portalApi.createSiteCandidate(siteID, { workspace_revision: workspaceRevision, service_port: port }, idempotencyKey),
    onSuccess: async (_release, { siteID }) => {
      candidateAttempt.current = null;
      await client.invalidateQueries({ queryKey: ['site-releases', siteID] });
    },
  });
  const materialize = useMutation({
    mutationFn: ({ siteID, releaseID }: { siteID: string; releaseID: string }) => portalApi.materializeSiteRelease(siteID, releaseID),
    onSuccess: async (_release, { siteID }) => { await client.invalidateQueries({ queryKey: ['site-releases', siteID] }); },
  });

  const submitSite = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (!selectedTarget || createSite.isPending) return;
    if (!createAttempt.current || createAttempt.current.targetKey !== selectedTarget.key) {
      createAttempt.current = { targetKey: selectedTarget.key, key: newIdempotencyKey() };
    }
    setCreatedSiteMessage('');
    createSite.mutate({ target: selectedTarget, idempotencyKey: createAttempt.current.key });
  };
  const submitCandidate = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const port = Number(servicePort);
    if (!selectedSite || !selectedWorkspace || selectedWorkspace.state !== 'ready' || selectedWorkspace.revision < 1
      || !Number.isInteger(port) || port < 1024 || port > 65535 || createCandidate.isPending) return;
    const requestKey = `${selectedSite.id}/${selectedWorkspace.revision}/${port}`;
    if (!candidateAttempt.current || candidateAttempt.current.requestKey !== requestKey) {
      candidateAttempt.current = { requestKey, key: newIdempotencyKey() };
    }
    createCandidate.mutate({ siteID: selectedSite.id, workspaceRevision: selectedWorkspace.revision, port, idempotencyKey: candidateAttempt.current.key });
  };

  return <section>
    <div className="page-heading"><div><p className="eyebrow">资源管理</p><h1>站点</h1><p className="muted">从已绑定工作区的已保存版本创建私有快照。</p></div></div>

    <div className="notice notice-warning" role="note" aria-labelledby="site-release-boundary-title">
      <h2 id="site-release-boundary-title">私有快照，不执行、不发布、不生成公开 URL</h2>
      <p>候选版本只会把指定工作区版本保存到私有存储。网站构建、运行和公开访问功能尚未开放。</p>
    </div>

    <section aria-labelledby="site-create-title">
      <h2 id="site-create-title">创建私有站点</h2>
      <p className="muted">只能选择你拥有且正在运行的主机，以及已完整应用到该主机的工作区。</p>
      {hosts.isError || workspaces.isError ? <div className="error-card" role="alert">
        无法读取主机或工作区绑定状态：{errorDetails(hosts.error ?? workspaces.error).message}
        <button type="button" className="button button-secondary" onClick={() => { void hosts.refetch(); void workspaces.refetch(); }}>重试</button>
      </div> : hosts.isPending || workspaces.isPending ? <div className="loading-card" role="status">正在检查主机和工作区绑定…</div>
        : <form className="form-card form-stack" aria-label="创建私有站点" onSubmit={submitSite}>
          {targets.length ? <>
            <label htmlFor="site-target">运行中的主机和已应用工作区</label>
            <select id="site-target" value={selectedTargetKeyValue} onChange={(event) => { setSelectedTargetKey(event.target.value); createSite.reset(); setCreatedSiteMessage(''); }}>
              <option value="">选择主机和工作区</option>
              {targets.map(({ host, workspace, key }) => <option key={key} value={key}>{host.name || host.id} · {workspace.name || workspace.id} · v{workspace.revision}</option>)}
            </select>
            {createSite.isError && <p className="form-error" role="alert">创建站点失败：{errorDetails(createSite.error).message}</p>}
            {createdSiteMessage && <p className="notice notice-success" role="status">{createdSiteMessage}</p>}
            <div className="form-actions"><button type="submit" className="button button-primary" disabled={!selectedTarget || createSite.isPending}>{createSite.isPending ? '正在创建…' : createSite.isError ? '重试创建' : '创建站点'}</button></div>
          </> : <p className="muted">暂无可用绑定。请先启动一台你拥有的主机，并将一个工作区完整应用到该主机。</p>}
        </form>}
    </section>

    <section aria-labelledby="sites-list-title">
      <h2 id="sites-list-title">我的站点</h2>
      {sites.isPending ? <div className="loading-card" role="status">正在加载站点…</div>
        : sites.isError && !sites.data ? <div className="error-card" role="alert">无法读取站点：{errorDetails(sites.error).message}<button type="button" className="button button-secondary" onClick={() => void sites.refetch()}>重试</button></div>
          : !siteItems.length ? <div className="empty-card"><h3>还没有站点</h3><p>选择符合条件的主机和工作区后，可以创建一个私有站点。</p></div>
            : <>
              <div className="table-wrap"><table>
                <thead><tr><th>站点 ID</th><th>主机</th><th>工作区</th><th>状态</th><th>创建时间</th><th>操作</th></tr></thead>
                <tbody>{siteItems.map((site) => <tr key={site.id}>
                  <td><strong className="mono">{site.id}</strong></td>
                  <td className="mono">{site.host_id}</td>
                  <td className="mono">{site.workspace_id}</td>
                  <td><span className={`status ${siteStateClass[site.state]}`}>{siteStateLabel[site.state]}</span></td>
                  <td><time dateTime={site.created_at}>{formatDate(site.created_at)}</time></td>
                  <td><button type="button" className="button button-secondary button-small" aria-pressed={selectedSiteID === site.id} onClick={() => { setSelectedSiteID(site.id); createCandidate.reset(); materialize.reset(); }}>{selectedSiteID === site.id ? '正在查看' : '查看版本'}</button></td>
                </tr>)}</tbody>
              </table></div>
              {!selectedSite && <p className="muted">选择一个站点查看或保存它的私有候选快照。</p>}
              {sites.isFetchNextPageError && <div className="error-card" role="alert">加载更多站点失败：{errorDetails(sites.error).message}<button type="button" className="button button-secondary" onClick={() => void sites.fetchNextPage()}>重试</button></div>}
              {sites.hasNextPage && <button type="button" className="button button-secondary" disabled={sites.isFetchingNextPage} onClick={() => void sites.fetchNextPage()}>{sites.isFetchingNextPage ? '正在加载站点…' : '加载更多站点'}</button>}
            </>}
    </section>

    {selectedSite && <section aria-labelledby="site-releases-title">
      <div className="connection-section-heading"><div><h2 id="site-releases-title">版本记录 · <span className="mono">{selectedSite.id}</span></h2><p className="muted">候选快照固定到工作区版本；保存后仍为私有数据，不会运行或公开。</p></div></div>
      {selectedWorkspace ? <form className="form-card form-stack" aria-label="创建候选快照" onSubmit={submitCandidate}>
        <p>当前工作区版本：<strong>v{selectedWorkspace.revision}</strong></p>
        {selectedWorkspace.revision < 1 && <p className="muted">工作区还没有已保存版本。先同步文件，再创建候选快照。</p>}
        {selectedWorkspace.state !== 'ready' && <p className="muted">工作区当前不可用，暂不能创建候选快照。</p>}
        {!selectedSiteCanCreateCandidate && <p className="muted">主机或工作区绑定当前不可用，暂不能创建候选快照。</p>}
        <label htmlFor="site-service-port">服务端口（仅记录，不会启动服务）</label>
        <input id="site-service-port" type="number" min={1024} max={65535} step={1} value={servicePort} onChange={(event) => { setServicePort(event.target.value); createCandidate.reset(); }} />
        {createCandidate.isError && <p className="form-error" role="alert">创建候选快照失败：{errorDetails(createCandidate.error).message}</p>}
        {materialize.isError && <p className="form-error" role="alert">保存私有快照失败：{errorDetails(materialize.error).message}</p>}
        <div className="form-actions"><button type="submit" className="button button-primary" disabled={!selectedSiteCanCreateCandidate || selectedWorkspace.state !== 'ready' || selectedWorkspace.revision < 1 || !Number.isInteger(Number(servicePort)) || Number(servicePort) < 1024 || Number(servicePort) > 65535 || releases.isPending || (releases.isError && !releases.data) || releaseItems.some((release) => release.state === 'candidate') || createCandidate.isPending}>
          {createCandidate.isPending ? '正在创建…' : releaseItems.some((release) => release.state === 'candidate') ? '已有候选快照' : createCandidate.isError ? '重试创建候选快照' : '创建候选快照'}
        </button></div>
      </form> : <p className="muted">工作区信息暂不可用，无法创建新的候选快照。</p>}
      {releases.isPending ? <div className="loading-card" role="status">正在加载版本记录…</div>
        : releases.isError && !releases.data ? <div className="error-card" role="alert">无法读取版本记录：{errorDetails(releases.error).message}<button type="button" className="button button-secondary" onClick={() => void releases.refetch()}>重试</button></div>
          : !releaseItems.length ? <div className="empty-card"><h3>还没有版本快照</h3><p>创建候选快照后，可以将指定工作区版本保存到私有存储。</p></div>
            : <>
              <div className="table-wrap"><table>
                <thead><tr><th>状态</th><th>工作区版本</th><th>快照大小</th><th>创建时间</th><th>保存时间</th><th>操作</th></tr></thead>
                <tbody>{releaseItems.map((release) => <tr key={release.id}>
                  <td><span className={`status ${releaseStateClass[release.state]}`}>{releaseStateLabel[release.state]}</span><small className="table-sub mono">{release.id}</small></td>
                  <td>v{release.workspace_revision}</td>
                  <td>{formatBytes(release.artifact_bytes)} <small className="muted">· {release.artifact_file_count} 个文件</small></td>
                  <td><time dateTime={release.created_at}>{formatDate(release.created_at)}</time></td>
                  <td>{release.materialized_at ? <time dateTime={release.materialized_at}>{formatDate(release.materialized_at)}</time> : '尚未保存'}</td>
                  <td>{release.state === 'candidate' && !release.materialized_at
                    ? <button type="button" className="button button-secondary button-small" aria-label={`保存私有快照 ${release.id}`} disabled={materialize.isPending} onClick={() => materialize.mutate({ siteID: selectedSite.id, releaseID: release.id })}>{materialize.isPending && materialize.variables?.releaseID === release.id ? '正在保存…' : '保存私有快照'}</button>
                    : release.materialized_at ? '私有快照已保存' : '—'}</td>
                </tr>)}</tbody>
              </table></div>
              {releases.isFetchNextPageError && <div className="error-card" role="alert">加载更多版本失败：{errorDetails(releases.error).message}<button type="button" className="button button-secondary" onClick={() => void releases.fetchNextPage()}>重试</button></div>}
              {releases.hasNextPage && <button type="button" className="button button-secondary" disabled={releases.isFetchingNextPage} onClick={() => void releases.fetchNextPage()}>{releases.isFetchingNextPage ? '正在加载版本…' : '加载更多版本'}</button>}
            </>}
    </section>}
  </section>;
}
