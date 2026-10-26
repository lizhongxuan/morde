import { Link, NavLink, Outlet, useLocation, useNavigate } from 'react-router';
import { useAuth } from '../app/auth';
import { errorDetails } from '../app/api';
import { useToast } from '../app/toast';

const primaryLinks = [
  { to: '/', label: '工作台', icon: '⌂', end: true },
  { to: '/assistant', label: 'AI 助手', icon: '✧' },
  { to: '/tasks', label: '任务与计划', icon: '✓' },
  { to: '/hosts', label: '云端电脑', icon: '▣' },
  { to: '/workspaces', label: '工作区', icon: '▱' },
];

const settingLinks = [
  { to: '/connections', label: '模型连接', icon: '↔' },
  { to: '/clients', label: '客户端', icon: '▤' },
  { to: '/apikeys', label: 'API Key', icon: '⌘' },
];

const titles: Record<string, string> = {
  '/': '工作台', '/assistant': 'AI 助手', '/tasks': '任务与计划', '/hosts': '云端电脑',
  '/hosts/new': '创建电脑', '/workspaces': '工作区', '/connections': '模型连接',
  '/clients': '客户端', '/client-authorize': '客户端授权', '/apikeys': 'API Key',
  '/notifications': '通知', '/download': '下载 Morde Desktop',
};

export function PortalLayout() {
  const { user, signOut } = useAuth();
  const { notify } = useToast();
  const navigate = useNavigate();
  const location = useLocation();
  const pageTitle = titles[location.pathname] ?? (location.pathname.startsWith('/tasks/') ? '任务详情' : 'PC 用户端');
  const logout = async () => {
    try { await signOut(); navigate('/login', { replace: true }); }
    catch (error) { notify(`退出登录失败：${errorDetails(error).message}`, 'error'); }
  };

  return <div className="morde-shell">
    <a className="skip-link" href="#main-content">跳到主要内容</a>
    <aside className="morde-sidebar" aria-label="PC 用户端导航">
      <Link to="/" className="morde-brand"><span className="morde-brand-mark">M</span><span><strong>Morde</strong><small>把工作继续做完</small></span></Link>
      <Link to="/workspaces" className="morde-space-chip"><span aria-hidden="true">◈</span><span>我的工作空间</span></Link>
      <nav className="morde-navigation" aria-label="工作导航">
        <span className="morde-nav-heading">工作</span>
        {primaryLinks.map(({ to, label, icon, end }) => <NavLink className="morde-nav-link" to={to} end={end} key={to}><span className="morde-nav-icon" aria-hidden="true">{icon}</span><span>{label}</span></NavLink>)}
        <span className="morde-nav-heading morde-nav-heading-spaced">设置与授权</span>
        {settingLinks.map(({ to, label, icon }) => <NavLink className="morde-nav-link" to={to} key={to}><span className="morde-nav-icon" aria-hidden="true">{icon}</span><span>{label}</span></NavLink>)}
      </nav>
      <div className="morde-sidebar-bottom">
        {(user?.role === 'admin' || user?.role === 'super_admin') && <a className="morde-nav-link" href="/admin/"><span className="morde-nav-icon" aria-hidden="true">⚙</span><span>管理后台</span></a>}
        <div className="morde-profile"><span className="morde-avatar" aria-hidden="true">{user?.name.slice(0, 1) || 'M'}</span><span><strong>{user?.name}</strong><small>PC 用户端</small></span><button className="button button-quiet button-small" type="button" onClick={() => void logout()}>退出</button></div>
      </div>
    </aside>
    <main className="morde-main">
      <header className="morde-topbar"><div className="morde-crumb"><span>PC 用户端</span><span aria-hidden="true">›</span><strong>{pageTitle}</strong></div><div className="morde-top-actions"><a className="morde-notification-link" href="/app/" aria-label="打开手机端"><span aria-hidden="true">▣</span><span>手机端</span></a><Link className="morde-notification-link" to="/notifications" aria-label="查看通知"><span aria-hidden="true">♧</span><span>通知</span></Link><span className="morde-top-user">{user?.name}</span></div></header>
      <div id="main-content" className="morde-page" tabIndex={-1}><Outlet /></div>
    </main>
  </div>;
}
