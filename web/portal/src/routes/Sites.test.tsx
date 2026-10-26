import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { portalApi } from '../app/api';
import { formatDate } from '../app/format';
import { Sites } from './Sites';

vi.mock('../app/auth', () => ({ useAuth: () => ({ user: { id: 'u1234567' } }) }));

const timestamp = '2026-10-22T08:00:00.000Z';
type Site = Awaited<ReturnType<typeof portalApi.sites>>['items'][number];
type SiteRelease = Awaited<ReturnType<typeof portalApi.siteReleases>>['items'][number];
type Host = Awaited<ReturnType<typeof portalApi.hosts>>['items'][number];
type Workspace = Awaited<ReturnType<typeof portalApi.workspaces>>['items'][number];

const workspace: Workspace = {
  id: 'w1234567', name: '博客项目', revision: 4, used_bytes: 2048, quota_bytes: 1024 * 1024, state: 'ready', created_at: timestamp, updated_at: timestamp,
};
const binding: Host['workspaces'][number] = {
  workspace: { id: workspace.id, name: workspace.name, revision: workspace.revision, used_bytes: workspace.used_bytes, state: workspace.state },
  directory: 'projects/blog', state: 'active', binding_version: 2, desired_revision: 4, applied_revision: 4, sync_state: 'healthy', sync_conflicts: [],
};
const host: Host = {
  id: 'h1234567', name: '开发电脑', owner_id: 'u1234567', template_id: 'std', isolation: 'container', state: 'running', state_reason: '',
  runtime_generation: 1, control_version: 1, control_state: 'ai', url: '', workspaces: [binding],
  resources: { cpu_milli: 500, mem_mib: 1024, cpu_limit_milli: 1000, mem_limit_mib: 2048 },
  expires_at: null, last_active_at: timestamp, created_at: timestamp,
};
const site: Site = {
  id: 'site_0123456789abcdef', host_id: host.id, workspace_id: workspace.id, state: 'active', created_at: timestamp, updated_at: timestamp,
};
const secondSite: Site = { ...site, id: 'site_fedcba9876543210' };
const release: SiteRelease = {
  id: 'rel_0123456789abcdef', site_id: site.id, workspace_id: site.workspace_id, workspace_revision: 4,
  manifest_sha256: 'a'.repeat(64), service_port: 3000, state: 'candidate', artifact_file_count: 3, artifact_bytes: 1536,
  materialized_at: timestamp, created_at: timestamp, updated_at: timestamp,
};
const candidate: SiteRelease = { ...release, materialized_at: undefined, artifact_file_count: 0, artifact_bytes: 0 };
const secondRelease: SiteRelease = { ...release, id: 'rel_fedcba9876543210', workspace_revision: 5 };

function renderPage(staleTime = 0) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime }, mutations: { retry: false } } });
  return { client, ...render(<QueryClientProvider client={client}><Sites /></QueryClientProvider>) };
}

describe('Sites', () => {
  beforeEach(() => {
    vi.spyOn(portalApi, 'hosts').mockResolvedValue({ items: [] });
    vi.spyOn(portalApi, 'workspaces').mockResolvedValue({ items: [] });
  });
  afterEach(() => { cleanup(); vi.restoreAllMocks(); });

  it('shows the empty state, no-binding guidance, and the private-only boundary', async () => {
    vi.spyOn(portalApi, 'sites').mockResolvedValue({ items: [], next_cursor: null });
    const siteReleases = vi.spyOn(portalApi, 'siteReleases');
    renderPage();

    expect(await screen.findByRole('heading', { name: '还没有站点' })).toBeInTheDocument();
    expect(screen.getByRole('heading', { name: '私有快照，不执行、不发布、不生成公开 URL' })).toBeInTheDocument();
    expect(screen.getByText('暂无可用绑定。请先启动一台你拥有的主机，并将一个工作区完整应用到该主机。')).toBeInTheDocument();
    expect(siteReleases).not.toHaveBeenCalled();
    expect(screen.queryByRole('button', { name: /发布|公开 URL|访问网站/ })).not.toBeInTheDocument();
  });

  it('creates a Site from an owned running Host and an active applied Workspace binding, then saves a private snapshot', async () => {
    const sites = vi.spyOn(portalApi, 'sites')
      .mockResolvedValueOnce({ items: [], next_cursor: null })
      .mockResolvedValue({ items: [site], next_cursor: null });
    vi.mocked(portalApi.hosts).mockResolvedValue({ items: [host] });
    vi.mocked(portalApi.workspaces).mockResolvedValue({ items: [workspace] });
    const createSite = vi.spyOn(portalApi, 'createSite').mockResolvedValue(site);
    const createCandidate = vi.spyOn(portalApi, 'createSiteCandidate').mockResolvedValue(candidate);
    const materialize = vi.spyOn(portalApi, 'materializeSiteRelease').mockResolvedValue(release);
    vi.spyOn(portalApi, 'siteReleases')
      .mockResolvedValueOnce({ items: [], next_cursor: null })
      .mockResolvedValueOnce({ items: [candidate], next_cursor: null })
      .mockResolvedValue({ items: [release], next_cursor: null });
    const user = userEvent.setup();
    renderPage();

    await user.click(await screen.findByRole('button', { name: '创建站点' }));
    await waitFor(() => expect(createSite).toHaveBeenCalledWith({ host_id: host.id, workspace_id: workspace.id }, expect.stringMatching(/^[A-Za-z0-9._~-]{8,128}$/)));
    expect(await screen.findByRole('heading', { name: new RegExp(`版本记录 · ${site.id}`) })).toBeInTheDocument();
    await user.click(await screen.findByRole('button', { name: '创建候选快照' }));
    await waitFor(() => expect(createCandidate).toHaveBeenCalledWith(site.id, { workspace_revision: workspace.revision, service_port: 3000 }, expect.stringMatching(/^[A-Za-z0-9._~-]{8,128}$/)));
    await user.click(await screen.findByRole('button', { name: `保存私有快照 ${candidate.id}` }));

    expect(await screen.findByText('私有快照已保存')).toBeInTheDocument();
    await waitFor(() => expect(materialize).toHaveBeenCalledWith(site.id, candidate.id));
    expect(sites).toHaveBeenCalled();
    expect(screen.getByRole('heading', { name: '私有快照，不执行、不发布、不生成公开 URL' })).toBeInTheDocument();
    expect(screen.queryByRole('link')).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /发布|访问网站|公开 URL/ })).not.toBeInTheDocument();
  });

  it('disables candidate creation when the Site Host is no longer running', async () => {
    vi.spyOn(portalApi, 'sites').mockResolvedValue({ items: [site], next_cursor: null });
    vi.mocked(portalApi.hosts).mockResolvedValue({ items: [{ ...host, state: 'stopped' }] });
    vi.mocked(portalApi.workspaces).mockResolvedValue({ items: [workspace] });
    const createCandidate = vi.spyOn(portalApi, 'createSiteCandidate');
    const user = userEvent.setup();
    renderPage();

    await user.click(await screen.findByRole('button', { name: '查看版本' }));

    expect(await screen.findByText('主机或工作区绑定当前不可用，暂不能创建候选快照。')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: '创建候选快照' })).toBeDisabled();
    expect(createCandidate).not.toHaveBeenCalled();
  });

  it('loads a selected Site release list and shows its status, source revision, size and times', async () => {
    vi.spyOn(portalApi, 'sites').mockResolvedValue({ items: [site], next_cursor: null });
    vi.mocked(portalApi.workspaces).mockResolvedValue({ items: [workspace] });
    const siteReleases = vi.spyOn(portalApi, 'siteReleases').mockResolvedValue({ items: [release], next_cursor: null });
    const user = userEvent.setup();
    renderPage();

    await user.click(await screen.findByRole('button', { name: '查看版本' }));

    await waitFor(() => expect(siteReleases).toHaveBeenCalledWith(site.id, undefined));
    expect(await screen.findByText('候选快照')).toBeInTheDocument();
    expect(screen.getAllByText('v4')).toHaveLength(2);
    expect(screen.getByText(/1,536 B/)).toBeInTheDocument();
    expect(screen.getByText(/3 个文件/)).toBeInTheDocument();
    expect(screen.getAllByText(formatDate(timestamp)).length).toBeGreaterThan(0);
    expect(screen.queryByRole('link')).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /发布|访问网站|公开 URL/ })).not.toBeInTheDocument();
  });

  it('invalidates the candidate list for the Site being mutated after selection changes', async () => {
    let aReads = 0;
    let resolveCandidate!: (value: SiteRelease) => void;
    const candidateResult = new Promise<SiteRelease>((resolve) => { resolveCandidate = resolve; });
    vi.spyOn(portalApi, 'sites').mockResolvedValue({ items: [site, secondSite], next_cursor: null });
    vi.mocked(portalApi.hosts).mockResolvedValue({ items: [host] });
    vi.mocked(portalApi.workspaces).mockResolvedValue({ items: [workspace] });
    const siteReleases = vi.spyOn(portalApi, 'siteReleases').mockImplementation(async (siteID) => {
      if (siteID === site.id) {
        aReads += 1;
        return { items: aReads === 1 ? [] : [candidate], next_cursor: null };
      }
      return { items: [], next_cursor: null };
    });
    const createCandidate = vi.spyOn(portalApi, 'createSiteCandidate').mockReturnValue(candidateResult);
    const user = userEvent.setup();
    const { client } = renderPage(Infinity);

    await user.click(within(await screen.findByRole('row', { name: new RegExp(site.id) })).getByRole('button', { name: '查看版本' }));
    await screen.findByText('还没有版本快照');
    await user.click(screen.getByRole('button', { name: '创建候选快照' }));
    await waitFor(() => expect(createCandidate).toHaveBeenCalledWith(site.id, expect.anything(), expect.any(String)));
    await user.click(within(await screen.findByRole('row', { name: new RegExp(secondSite.id) })).getByRole('button', { name: '查看版本' }));
    await waitFor(() => expect(siteReleases).toHaveBeenCalledTimes(2));

    resolveCandidate(candidate);
    await waitFor(() => expect(client.getQueryState(['site-releases', site.id])?.isInvalidated).toBe(true));
    expect(siteReleases).toHaveBeenCalledTimes(2);
    await user.click(within(await screen.findByRole('row', { name: new RegExp(site.id) })).getByRole('button', { name: '查看版本' }));

    expect(await screen.findByText('候选快照')).toBeInTheDocument();
    expect(siteReleases).toHaveBeenCalledTimes(3);
  });

  it('invalidates the materialized release list for the Site being mutated after selection changes', async () => {
    let aReads = 0;
    let resolveMaterialization!: (value: SiteRelease) => void;
    const materializationResult = new Promise<SiteRelease>((resolve) => { resolveMaterialization = resolve; });
    vi.spyOn(portalApi, 'sites').mockResolvedValue({ items: [site, secondSite], next_cursor: null });
    const siteReleases = vi.spyOn(portalApi, 'siteReleases').mockImplementation(async (siteID) => {
      if (siteID === site.id) {
        aReads += 1;
        return { items: [aReads === 1 ? candidate : release], next_cursor: null };
      }
      return { items: [], next_cursor: null };
    });
    const materialize = vi.spyOn(portalApi, 'materializeSiteRelease').mockReturnValue(materializationResult);
    const user = userEvent.setup();
    const { client } = renderPage(Infinity);

    await user.click(within(await screen.findByRole('row', { name: new RegExp(site.id) })).getByRole('button', { name: '查看版本' }));
    await user.click(await screen.findByRole('button', { name: `保存私有快照 ${candidate.id}` }));
    await waitFor(() => expect(materialize).toHaveBeenCalledWith(site.id, candidate.id));
    await user.click(within(await screen.findByRole('row', { name: new RegExp(secondSite.id) })).getByRole('button', { name: '查看版本' }));
    await waitFor(() => expect(siteReleases).toHaveBeenCalledTimes(2));

    resolveMaterialization(release);
    await waitFor(() => expect(client.getQueryState(['site-releases', site.id])?.isInvalidated).toBe(true));
    expect(siteReleases).toHaveBeenCalledTimes(2);
    await user.click(within(await screen.findByRole('row', { name: new RegExp(site.id) })).getByRole('button', { name: '查看版本' }));

    expect(await screen.findByText('私有快照已保存')).toBeInTheDocument();
    expect(siteReleases).toHaveBeenCalledTimes(3);
  });

  it('paginates Sites and retries a failed next release page', async () => {
    const sites = vi.spyOn(portalApi, 'sites')
      .mockResolvedValueOnce({ items: [site], next_cursor: 'site-next' })
      .mockResolvedValueOnce({ items: [secondSite], next_cursor: null });
    const siteReleases = vi.spyOn(portalApi, 'siteReleases')
      .mockResolvedValueOnce({ items: [release], next_cursor: 'release-next' })
      .mockRejectedValueOnce(new Error('临时网络错误'))
      .mockResolvedValueOnce({ items: [secondRelease], next_cursor: null });
    const user = userEvent.setup();
    renderPage();

    await user.click(await screen.findByRole('button', { name: '加载更多站点' }));
    expect(await screen.findByText(secondSite.id)).toBeInTheDocument();
    await waitFor(() => expect(sites).toHaveBeenNthCalledWith(2, 'site-next'));

    await user.click(within(screen.getByRole('row', { name: new RegExp(site.id) })).getByRole('button', { name: '查看版本' }));
    expect(await screen.findByText('候选快照')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: '加载更多版本' }));
    expect(await screen.findByText(/加载更多版本失败：临时网络错误/)).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: '重试' }));

    expect(await screen.findByText('v5')).toBeInTheDocument();
    await waitFor(() => expect(siteReleases).toHaveBeenNthCalledWith(3, site.id, 'release-next'));
  });
});
