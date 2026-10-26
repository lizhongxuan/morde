import { Link, NavLink, Outlet, useLocation, useNavigate } from 'react-router';
import { errorDetails } from '../../app/api';
import { useAuth } from '../../app/auth';
import { useToast } from '../../app/toast';

const links = [
  { to: '/admin', label: '总览', icon: '▦', end: true },
  { to: '/admin/users', label: '用户', icon: '♧' },
  { to: '/admin/hosts', label: '云端电脑', icon: '▣' },
  { to: '/admin/workspaces', label: '工作区', icon: '▱' },
  { to: '/admin/audit', label: '审计', icon: '≡' },
];

const titles: Record<string, string> = {
  '/admin': '总览', '/admin/users': '用户', '/admin/hosts': '云端电脑',
  '/admin/workspaces': '工作区', '/admin/audit': '审计',
};

export function AdminLayout() {
  const { user, signOut } = useAuth();
  const { notify } = useToast();
  const navigate = useNavigate();
  const location = useLocation();
  const title = titles[location.pathname] ?? (location.pathname.startsWith('/admin/users/') ? '用户详情' : location.pathname.startsWith('/admin/hosts/') ? '电脑详情' : '管理后台');
  const logout = async () => {
    try { await signOut(); navigate('/admin/login', { replace: true }); }
    catch (error) { notify(`退出登录失败：${errorDetails(error).message}`, 'error'); }
  };

  return <div className="morde-shell morde-admin-shell">
    <a className="skip-link" href="#admin-content">跳到主要内容</a>
    <aside className="morde-sidebar morde-admin-sidebar" aria-label="管理后台导航">
      <Link to="/admin/" className="morde-brand"><span className="morde-brand-mark">M</span><span><strong>Morde</strong><small>管理后台</small></span></Link>
      <div className="morde-space-chip"><span aria-hidden="true">◈</span><span>管理控制台</span></div>
      <nav className="morde-navigation" aria-label="管理导航">
        <span className="morde-nav-heading">日常管理</span>
        {links.map(({ to, label, icon, end }) => <NavLink className="morde-nav-link" to={to} end={end} key={to}><span className="morde-nav-icon" aria-hidden="true">{icon}</span><span>{label}</span></NavLink>)}
      </nav>
      <div className="morde-sidebar-bottom">
        <a className="morde-nav-link" href="/"><span className="morde-nav-icon" aria-hidden="true">↗</span><span>PC 用户端</span></a>
        <div className="morde-profile"><span className="morde-avatar" aria-hidden="true">{user?.name.slice(0, 1) || 'M'}</span><span><strong>{user?.name}</strong><small>管理员</small></span><button className="button button-quiet button-small" type="button" onClick={() => void logout()}>退出</button></div>
      </div>
    </aside>
    <main className="morde-main">
      <header className="morde-topbar"><div className="morde-crumb"><span>管理后台</span><span aria-hidden="true">›</span><strong>{title}</strong></div><div className="morde-top-actions"><span className="morde-admin-role">管理员视图</span><span className="morde-top-user">{user?.name}</span></div></header>
      <div id="admin-content" className="morde-page admin-page" tabIndex={-1}><Outlet /></div>
    </main>
  </div>;
}
