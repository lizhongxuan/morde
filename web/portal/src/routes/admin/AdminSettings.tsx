import { useState } from 'react';
import type { FormEvent } from 'react';
import { Link } from 'react-router';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { errorDetails, portalApi } from '../../app/api';
import { formatBytes } from '../../app/format';

const formatPolicyState = (enabled: boolean) => enabled ? '已开启' : '已关闭';
type Draft = { tokenLimit: string; maxConcurrent: string };

export function AdminSettings() {
  const client = useQueryClient();
  const query = useQuery({ queryKey: ['admin', 'settings'], queryFn: portalApi.adminSettings });
  const [draft, setDraft] = useState<Draft | null>(null);
  const [saveError, setSaveError] = useState('');
  const [validationError, setValidationError] = useState('');
  const [conflicted, setConflicted] = useState(false);
  const [saved, setSaved] = useState(false);
  const data = query.data;
  const budget = data?.default_model_budget;
  const tokenLimit = draft?.tokenLimit ?? (budget?.configured ? String(budget.token_limit) : '');
  const maxConcurrent = draft?.maxConcurrent ?? (budget?.configured ? String(budget.max_concurrent) : '');
  const saveBudget = useMutation({
    mutationFn: portalApi.updateAdminModelBudgetDefault,
    onSuccess: (result) => {
      client.setQueryData(['admin', 'settings'], (current: typeof data) => current ? { ...current, default_model_budget: result } : current);
      setDraft(null);
      setSaveError('');
      setValidationError('');
      setConflicted(false);
      setSaved(true);
    },
    onError: (error) => {
      const details = errorDetails(error);
      setSaveError(details.code === 'conflict' ? '默认额度已被其他管理员修改。请刷新配置后确认最新值，再重新保存。' : details.message);
      if (details.code === 'conflict') setConflicted(true);
      setSaved(false);
    },
  });

  const refresh = () => {
    setDraft(null);
    setSaveError('');
    setValidationError('');
    setConflicted(false);
    setSaved(false);
    void query.refetch();
  };

  const submitBudget = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (!budget) return;
    const token = Number(tokenLimit);
    const concurrent = Number(maxConcurrent);
    if (!Number.isSafeInteger(token) || token < 1 || !Number.isInteger(concurrent) || concurrent < 1 || concurrent > 100) {
      setValidationError('Token 额度必须是正整数，并发上限需为 1–100 的整数。');
      return;
    }
    setValidationError('');
    setSaveError('');
    setSaved(false);
    saveBudget.mutate({ expected_version: budget.version, token_limit: token, max_concurrent: concurrent });
  };

  return <section className="admin-content">
    <div className="page-heading"><div><p className="eyebrow">平台策略</p><h1>设置与默认额度</h1><p className="muted">管理新账号审核、首次通过时应用的模型额度及个人数据默认额度。</p></div>
      <button type="button" className="button button-secondary" disabled={query.isFetching} onClick={refresh}>{query.isFetching ? '正在刷新…' : '刷新数据'}</button>
    </div>

    {query.isPending ? <div className="loading-card" role="status">正在加载平台设置…</div>
      : query.isError ? <div className="error-card" role="alert">{errorDetails(query.error).message}<button type="button" className="button button-secondary" onClick={refresh}>重试</button></div>
        : data && budget && <>
          <section className="admin-detail-section" aria-labelledby="admin-settings-policy-heading">
            <h2 id="admin-settings-policy-heading">账号与奖励规则</h2>
            <div className="admin-detail-facts">
              <article><span>新账号审核</span><strong>{data.registration_review_required ? '需要审核' : '无需审核'}</strong><p className="muted">邀请关系不会绕过账号审核。</p></article>
              <article><span>首次通过时应用默认模型额度</span><strong>{formatPolicyState(data.automatic_new_user_model_budget)}</strong><p className="muted">配置默认额度后，新账号首次审核通过时自动应用；现有账号额度不会被覆盖。</p></article>
              <article><span>自动邀请奖励</span><strong>{formatPolicyState(data.automatic_invitation_reward)}</strong><p className="muted">邀请不会自动发放奖励。</p></article>
              <article><span>自动贡献奖励</span><strong>{formatPolicyState(data.automatic_contribution_reward)}</strong><p className="muted">贡献不会自动发放奖励。</p></article>
            </div>
          </section>

          <section className="admin-detail-section" aria-labelledby="admin-settings-model-budget-heading">
            <h2 id="admin-settings-model-budget-heading">新用户默认模型额度</h2>
            {budget.configured
              ? <p className="notice notice-info" role="status">当前默认值：{budget.token_limit.toLocaleString()} tokens · 并发 {budget.max_concurrent} · 版本 v{budget.version}</p>
              : <p className="notice notice-warning" role="alert">尚未配置默认额度。首次批准新账号前必须设置正数 Token 额度和并发上限；配置前的批准请求会失败，账号保持待审核。</p>}
            <p className="muted">仅在账号首次从待审核或已拒绝变为已通过时创建模型额度，不影响已有账号，也不会因禁用后恢复而重置。</p>
            <form className="form-card form-stack" onSubmit={submitBudget}>
              <label htmlFor="admin-default-model-token-limit">Token 额度</label>
              <input id="admin-default-model-token-limit" type="number" min="1" max={Number.MAX_SAFE_INTEGER} step="1" required value={tokenLimit} onChange={(event) => { setDraft({ tokenLimit: event.target.value, maxConcurrent }); setSaved(false); }} />
              <label htmlFor="admin-default-model-max-concurrent">最大并发数</label>
              <input id="admin-default-model-max-concurrent" type="number" min="1" max="100" step="1" required value={maxConcurrent} onChange={(event) => { setDraft({ tokenLimit, maxConcurrent: event.target.value }); setSaved(false); }} />
              <div className="inline-actions">
                <button className="button button-primary" type="submit" disabled={saveBudget.isPending || conflicted}>{saveBudget.isPending ? '正在保存…' : '保存默认额度'}</button>
                {conflicted && <button className="button button-secondary" type="button" disabled={query.isFetching} onClick={refresh}>刷新配置并放弃当前草稿</button>}
              </div>
            </form>
            {validationError && <p className="notice notice-warning" role="alert">{validationError}</p>}
            {saveError && <p className="notice notice-warning" role="alert">{saveError}</p>}
            {saved && <p className="notice notice-success" role="status">默认模型额度已保存。</p>}
          </section>

          <section className="admin-detail-section" aria-labelledby="admin-settings-quota-heading">
            <h2 id="admin-settings-quota-heading">个人数据默认额度</h2>
            <p className="muted">以下默认值与普通聊天、工作区等其他额度分开计算；账号的实际生效额度可能由管理员单独调整。</p>
            <div className="admin-detail-facts">
              <article><span>每账号默认总量</span><strong>{data.default_personal_data_quota_bytes === 5 * 1024 ** 3 ? '5 GiB' : formatBytes(data.default_personal_data_quota_bytes)}</strong><small>{formatBytes(data.default_personal_data_quota_bytes)}</small></article>
              <article><span>单附件默认上限</span><strong>{data.default_personal_attachment_bytes === 300 * 1024 ** 2 ? '300 MiB' : formatBytes(data.default_personal_attachment_bytes)}</strong><small>{formatBytes(data.default_personal_attachment_bytes)}</small></article>
            </div>
            <p className="muted">个人数据的已用量、当前生效上限与单账号调整入口位于用户详情页。</p>
            <p><Link className="button button-secondary button-small" to="/admin/users">查看用户</Link></p>
          </section>
        </>}
  </section>;
}
