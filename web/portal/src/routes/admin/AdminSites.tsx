import { useState } from 'react';
import type { FormEvent } from 'react';
import { Link } from 'react-router';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { errorDetails, portalApi } from '../../app/api';
import { formatDate } from '../../app/format';

const PAGE_SIZE = 25;
type NudgePolicy = Awaited<ReturnType<typeof portalApi.adminNudges>>;
type NudgeDraft = Pick<NudgePolicy, 'enabled' | 'url' | 'text_zh' | 'text_en'>;
const emptyDraft: NudgeDraft = { enabled: false, url: '', text_zh: '', text_en: '' };
const draftFromPolicy = (policy: NudgePolicy): NudgeDraft => ({ enabled: policy.enabled, url: policy.url, text_zh: policy.text_zh, text_en: policy.text_en });

const siteStateLabels: Record<string, string> = { active: '运行中', draining: '正在停止', stopped: '已停止' };

function isConflict(error: unknown) {
  const value = error instanceof Error ? error as Error & { code?: string; status?: number } : undefined;
  return value?.code === 'conflict' || value?.status === 409;
}

function validateNudge(draft: NudgeDraft): string {
  const url = draft.url.trim();
  const textZH = draft.text_zh.trim();
  const textEN = draft.text_en.trim();
  if (url.length > 512) return 'GitHub 链接不能超过 512 个字符。';
  if ([...textZH].length > 240 || [...textEN].length > 240) return '中英文提示各不能超过 240 个字符。';
  if ([textZH, textEN].some((text) => /[\r\n\0]/u.test(text))) return '提示内容不能包含换行或空字符。';
  if (!draft.enabled && !url && !textZH && !textEN) return '';
  if (!url) return '请填写 GitHub 项目链接；若要清空提示，请先关闭提示并清空所有字段。';
  try {
    const parsed = new URL(url);
    const validGitHubURL = parsed.protocol === 'https:' && ['github.com', 'www.github.com'].includes(parsed.hostname.toLowerCase())
      && parsed.pathname.replaceAll('/', '').length > 0 && !parsed.username && !parsed.password && !parsed.search && !parsed.hash;
    if (!validGitHubURL) return '链接必须是指向 GitHub 项目的 HTTPS 地址，且不能带查询参数或片段。';
  } catch {
    return '请输入有效的 GitHub 项目链接。';
  }
  if (draft.enabled && (!textZH || !textEN)) return '启用提示时，中英文内容都必须填写。';
  return '';
}

export function AdminSites() {
  const client = useQueryClient();
  const [siteOffset, setSiteOffset] = useState(0);
  const siteQuery = useQuery({ queryKey: ['admin', 'sites', siteOffset], queryFn: () => portalApi.adminSites(PAGE_SIZE, siteOffset) });
  const nudgeKey = ['admin', 'nudges'];
  const nudgeQuery = useQuery({ queryKey: nudgeKey, queryFn: portalApi.adminNudges });
  const [draft, setDraft] = useState<NudgeDraft | null>(null);
  const [validationError, setValidationError] = useState('');
  const [saveError, setSaveError] = useState('');
  const [versionConflict, setVersionConflict] = useState(false);
  const [notice, setNotice] = useState('');
  const policy = nudgeQuery.data;
  const currentDraft = draft ?? (policy ? draftFromPolicy(policy) : emptyDraft);

  const update = useMutation({
    mutationFn: (body: Parameters<typeof portalApi.updateAdminNudges>[0]) => portalApi.updateAdminNudges(body),
    onSuccess: (saved) => {
      client.setQueryData(nudgeKey, saved);
      setDraft(draftFromPolicy(saved));
      setValidationError('');
      setSaveError('');
      setVersionConflict(false);
      setNotice('运营提示策略已保存。');
    },
    onError: (error) => {
      setSaveError(errorDetails(error).message);
      if (isConflict(error)) setVersionConflict(true);
    },
  });

  async function reloadPolicyAfterConflict() {
    const result = await nudgeQuery.refetch();
    if (!result.isError) {
      if (result.data) setDraft(draftFromPolicy(result.data));
      setVersionConflict(false);
      setSaveError('');
      setNotice('已重新加载最新的运营提示策略。');
    }
  }

  function submitNudge(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setSaveError('');
    setNotice('');
    if (!policy) return;
    const message = validateNudge(currentDraft);
    if (message) {
      setValidationError(message);
      return;
    }
    setValidationError('');
    update.mutate({
      expected_version: policy.version,
      enabled: currentDraft.enabled,
      url: currentDraft.url.trim(),
      text_zh: currentDraft.text_zh.trim(),
      text_en: currentDraft.text_en.trim(),
    });
  }

  const sites = siteQuery.data;
  const siteError = siteQuery.isError && !sites;
  const siteLabel = (state: string) => siteStateLabels[state] ?? state;

  return <section className="admin-content">
    <div className="page-heading"><div><p className="eyebrow">本地内容元数据</p><h1>站点与运营提示</h1><p className="muted">站点页只显示 Morde 本地站点及发布统计，不展示或发布站点内容。</p></div>
      <button type="button" className="button button-secondary" disabled={siteQuery.isFetching || nudgeQuery.isFetching} onClick={() => { void siteQuery.refetch(); void nudgeQuery.refetch(); }}>{siteQuery.isFetching || nudgeQuery.isFetching ? '正在刷新…' : '刷新数据'}</button>
    </div>

    <section className="admin-detail-section" aria-labelledby="admin-sites-heading">
      <h2 id="admin-sites-heading">本地站点统计</h2>
      {siteQuery.isPending ? <div className="loading-card" role="status">正在加载站点统计…</div>
        : siteError ? <div className="error-card" role="alert">{errorDetails(siteQuery.error).message}<button type="button" className="button button-secondary" onClick={() => void siteQuery.refetch()}>重试</button></div>
          : sites && <>
            <div className="admin-detail-facts">
              <article><span>站点总数</span><strong>{new Intl.NumberFormat('zh-CN').format(sites.summary.sites)}</strong></article>
              <article><span>运行中站点</span><strong>{new Intl.NumberFormat('zh-CN').format(sites.summary.active_sites)}</strong></article>
              <article><span>发布总数</span><strong>{new Intl.NumberFormat('zh-CN').format(sites.summary.releases)}</strong></article>
              <article><span>健康发布</span><strong>{new Intl.NumberFormat('zh-CN').format(sites.summary.healthy_releases)}</strong></article>
            </div>
            {!sites.items.length ? <div className="empty-card compact" role="status"><p>当前没有本地站点。</p></div>
              : <div className="table-wrap"><table>
                <thead><tr><th>站点 ID</th><th>所有者</th><th>云端电脑</th><th>站点状态</th><th>发布统计</th><th>最近发布</th></tr></thead>
                <tbody>{sites.items.map((site) => <tr key={site.id}>
                  <td><strong className="mono">{site.id}</strong></td>
                  <td><Link className="admin-table-link" to={`/admin/users/${encodeURIComponent(site.owner_id)}`}>{site.owner_name || site.owner_id}<small className="table-sub mono">{site.owner_id}</small></Link></td>
                  <td><Link className="admin-table-link mono" to={`/admin/hosts/${encodeURIComponent(site.host_id)}`}>{site.host_id}</Link></td>
                  <td><span className={`status status-${site.state}`}>{siteLabel(site.state)}</span></td>
                  <td>{site.healthy_release_count} / {site.release_count} 健康</td>
                  <td>{formatDate(site.latest_release_at)}</td>
                </tr>)}</tbody>
              </table></div>}
            <div className="inline-actions admin-pagination">
              <span className="muted">显示 {sites.items.length ? `${sites.offset + 1}–${sites.offset + sites.items.length}` : '0'} / {new Intl.NumberFormat('zh-CN').format(sites.total)} 个站点</span>
              <button type="button" className="button button-secondary" disabled={siteOffset === 0 || siteQuery.isFetching} onClick={() => setSiteOffset(Math.max(0, siteOffset - PAGE_SIZE))}>上一页站点</button>
              <button type="button" className="button button-secondary" disabled={siteOffset + sites.items.length >= sites.total || siteQuery.isFetching} onClick={() => setSiteOffset(siteOffset + PAGE_SIZE)}>下一页站点</button>
            </div>
            <p className="muted">只读取站点、发布数量和健康状态等本地元数据；没有站点内容预览或发布操作。</p>
          </>}
      {siteQuery.isError && sites && <div className="error-card admin-pagination-error" role="alert">{errorDetails(siteQuery.error).message}<button type="button" className="button button-secondary" onClick={() => void siteQuery.refetch()}>重试</button></div>}
    </section>

    <section className="admin-detail-section" aria-labelledby="admin-site-integrations-heading">
      <h2 id="admin-site-integrations-heading">外部数据源</h2>
      {sites ? <div className="admin-detail-facts">
        <article><span>Demo 展示数据</span><strong>{sites.integrations.demo === 'not_configured' ? '未配置' : sites.integrations.demo}</strong><p className="muted">没有配置 Demo 数据源。</p></article>
        <article><span>GitHub 统计</span><strong>{sites.integrations.github_stats === 'not_configured' ? '未配置' : sites.integrations.github_stats}</strong><p className="muted">没有连接 GitHub 统计。</p></article>
        <article><span>访问流量</span><strong>{sites.integrations.traffic === 'not_collected' ? '未采集' : sites.integrations.traffic}</strong><p className="muted">当前不采集站点访问流量。</p></article>
      </div> : <div className="empty-card compact"><p>站点数据源状态暂不可用；未验证的集成不会显示为已配置。</p></div>}
    </section>

    <section className="admin-detail-section" aria-labelledby="admin-nudge-heading">
      <h2 id="admin-nudge-heading">运营提示</h2>
      <p className="muted">管理用户端显示的项目提示。默认关闭；链接仅支持 HTTPS GitHub 项目地址，不会访问外部集成。</p>
      {versionConflict && <div className="error-card" role="alert">运营提示版本已变化。重新加载最新版本后再编辑。<button type="button" className="button button-secondary" onClick={() => void reloadPolicyAfterConflict()}>重新加载</button></div>}
      {notice && <p className="notice notice-info" role="status">{notice}</p>}
      {nudgeQuery.isPending ? <div className="loading-card" role="status">正在加载运营提示策略…</div>
        : nudgeQuery.isError && !policy ? <div className="error-card" role="alert">{errorDetails(nudgeQuery.error).message}<button type="button" className="button button-secondary" onClick={() => void nudgeQuery.refetch()}>重试</button></div>
          : policy && <>
            <p className="muted">当前状态：{policy.enabled ? '已启用' : '已关闭'} · 版本 v{policy.version} · 最近更新 {formatDate(policy.updated_at)}</p>
            <form className="form-card connection-form" onSubmit={submitNudge}>
              <label><input type="checkbox" checked={currentDraft.enabled} onChange={(event) => setDraft({ ...currentDraft, enabled: event.target.checked })} /> 启用运营提示</label>
              <div className="connection-form-grid">
                <div className="connection-form-wide"><label htmlFor="admin-nudge-url">GitHub 项目链接</label><input id="admin-nudge-url" type="url" maxLength={512} value={currentDraft.url} onChange={(event) => setDraft({ ...currentDraft, url: event.target.value })} placeholder="https://github.com/owner/project" /></div>
                <div><label htmlFor="admin-nudge-text-zh">中文提示</label><textarea id="admin-nudge-text-zh" maxLength={240} rows={3} value={currentDraft.text_zh} onChange={(event) => setDraft({ ...currentDraft, text_zh: event.target.value })} /></div>
                <div><label htmlFor="admin-nudge-text-en">English message</label><textarea id="admin-nudge-text-en" maxLength={240} rows={3} value={currentDraft.text_en} onChange={(event) => setDraft({ ...currentDraft, text_en: event.target.value })} /></div>
              </div>
              {validationError && <p className="form-error" role="alert">{validationError}</p>}
              {saveError && <p className="form-error" role="alert">{saveError}</p>}
              <div className="inline-actions"><button type="submit" className="button button-primary" disabled={update.isPending || versionConflict || nudgeQuery.isFetching}>{update.isPending ? '保存中…' : '保存运营提示'}</button><span className="muted">保存时使用当前版本 v{policy.version}。</span></div>
            </form>
          </>}
    </section>
  </section>;
}
