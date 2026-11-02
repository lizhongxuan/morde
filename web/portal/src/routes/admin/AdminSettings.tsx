import { Link } from 'react-router';
import { useQuery } from '@tanstack/react-query';
import { errorDetails, portalApi } from '../../app/api';
import { formatBytes } from '../../app/format';

const formatPolicyState = (enabled: boolean) => enabled ? '已开启' : '已关闭';

export function AdminSettings() {
  const query = useQuery({ queryKey: ['admin', 'settings'], queryFn: portalApi.adminSettings });
  const data = query.data;

  return <section className="admin-content">
    <div className="page-heading"><div><p className="eyebrow">平台策略</p><h1>设置与默认额度</h1><p className="muted">查看账号审核、自动奖励和个人数据默认额度。当前页面展示平台固定规则。</p></div>
      <button type="button" className="button button-secondary" disabled={query.isFetching} onClick={() => void query.refetch()}>{query.isFetching ? '正在刷新…' : '刷新数据'}</button>
    </div>

    {query.isPending ? <div className="loading-card" role="status">正在加载平台设置…</div>
      : query.isError ? <div className="error-card" role="alert">{errorDetails(query.error).message}<button type="button" className="button button-secondary" onClick={() => void query.refetch()}>重试</button></div>
        : data && <>
          <section className="admin-detail-section" aria-labelledby="admin-settings-policy-heading">
            <h2 id="admin-settings-policy-heading">账号与奖励规则</h2>
            <div className="admin-detail-facts">
              <article><span>新账号审核</span><strong>{data.registration_review_required ? '需要审核' : '无需审核'}</strong><p className="muted">邀请关系不会绕过账号审核。</p></article>
              <article><span>新用户自动模型预算</span><strong>{formatPolicyState(data.automatic_new_user_model_budget)}</strong><p className="muted">模型预算由管理员在用户详情中按账号设置。</p></article>
              <article><span>自动邀请奖励</span><strong>{formatPolicyState(data.automatic_invitation_reward)}</strong><p className="muted">邀请不会自动发放奖励。</p></article>
              <article><span>自动贡献奖励</span><strong>{formatPolicyState(data.automatic_contribution_reward)}</strong><p className="muted">贡献不会自动发放奖励。</p></article>
            </div>
            <p><Link className="button button-secondary button-small" to="/admin/users">查看用户与按人设置模型预算</Link></p>
          </section>

          <section className="admin-detail-section" aria-labelledby="admin-settings-quota-heading">
            <h2 id="admin-settings-quota-heading">个人数据默认额度</h2>
            <p className="muted">以下默认值与普通聊天、工作区等其他额度分开计算；账号的实际生效额度可能由管理员单独调整。</p>
            <div className="admin-detail-facts">
              <article><span>每账号默认总量</span><strong>{data.default_personal_data_quota_bytes === 5 * 1024 ** 3 ? '5 GiB' : formatBytes(data.default_personal_data_quota_bytes)}</strong><small>{formatBytes(data.default_personal_data_quota_bytes)}</small></article>
              <article><span>单附件默认上限</span><strong>{data.default_personal_attachment_bytes === 300 * 1024 ** 2 ? '300 MiB' : formatBytes(data.default_personal_attachment_bytes)}</strong><small>{formatBytes(data.default_personal_attachment_bytes)}</small></article>
            </div>
            <p className="muted">个人数据的已用量、当前生效上限与单账号调整入口位于用户详情页。</p>
          </section>
        </>}
  </section>;
}
