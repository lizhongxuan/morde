import { useState } from 'react';
import type { FormEvent } from 'react';
import { Link } from 'react-router';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { errorDetails, portalApi } from '../../app/api';
import { formatBytes } from '../../app/format';
import { FloatingError } from '../../components/FloatingError';

const formatPolicyState = (enabled: boolean) => enabled ? '已开启' : '已关闭';
type Draft = { tokenLimit: string; maxConcurrent: string };

export function AdminSettings() {
  const client = useQueryClient();
  const query = useQuery({ queryKey: ['admin', 'settings'], queryFn: portalApi.adminSettings });
  const modelsQuery = useQuery({ queryKey: ['admin', 'model-connections'], queryFn: portalApi.adminModelConnections });
  const [draft, setDraft] = useState<Draft | null>(null);
  const [modelDefaultDraft, setModelDefaultDraft] = useState<string | null>(null);
  const [saveError, setSaveError] = useState('');
  const [modelDefaultError, setModelDefaultError] = useState('');
  const [validationError, setValidationError] = useState('');
  const [conflicted, setConflicted] = useState(false);
  const [modelDefaultConflicted, setModelDefaultConflicted] = useState(false);
  const [saved, setSaved] = useState(false);
  const [modelDefaultSaved, setModelDefaultSaved] = useState(false);
  const data = query.data;
  const budget = data?.default_model_budget;
  const modelDefault = data?.default_model;
  const modelDefaultValue = modelDefaultDraft ?? modelDefault?.model_id ?? '';
  const modelCandidates = modelsQuery.data?.items.filter((model) => model.shared && model.checked_at
    && model.capabilities.text) ?? [];
  const selectedModelAvailable = modelCandidates.some((model) => model.id === modelDefaultValue);
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
  const saveModelDefault = useMutation({
    mutationFn: portalApi.updateAdminModelDefault,
    onSuccess: (result) => {
      client.setQueryData(['admin', 'settings'], (current: typeof data) => current ? { ...current, default_model: result } : current);
      setModelDefaultDraft(null);
      setModelDefaultError('');
      setModelDefaultConflicted(false);
      setModelDefaultSaved(true);
    },
    onError: (error) => {
      const details = errorDetails(error);
      setModelDefaultError(details.code === 'conflict' ? '默认模型已被其他管理员修改。请刷新配置后确认最新值，再重新保存。' : details.message);
      if (details.code === 'conflict') setModelDefaultConflicted(true);
      setModelDefaultSaved(false);
    },
  });

  const refresh = () => {
    setDraft(null);
    setModelDefaultDraft(null);
    setSaveError('');
    setModelDefaultError('');
    setValidationError('');
    setConflicted(false);
    setModelDefaultConflicted(false);
    setSaved(false);
    setModelDefaultSaved(false);
    void query.refetch();
    void modelsQuery.refetch();
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

  const submitModelDefault = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (!modelDefault || saveModelDefault.isPending || modelDefaultConflicted) return;
    if (modelDefaultValue && !selectedModelAvailable) {
      setModelDefaultError('只能选择已检查且支持文本的共享模型。');
      setModelDefaultSaved(false);
      return;
    }
    setModelDefaultError('');
    setModelDefaultSaved(false);
    saveModelDefault.mutate({ expected_version: modelDefault.version, model_id: modelDefaultValue });
  };

  return <section className="admin-content">
    {query.isPending ? <div className="loading-card" role="status">正在加载平台设置…</div>
      : query.isError ? <FloatingError className="error-card" role="alert">{errorDetails(query.error).message}<button type="button" className="button button-secondary" onClick={refresh}>重试</button></FloatingError>
        : data && budget && <>
          <section className="admin-detail-section" aria-labelledby="admin-settings-policy-heading">
            <div className="design-panel-header"><h2 id="admin-settings-policy-heading">账户规则</h2><button type="button" className="button button-secondary" disabled={query.isFetching || modelsQuery.isFetching} onClick={refresh}>{query.isFetching || modelsQuery.isFetching ? '正在刷新…' : '刷新数据'}</button></div>
            <div className="admin-detail-facts">
              <article><span>新账号审核</span><strong>{data.registration_review_required ? '需要审核' : '无需审核'}</strong><p className="muted">新账号的登录权限由审核状态决定。</p></article>
              <article><span>首次通过时应用默认模型额度</span><strong>{formatPolicyState(data.automatic_new_user_model_budget)}</strong><p className="muted">配置默认额度后，新账号首次审核通过时自动应用；现有账号额度不会被覆盖。</p></article>
              <article><span>自动贡献奖励</span><strong>{formatPolicyState(data.automatic_contribution_reward)}</strong><p className="muted">贡献不会自动发放奖励。</p></article>
            </div>
          </section>

          <section className="admin-detail-section" aria-labelledby="admin-settings-model-budget-heading">
            <h2 id="admin-settings-model-budget-heading">新用户默认模型额度</h2>
            {budget.configured
              ? <p className="notice notice-info" role="status">当前默认值：{budget.token_limit.toLocaleString()} tokens · 并发 {budget.max_concurrent} · 版本 v{budget.version}</p>
              : <FloatingError>尚未配置默认额度。新账号仍可通过审核并使用个人模型；使用平台模型前需要配置额度。</FloatingError>}
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
            {validationError && <FloatingError>{validationError}</FloatingError>}
            {saveError && <FloatingError className="error-inline" role="alert">{saveError}</FloatingError>}
            {saved && <p className="notice notice-success" role="status">默认模型额度已保存。</p>}
          </section>

          <section className="admin-detail-section" aria-labelledby="admin-settings-model-default-heading">
            <h2 id="admin-settings-model-default-heading">Cloud Agent 默认模型</h2>
            {modelDefault?.configured && modelDefaultValue && selectedModelAvailable
              ? <p className="notice notice-info" role="status">当前默认模型：{modelCandidates.find((model) => model.id === modelDefaultValue)?.name} · 版本 v{modelDefault.version}</p>
              : <FloatingError>{modelDefault?.model_id ? `默认模型 ${modelDefault.model_id} 尚不可用或未通过检查。` : '尚未配置默认模型。'}用户仍可选择已检查的个人模型或共享模型。</FloatingError>}
            <p className="muted">默认项只能指向管理员共享连接；用户也可以自行选择其他可用模型。</p>
            {modelsQuery.isError && <FloatingError className="error-inline" role="alert">{errorDetails(modelsQuery.error).message}<button type="button" className="button button-secondary button-small" onClick={() => void modelsQuery.refetch()}>重试读取模型</button></FloatingError>}
            <form className="form-card form-stack" onSubmit={submitModelDefault}>
              <label htmlFor="admin-default-model-connection">默认模型连接</label>
              <select id="admin-default-model-connection" value={modelDefaultValue} disabled={modelsQuery.isPending || modelDefaultConflicted} onChange={(event) => { setModelDefaultDraft(event.target.value); setModelDefaultError(''); setModelDefaultSaved(false); }}>
                <option value="">不设置默认模型</option>
                {modelDefaultValue && !selectedModelAvailable && <option value={modelDefaultValue} disabled>当前默认模型不可用 · {modelDefaultValue}</option>}
                {modelCandidates.map((model) => <option key={model.id} value={model.id}>{model.name} · {model.model_name}</option>)}
              </select>
              <small className="muted">可选项来自已检查且支持文本的平台模型；个人连接和未检查模型不会列出。</small>
              <div className="inline-actions">
                <button className="button button-primary" type="submit" disabled={saveModelDefault.isPending || modelDefaultConflicted || (Boolean(modelDefaultValue) && (!selectedModelAvailable || modelsQuery.isPending || modelsQuery.isError))}>{saveModelDefault.isPending ? '正在保存…' : modelDefaultValue ? '保存默认模型' : '清除默认模型'}</button>
                {modelDefaultConflicted && <button type="button" className="button button-secondary" disabled={query.isFetching} onClick={refresh}>刷新配置并放弃当前草稿</button>}
              </div>
            </form>
            {modelDefaultError && <FloatingError>{modelDefaultError}</FloatingError>}
            {modelDefaultSaved && <p className="notice notice-success" role="status">默认模型已保存。</p>}
          </section>

          <section className="admin-detail-section" aria-labelledby="admin-settings-quota-heading">
            <h2 id="admin-settings-quota-heading">个人数据默认额度</h2>
            <p className="muted">以下默认值与普通聊天、云端电脑文件等其他额度分开计算；账号的实际生效额度可能由管理员单独调整。</p>
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
