import { createContext, useContext, useEffect, useRef, useState, type ReactNode } from 'react';
import { Link, NavLink, Outlet, useLocation, useNavigate, useNavigationType } from 'react-router';
import { useAuth } from '../app/auth';
import { errorDetails } from '../app/api';
import { ToastNoticeCenter, useToast } from '../app/toast';
import { ConfirmDialog } from './ConfirmDialog';
import { MordeIcon, type MordeIconName } from './MordeIcon';

const primaryLinks: { to: string; label: string; icon: MordeIconName; end?: boolean }[] = [
  { to: '/hosts', label: '云端电脑', icon: 'monitor' },
  { to: '/assistant', label: '对话', icon: 'spark' },
  { to: '/codex', label: 'Codex 会话', icon: 'spark' },
  { to: '/projects', label: '项目', icon: 'folder' },
  { to: '/personal', label: '个人数据', icon: 'user' },
  { to: '/goals', label: '目标与计划', icon: 'target' },
];

const settingLinks: { to: string; label: string; icon: MordeIconName }[] = [
  { to: '/connections', label: '模型与工具', icon: 'link' },
  { to: '/clients', label: '客户端', icon: 'panel' },
  { to: '/apikeys', label: 'API Key', icon: 'key' },
  { to: '/settings', label: '账户', icon: 'user' },
];

type PageNavigationItem = { label: string; to: string; icon: MordeIconName; query?: [string, string] };
type PageNavigation = { title: string; items: PageNavigationItem[]; queryDefault?: [string, string] };
type RouteHistoryEntry = { key: string; url: string };
type RouteHistoryState = { canGoBack: boolean; canGoForward: boolean; goBack: () => void; goForward: () => void };

const RouteHistoryContext = createContext<RouteHistoryState>({ canGoBack: false, canGoForward: false, goBack: () => {}, goForward: () => {} });

export function PortalNavigationHistoryProvider({ children }: { children: ReactNode }) {
  const navigate = useNavigate();
  const location = useLocation();
  const navigationType = useNavigationType();
  const routeHistory = useRef<RouteHistoryEntry[]>([]);
  const routeHistoryIndex = useRef(0);
  const [canGoBack, setCanGoBack] = useState(false);
  const [canGoForward, setCanGoForward] = useState(false);

  useEffect(() => {
    const current = { key: location.key, url: `${location.pathname}${location.search}${location.hash}` };
    if (!routeHistory.current.length) {
      routeHistory.current = [current];
      routeHistoryIndex.current = 0;
    } else {
      const keyIndex = routeHistory.current.findIndex((entry) => entry.key === current.key);
      const existingIndex = keyIndex >= 0 ? keyIndex : routeHistory.current.findIndex((entry) => entry.url === current.url);
      if (navigationType === 'POP' && existingIndex >= 0) routeHistoryIndex.current = existingIndex;
      else if (navigationType === 'REPLACE') routeHistory.current[routeHistoryIndex.current] = current;
      else if (routeHistory.current[routeHistoryIndex.current]?.key !== current.key) {
        if (navigationType === 'POP') {
          routeHistory.current = [current];
          routeHistoryIndex.current = 0;
        } else {
          routeHistory.current = [...routeHistory.current.slice(0, routeHistoryIndex.current + 1), current];
          routeHistoryIndex.current = routeHistory.current.length - 1;
        }
      }
    }
    setCanGoBack(routeHistoryIndex.current > 0);
    setCanGoForward(routeHistoryIndex.current < routeHistory.current.length - 1);
  }, [location.hash, location.key, location.pathname, location.search, navigationType]);

  return <RouteHistoryContext.Provider value={{ canGoBack, canGoForward, goBack: () => navigate(-1), goForward: () => navigate(1) }}>{children}</RouteHistoryContext.Provider>;
}

const personalSections: Array<[string, string, MordeIconName]> = [
  ['profile', '个人资料', 'user'], ['soul', 'AI 个性', 'spark'], ['memory', '记忆', 'file'],
  ['feed', '动态', 'file'], ['ideas', '灵感', 'target'], ['library', '资源库', 'folder'], ['preferences', '偏好', 'settings'],
];

const pageNavigationFor = (pathname: string): PageNavigation | null => {
  if (pathname === '/hosts' || pathname.startsWith('/hosts/')) return {
    title: '云端电脑', queryDefault: ['sync', '0'],
    items: [
      { label: '云端电脑', icon: 'monitor', to: '/hosts', query: ['sync', '0'] },
      { label: '文件同步', icon: 'folder', to: '/hosts?sync=1', query: ['sync', '1'] },
    ],
  };
  if (pathname === '/personal') return {
    title: '个人数据', queryDefault: ['section', 'profile'],
    items: personalSections.map(([value, label, icon]) => ({ label, icon, to: `/personal?section=${value}`, query: ['section', value] })),
  };
  if (pathname === '/goals') return {
    title: '目标与计划', queryDefault: ['view', 'goals'],
    items: [
      { label: '目标', icon: 'target', to: '/goals?view=goals', query: ['view', 'goals'] },
      { label: '定时计划', icon: 'refresh', to: '/goals?view=schedules', query: ['view', 'schedules'] },
    ],
  };
  return null;
};

function readContextSidebarCollapsed() {
  try { return localStorage.getItem('morde:pc-context-sidebar-collapsed') === 'true'; }
  catch { return false; }
}

function matchesPageItem(item: PageNavigationItem, pathname: string, search: string, fallback?: [string, string]) {
  const target = new URL(item.to, window.location.origin);
  if (target.pathname !== pathname && !(target.pathname === '/hosts' && pathname.startsWith('/hosts/'))) return false;
  if (!item.query) return true;
  const params = new URLSearchParams(search);
  const fallbackValue = fallback?.[0] === item.query[0] ? fallback[1] : undefined;
  return (params.get(item.query[0]) ?? fallbackValue) === item.query[1];
}

export function PortalLayout() {
  const { user, signOut } = useAuth();
  const { notify, closeNotices } = useToast();
  const navigate = useNavigate();
  const location = useLocation();
  const { canGoBack, canGoForward, goBack, goForward } = useContext(RouteHistoryContext);
  const [contextSidebarCollapsed, setContextSidebarCollapsed] = useState(readContextSidebarCollapsed);
  const [logoutConfirmationOpen, setLogoutConfirmationOpen] = useState(false);
  const pageNavigation = pageNavigationFor(location.pathname);
  const hasCollapsibleSidebar = Boolean(pageNavigation) || location.pathname === '/assistant';

  useEffect(() => { closeNotices(); }, [closeNotices, location.pathname, location.search]);

  useEffect(() => {
    try { localStorage.setItem('morde:pc-context-sidebar-collapsed', contextSidebarCollapsed ? 'true' : 'false'); }
    catch { /* Keep navigation usable when browser storage is unavailable. */ }
  }, [contextSidebarCollapsed]);

  const logout = async () => {
    try { await signOut(); navigate('/login', { replace: true }); }
    catch (error) { notify(`退出登录失败：${errorDetails(error).message}`, 'error'); }
  };

  return <div className={`morde-shell${contextSidebarCollapsed ? ' morde-shell-context-collapsed' : ''}${pageNavigation ? ' morde-shell-with-page-navigation' : ''}`}>
    <a className="skip-link" href="#main-content">跳到主要内容</a>
    <header className="morde-topbar"><div className="morde-top-navigation">
      <button type="button" className="morde-top-control" aria-label="返回上一页" title="返回上一页" disabled={!canGoBack} onClick={goBack}><MordeIcon name="back" /></button>
      <button type="button" className="morde-top-control" aria-label="前进" title="前进" disabled={!canGoForward} onClick={goForward}><MordeIcon name="forward" /></button>
      <span className="morde-top-divider" aria-hidden="true" />
      {hasCollapsibleSidebar && <button type="button" className="morde-top-control" aria-label={contextSidebarCollapsed ? '展开侧边栏' : '折叠侧边栏'} title={contextSidebarCollapsed ? '展开侧边栏' : '折叠侧边栏'} aria-expanded={!contextSidebarCollapsed} aria-controls="context-sidebar" onClick={() => setContextSidebarCollapsed((collapsed) => !collapsed)}><MordeIcon name="panel" /></button>}
    </div><ToastNoticeCenter /><div className="morde-top-actions"><Link className="morde-notification-link" to="/clients" aria-label="查看客户端与设备"><MordeIcon name="monitor" /></Link><Link className="morde-notification-link" to="/notifications" aria-label="查看通知"><MordeIcon name="bell" /></Link><span className="morde-avatar" aria-hidden="true">{user?.name.slice(0, 1) || 'M'}</span><span className="morde-top-user">{user?.name}</span></div></header>
    <aside className="morde-sidebar" aria-label="PC 用户端导航">
      <Link to="/assistant" className="morde-brand" aria-label="Morde 对话" title="Morde 对话"><span className="morde-brand-mark" aria-hidden="true">M</span><span><strong>Morde</strong><small>PC 用户端</small></span></Link>
      <nav id="primary-navigation" className="morde-navigation" aria-label="工作导航">
        <span className="morde-nav-heading">工作</span>
        {primaryLinks.filter(({ to }) => to !== '/projects' || user?.role === 'user').map(({ to, label, icon, end }) => <NavLink className={({ isActive }) => `morde-nav-link${isActive ? ' active' : ''}`} to={to} end={end} key={to} aria-label={label} title={label}><MordeIcon name={icon} /><span>{label}</span></NavLink>)}
      </nav>
      <nav className="morde-navigation" aria-label="设置导航">
        <span className="morde-nav-heading morde-nav-heading-spaced">设置与授权</span>
        {settingLinks.map(({ to, label, icon }) => <NavLink className={({ isActive }) => `morde-nav-link${isActive ? ' active' : ''}`} to={to} key={to} aria-label={label} title={label}><MordeIcon name={icon} /><span>{label}</span></NavLink>)}
      </nav>
      <div className="morde-sidebar-bottom">
        <div className="morde-profile">
          <details className="morde-account-menu">
            <summary className="morde-avatar" aria-label="账号菜单" title="账号菜单">{user?.name.slice(0, 1) || 'M'}</summary>
            <div className="morde-account-menu-popover">
              <strong>{user?.name}</strong><small>PC 用户端</small>
              <NavLink to="/settings" className="morde-account-menu-action" aria-label="账户设置" title="账户设置"><MordeIcon name="settings" />账户设置</NavLink>
              {(user?.role === 'admin' || user?.role === 'super_admin') && <a className="morde-account-menu-action" href="/admin/" aria-label="切换至管理后台"><MordeIcon name="link" />切换至管理后台</a>}
              <button className="morde-account-menu-action morde-profile-logout" type="button" onClick={() => setLogoutConfirmationOpen(true)} aria-label="退出登录" title="退出登录"><MordeIcon name="logout" />退出登录</button>
            </div>
          </details>
        </div>
      </div>
    </aside>
    {pageNavigation && <aside id="context-sidebar" className="page-navigation" aria-label={`${pageNavigation.title}子菜单`}>
      <nav aria-label={`${pageNavigation.title}分类`}>
        {pageNavigation.items.map((item) => {
          const active = matchesPageItem(item, location.pathname, location.search, pageNavigation.queryDefault);
          return <Link key={item.to} className={`page-navigation-link${active ? ' active' : ''}`} aria-current={active ? 'page' : undefined} to={item.to} title={item.label}><MordeIcon name={item.icon} /><span>{item.label}</span></Link>;
        })}
      </nav>
    </aside>}
    <main className="morde-main">
      <div id="main-content" className="morde-page" tabIndex={-1}><Outlet /></div>
    </main>
    {logoutConfirmationOpen && <ConfirmDialog title="退出登录" description="确定要退出 Morde 吗？退出后需要重新登录才能继续使用。" confirmLabel="退出登录" danger onClose={() => setLogoutConfirmationOpen(false)} onConfirm={() => { setLogoutConfirmationOpen(false); void logout(); }} />}
  </div>;
}
