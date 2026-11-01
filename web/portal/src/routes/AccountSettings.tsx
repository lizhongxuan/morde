import { Link } from 'react-router';
import { useAuth } from '../app/auth';

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
  </section>;
}
