import { Link, NavLink, Outlet, useLocation, useNavigate } from 'react-router';
import { useAuth } from '../app/auth';
import { errorDetails } from '../app/api';
import { useToast } from '../app/toast';
import { MordeIcon, type MordeIconName } from './MordeIcon';

const primaryLinks: { to: string; label: string; icon: MordeIconName; end?: boolean }[] = [
  { to: '/hosts', label: '云端电脑', icon: 'monitor', end: true },
  { to: '/assistant', label: 'moder', icon: 'spark' },
  { to: '/projects', label: '项目', icon: 'folder' },
  { to: '/personal', label: '个人数据', icon: 'user' },
  { to: '/goals', label: '目标与计划', icon: 'target' },
  { to: '/workspaces', label: '工作区', icon: 'folder' },
];

const settingLinks: { to: string; label: string; icon: MordeIconName }[] = [
  { to: '/connections', label: '模型连接', icon: 'link' },
  { to: '/clients', label: '客户端', icon: 'monitor' },
  { to: '/apikeys', label: 'API Key', icon: 'key' },
  { to: '/settings', label: '账户设置', icon: 'settings' },
];

const titles: Record<string, string> = {
  '/assistant': 'moder', '/hosts': '云端电脑', '/projects': '项目',
  '/personal': '个人数据', '/goals': '目标与计划',
  '/hosts/new': '创建电脑', '/workspaces': '工作区', '/connections': '模型连接',
  '/clients': '客户端', '/apikeys': 'API Key',
  '/settings': '账户设置',
  '/notifications': '通知', '/download': '下载 Morde Desktop',
};

export function PortalLayout() {
  const { user, signOut } = useAuth();
  const { notify } = useToast();
  const navigate = useNavigate();
  const location = useLocation();
  const pageTitle = titles[location.pathname] ?? 'PC 用户端';
  const logout = async () => {
    try { await signOut(); navigate('/login', { replace: true }); }
    catch (error) { notify(`退出登录失败：${errorDetails(error).message}`, 'error'); }
  };

  return <div className="morde-shell">
    <a className="skip-link" href="#main-content">跳到主要内容</a>
    <aside className="morde-sidebar" aria-label="PC 用户端导航">
      <Link to="/" className="morde-brand"><span className="morde-brand-mark">M</span><span><strong>Morde</strong><small>PC 用户端</small></span></Link>
      <nav className="morde-navigation" aria-label="工作导航">
        <span className="morde-nav-heading">工作</span>
        {primaryLinks.filter(({ to }) => to !== '/projects' || user?.role === 'user').map(({ to, label, icon, end }) => <NavLink className="morde-nav-link" to={to} end={end} key={to} aria-label={label} title={label}><MordeIcon name={icon} /><span>{label}</span></NavLink>)}
        <span className="morde-nav-heading morde-nav-heading-spaced">设置与授权</span>
        {settingLinks.map(({ to, label, icon }) => <NavLink className="morde-nav-link" to={to} key={to} aria-label={label} title={label}><MordeIcon name={icon} /><span>{label}</span></NavLink>)}
      </nav>
      <div className="morde-sidebar-bottom">
        {(user?.role === 'admin' || user?.role === 'super_admin') && <a className="morde-nav-link" href="/admin/" aria-label="切换至管理后台" title="切换至管理后台"><MordeIcon name="link" /><span>切换至管理后台</span></a>}
        <div className="morde-profile"><span className="morde-avatar" aria-hidden="true">{user?.name.slice(0, 1) || 'M'}</span><span><strong>{user?.name}</strong><small>PC 用户端</small></span><button className="button button-quiet button-small" type="button" onClick={() => void logout()}>退出</button></div>
      </div>
    </aside>
    <main className="morde-main">
      <header className="morde-topbar"><div className="morde-crumb"><span>PC 用户端</span><span aria-hidden="true">/</span><h1 className="morde-crumb-title">{pageTitle}</h1></div><div className="morde-top-actions"><a className="morde-notification-link" href="/app/" aria-label="打开手机端"><MordeIcon name="monitor" /></a><Link className="morde-notification-link" to="/notifications" aria-label="查看通知"><MordeIcon name="bell" /></Link><span className="morde-avatar" aria-hidden="true">{user?.name.slice(0, 1) || 'M'}</span><span className="morde-top-user">{user?.name}</span></div></header>
      <div id="main-content" className="morde-page" tabIndex={-1}><Outlet /></div>
    </main>
  </div>;
}
