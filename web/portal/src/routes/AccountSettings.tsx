import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Link } from 'react-router';
import { errorDetails, portalApi } from '../app/api';
import { useAuth } from '../app/auth';
import { useToast } from '../app/toast';

const consentLabels = { full_record: '完整会话记录', training_contribution: '训练样本贡献' } as const;

function RecordConsentSettings() {
  const client = useQueryClient();
  const { notify } = useToast();
  const query = useQuery({ queryKey: ['me', 'record-consents'], queryFn: portalApi.recordConsents });
  const update = useMutation({
    mutationFn: portalApi.setRecordConsent,
    onSuccess: async () => {
      notify('记录同意设置已保存。', 'success');
      await client.invalidateQueries({ queryKey: ['me', 'record-consents'] });
    },
    onError: (error) => notify(errorDetails(error).message, 'error'),
  });

  return <section className="connection-section" aria-labelledby="record-consent-settings-title">
    <div className="connection-section-heading"><div><h2 id="record-consent-settings-title">记录与样本同意</h2><p className="muted">两类记录分别控制，默认关闭。关闭后会清理待处理内容，已保存的记录仍可由你申请删除。</p></div></div>
    {query.isPending ? <div className="loading-card" role="status">正在读取记录同意设置…</div>
      : query.isError ? <div className="error-card" role="alert">{errorDetails(query.error).message}<button type="button" className="button button-secondary" onClick={() => void query.refetch()}>重试</button></div>
        : <fieldset className="form-card form-stack" disabled={update.isPending}>
          <legend className="sr-only">选择记录保存方式</legend>
          {query.data.items.map((consent) => <label className="checkbox-row" key={consent.purpose}>
            <input type="checkbox" checked={consent.enabled} onChange={(event) => update.mutate({ purpose: consent.purpose, enabled: event.target.checked })} />
            允许保存{consentLabels[consent.purpose]}
          </label>)}
          {update.isPending && <p role="status" className="muted">正在保存…</p>}
        </fieldset>}
  </section>;
}

const roleLabels: Record<string, string> = {
  user: '普通用户', admin: '管理员', super_admin: '超级管理员',
};

export function AccountSettings() {
  const { user } = useAuth();

  return <section>
    <div className="page-heading">
      <div><p className="eyebrow">账号安全</p><h1>账户设置</h1><p className="muted">查看当前账号并管理登录安全。</p></div>
    </div>
    <section className="connection-section" aria-labelledby="account-info-title">
      <div className="connection-section-heading"><div><h2 id="account-info-title">当前账号</h2><p className="muted">账号权限由服务端管理。</p></div></div>
      <article className="model-connection-card">
        <div className="model-connection-main">
          <div className="model-connection-title"><h3>{user?.name || '当前账号'}</h3><span className="status status-ready">{user ? roleLabels[user.role] ?? '已登录' : '加载中'}</span></div>
          <p className="muted">修改密码后，现有登录会话会被撤销，需要在设备上重新登录。</p>
        </div>
        <Link className="button button-secondary" to="/settings/password">修改密码</Link>
      </article>
      <div className="connection-section-heading account-clients-link"><div><h2>已登录设备</h2><p className="muted">查看或撤销已登录的电脑和手机。</p></div><Link className="button button-secondary" to="/clients">管理客户端</Link></div>
    </section>
    <RecordConsentSettings />
  </section>;
}
