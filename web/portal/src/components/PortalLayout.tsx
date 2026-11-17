import { createContext, useCallback, useContext, useEffect, useRef, useState, type CSSProperties, type ReactNode } from 'react';
import { Link, NavLink, Outlet, useLocation, useNavigate, useNavigationType } from 'react-router';
import { useAuth } from '../app/auth';
import { errorDetails } from '../app/api';
import { ToastNoticeCenter, useToast } from '../app/toast';
import { ConfirmDialog } from './ConfirmDialog';
import { MordeIcon, type MordeIconName } from './MordeIcon';

const primaryLinks: { to: string; label: string; icon: MordeIconName; end?: boolean; view?: 'goals' | 'schedules' }[] = [
  { to: '/assistant', label: '对话', icon: 'chat' },
  { to: '/reviews', label: '审核', icon: 'shield' },
  { to: '/goals?view=goals', label: '目标', icon: 'target', view: 'goals' },
  { to: '/goals?view=schedules', label: '定时计划', icon: 'refresh', view: 'schedules' },
];

const settingLinks: { to: string; label: string; icon: MordeIconName }[] = [
  { to: '/connections', label: '模型与工具', icon: 'link' },
  { to: '/hosts', label: '云端电脑', icon: 'monitor' },
  { to: '/apikeys', label: 'API Key', icon: 'key' },
  { to: '/settings', label: '账户', icon: 'user' },
];
const settingsEntry = { to: '/connections', label: '设置', icon: 'settings' as const };

type PageNavigationItem = { label: string; to: string; icon: MordeIconName; query?: [string, string] };
type PageNavigation = { title: string; items: PageNavigationItem[]; queryDefault?: [string, string] };
type RouteHistoryEntry = { key: string; url: string };
type RouteHistoryState = { canGoBack: boolean; canGoForward: boolean; goBack: () => void; goForward: () => void };
type AssistantPanelTab = 'tasks' | 'files' | 'computer';
const assistantPanelTabs: { id: AssistantPanelTab; label: string; icon: MordeIconName }[] = [
  { id: 'computer', label: '云端电脑', icon: 'monitor' },
  { id: 'tasks', label: '任务动态', icon: 'tasks' },
  { id: 'files', label: '文件', icon: 'folder' },
];

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
  ['library', '资源库', 'folder'],
];
const connectionNavigationLinks: PageNavigationItem[] = [
  ...settingLinks.slice(0, 2),
  { label: '文件同步', icon: 'folder', to: '/hosts?sync=1', query: ['sync', '1'] },
  ...settingLinks.slice(2, 3),
];
const settingsNavigationLinks: PageNavigationItem[] = [
  ...connectionNavigationLinks,
  ...settingLinks.slice(3),
  ...personalSections.map(([section, label, icon]) => ({ label, icon, to: `/personal?section=${section}`, query: ['section', section] as [string, string] })),
];

const pageNavigationFor = (pathname: string): PageNavigation | null => {
  if (pathname === '/hosts' || pathname === '/hosts/new') return {
    title: '设置与授权', queryDefault: ['sync', '0'],
    items: settingsNavigationLinks.map((item) => item.to === '/hosts' ? { ...item, query: ['sync', '0'] as [string, string] } : item),
  };
  if (pathname === '/personal') return {
    title: '设置与授权', queryDefault: ['section', 'profile'], items: settingsNavigationLinks,
  };
  if (pathname === '/clients' || settingLinks.some(({ to }) => to === pathname)) return { title: '设置与授权', items: settingsNavigationLinks };
  return null;
};

function readContextSidebarCollapsed() {
  try { return localStorage.getItem('morde:pc-context-sidebar-collapsed') === 'true'; }
  catch { return false; }
}

function matchesPageItem(item: PageNavigationItem, pathname: string, search: string, fallback?: [string, string]) {
  const target = new URL(item.to, window.location.origin);
  if (target.pathname !== pathname && !(target.pathname === '/hosts' && pathname === '/hosts/new')) return false;
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
  const [assistantPanelOpen, setAssistantPanelOpen] = useState(true);
  const [assistantWorkspaceWidth, setAssistantWorkspaceWidth] = useState<number | null>(null);
  const [assistantPanelTab, setAssistantPanelTab] = useState<AssistantPanelTab>('computer');
  const [assistantOpenPanelTabs, setAssistantOpenPanelTabs] = useState<AssistantPanelTab[]>(['computer', 'tasks', 'files']);
  const assistantPanelToggle = useRef<HTMLButtonElement>(null);
  const focusAssistantPanelToggle = useCallback(() => assistantPanelToggle.current?.focus(), []);
  const assistantWorkspaceAddMenu = useRef<HTMLDetailsElement>(null);
  const accountMenu = useRef<HTMLDetailsElement>(null);
  const pageNavigation = pageNavigationFor(location.pathname);
  const isAssistantPage = location.pathname === '/assistant';
  const isSettingsNavigation = pageNavigation?.title === '设置与授权';
  const hasCollapsibleSidebar = Boolean(pageNavigation) || isAssistantPage;
  const navigationItems = pageNavigation ? pageNavigation.items.map((item) => {
    const active = matchesPageItem(item, location.pathname, location.search, pageNavigation.queryDefault);
    return <Link key={item.to} className={`page-navigation-link${active ? ' active' : ''}`} aria-current={active ? 'page' : undefined} to={item.to} title={item.label}>
      <MordeIcon name={item.icon} /><span>{item.label}</span>
    </Link>;
  }) : null;

  useEffect(() => { closeNotices(); }, [closeNotices, location.pathname, location.search]);
  useEffect(() => {
    const closeOutside = (event: PointerEvent) => {
      const accountMenuElement = accountMenu.current;
      if (accountMenuElement?.open && !accountMenuElement.contains(event.target as Node)) accountMenuElement.open = false;
      if (!isAssistantPage) return;
      const menu = assistantWorkspaceAddMenu.current;
      if (menu?.open && !menu.contains(event.target as Node)) menu.open = false;
    };
    const closeOnEscape = (event: KeyboardEvent) => {
      const menu = assistantWorkspaceAddMenu.current;
      if (event.key !== 'Escape' || !menu?.open) return;
      event.preventDefault();
      event.stopPropagation();
      menu.open = false;
      menu.querySelector('summary')?.focus();
    };
    document.addEventListener('pointerdown', closeOutside);
    if (isAssistantPage) document.addEventListener('keydown', closeOnEscape);
    return () => {
      document.removeEventListener('pointerdown', closeOutside);
      if (isAssistantPage) document.removeEventListener('keydown', closeOnEscape);
    };
  }, [isAssistantPage]);

  useEffect(() => {
    try { localStorage.setItem('morde:pc-context-sidebar-collapsed', contextSidebarCollapsed ? 'true' : 'false'); }
    catch { /* Keep navigation usable when browser storage is unavailable. */ }
  }, [contextSidebarCollapsed]);

  const logout = async () => {
    try { await signOut(); navigate('/login', { replace: true }); }
    catch (error) { notify(`退出登录失败：${errorDetails(error).message}`, 'error'); }
  };

  const toggleFullscreen = async () => {
    try {
      if (document.fullscreenElement) await document.exitFullscreen();
      else await document.documentElement.requestFullscreen();
    } catch { notify('当前窗口不支持全屏操作。', 'error'); }
  };

  const openAssistantPanel = (tab: AssistantPanelTab) => {
    setAssistantOpenPanelTabs((openTabs) => openTabs.includes(tab) ? openTabs : [...openTabs, tab]);
    setAssistantPanelTab(tab);
    setAssistantPanelOpen(true);
  };
  const closeAssistantPanelTab = (tab: AssistantPanelTab) => {
    const remaining = assistantPanelTabs.filter((item) => assistantOpenPanelTabs.includes(item.id) && item.id !== tab).map((item) => item.id);
    setAssistantOpenPanelTabs(remaining);
    if (assistantPanelTab === tab) {
      if (remaining.length) setAssistantPanelTab(remaining[remaining.length - 1] ?? 'computer');
      else setAssistantPanelOpen(false);
    }
  };

  return <div className={`morde-shell${contextSidebarCollapsed ? ' morde-shell-context-collapsed' : ''}${pageNavigation ? ' morde-shell-with-page-navigation' : ''}${isSettingsNavigation ? ' morde-shell-settings-navigation' : ''}${isAssistantPage ? ` morde-shell-assistant-workspace${assistantPanelOpen ? '' : ' morde-shell-assistant-workspace-collapsed'}` : ''}`}
    style={isAssistantPage && assistantWorkspaceWidth !== null ? { '--pc-workspace-panel-user-width': `${assistantWorkspaceWidth}px` } as CSSProperties : undefined}>
    <a className="skip-link" href="#main-content">跳到主要内容</a>
    <header className="morde-topbar"><div className="morde-top-navigation">
      <button type="button" className="morde-top-control" aria-label="返回上一页" title="返回上一页" disabled={!canGoBack} onClick={goBack}><MordeIcon name="back" /></button>
      <button type="button" className="morde-top-control" aria-label="前进" title="前进" disabled={!canGoForward} onClick={goForward}><MordeIcon name="forward" /></button>
      <span className="morde-top-divider" aria-hidden="true" />
      {hasCollapsibleSidebar && <button type="button" className="morde-top-control" aria-label={contextSidebarCollapsed ? '展开侧边栏' : '折叠侧边栏'} title={contextSidebarCollapsed ? '展开侧边栏' : '折叠侧边栏'} aria-expanded={!contextSidebarCollapsed} aria-controls="context-sidebar" onClick={() => setContextSidebarCollapsed((collapsed) => !collapsed)}><MordeIcon name="panel" /></button>}
      {isAssistantPage && <div className="morde-top-navigation-status"><ToastNoticeCenter autoOpenErrors /></div>}
    </div><div className={`morde-top-actions${isAssistantPage ? ' morde-top-actions-assistant' : ''}`}>{!isAssistantPage && <ToastNoticeCenter autoOpenErrors />}{isAssistantPage ? <>
      {assistantPanelOpen && <nav className="morde-workspace-tabs" aria-label="对话工作区">
      <div className="morde-workspace-tab-list">{assistantPanelTabs.filter(({ id }) => assistantOpenPanelTabs.includes(id)).map(({ id: tab, label, icon }) => {
        const active = assistantPanelTab === tab;
        return <div key={tab} className={`morde-workspace-tab-wrap${active ? ' active' : ''}`}>
          <button type="button" aria-pressed={active} className="morde-workspace-tab"
            onClick={() => openAssistantPanel(tab)}><MordeIcon name={icon} /><span>{label}</span></button>
          <button type="button" className="morde-workspace-tab-close" aria-label={`关闭${label}标签`} title="关闭标签"
            onClick={() => closeAssistantPanelTab(tab)}>×</button>
        </div>;
      })}</div>
      <details ref={assistantWorkspaceAddMenu} className="morde-workspace-add">
        <summary className="morde-workspace-icon" aria-label="添加工作区标签" title="添加标签"><MordeIcon name="plus" /></summary>
        <div className="morde-workspace-add-menu">{assistantPanelTabs.map(({ id, label }) => <button key={id} type="button"
          onClick={(event) => { const menu = event.currentTarget.closest('details'); if (menu) menu.open = false; openAssistantPanel(id); }}>{id === 'files' ? '文件与产物' : label}</button>)}</div>
      </details>
      <button type="button" className="morde-workspace-icon" aria-label="切换全屏" title="切换全屏" onClick={() => void toggleFullscreen()}><MordeIcon name="fullscreen" /></button>
      </nav>}
      <button ref={assistantPanelToggle} type="button" className="morde-workspace-icon"
        aria-label={assistantPanelOpen ? '隐藏工作区侧栏' : '显示工作区侧栏'} title={assistantPanelOpen ? '隐藏工作区侧栏' : '显示工作区侧栏'}
        aria-expanded={assistantPanelOpen} onClick={() => setAssistantPanelOpen((open) => !open)}><MordeIcon name="panel" /></button>
    </> : <><Link className="morde-notification-link" to="/clients" aria-label="查看客户端与设备"><MordeIcon name="monitor" /></Link><Link className="morde-notification-link" to="/notifications" aria-label="查看通知"><MordeIcon name="bell" /></Link><span className="morde-avatar" aria-hidden="true">{user?.name.slice(0, 1) || 'M'}</span><span className="morde-top-user">{user?.name}</span></>}</div></header>
    <aside className="morde-sidebar" aria-label="PC 用户端导航">
      <Link to="/assistant" className="morde-brand" aria-label="Morde 对话" title="Morde 对话"><span className="morde-brand-mark" aria-hidden="true">M</span><span><strong>Morde</strong><small>PC 用户端</small></span></Link>
      <nav id="primary-navigation" className="morde-navigation" aria-label="工作导航">
        <span className="morde-nav-heading">工作</span>
        {primaryLinks.map(({ to, label, icon, end, view }) => {
          if (view) {
            const activeView = new URLSearchParams(location.search).get('view') === 'schedules' ? 'schedules' : 'goals';
            const active = location.pathname === '/goals' && activeView === view;
            return <Link className={`morde-nav-link${active ? ' active' : ''}`} to={to} key={to}
              aria-label={label} aria-current={active ? 'page' : undefined} title={label}>
              <MordeIcon name={icon} /><span>{label}</span>
            </Link>;
          }
          return <NavLink className={({ isActive }) => `morde-nav-link${isActive ? ' active' : ''}`} to={to} end={end} key={to}
            aria-label={label} title={label}>
            <MordeIcon name={icon} /><span>{label}</span>
          </NavLink>;
        })}
      </nav>
      <div className="morde-sidebar-bottom">
        <div className="morde-profile">
          <details ref={accountMenu} className="morde-account-menu">
            <summary className="morde-avatar" aria-label="账号菜单" title="账号菜单">{user?.name.slice(0, 1) || 'M'}</summary>
            <div className="morde-account-menu-popover">
              <strong>{user?.name}</strong><small>PC 用户端</small>
              <NavLink to={settingsEntry.to} className="morde-account-menu-action" aria-label={settingsEntry.label} title={settingsEntry.label}><MordeIcon name={settingsEntry.icon} />{settingsEntry.label}</NavLink>
              <NavLink to="/notifications" className="morde-account-menu-action" aria-label="查看通知" title="查看通知"><MordeIcon name="bell" />通知</NavLink>
              <NavLink to="/codex" className="morde-account-menu-action" aria-label="Codex 会话" title="Codex 会话"><MordeIcon name="spark" />Codex 会话</NavLink>
              {user?.role === 'user' && <NavLink to="/projects" className="morde-account-menu-action" aria-label="项目" title="项目"><MordeIcon name="folder" />项目</NavLink>}
              {(user?.role === 'admin' || user?.role === 'super_admin') && <a className="morde-account-menu-action" href="/admin/" aria-label="切换至管理后台"><MordeIcon name="link" />切换至管理后台</a>}
              <button className="morde-account-menu-action morde-profile-logout" type="button" onClick={() => setLogoutConfirmationOpen(true)} aria-label="退出登录" title="退出登录"><MordeIcon name="logout" />退出登录</button>
            </div>
          </details>
        </div>
      </div>
    </aside>
    {pageNavigation && <aside id="context-sidebar" className={`page-navigation${isSettingsNavigation ? ' settings-page-navigation' : ''}`} aria-label={`${pageNavigation.title}子菜单`}>
      {isSettingsNavigation && <h2>设置</h2>}
      <nav aria-label={`${pageNavigation.title}分类`}>
        {isSettingsNavigation ? <><p className="page-navigation-group">连接与设备</p>{navigationItems?.slice(0, connectionNavigationLinks.length)}<p className="page-navigation-group">个人设置</p>{navigationItems?.slice(connectionNavigationLinks.length)}</> : navigationItems}
      </nav>
    </aside>}
    <main className="morde-main">
      <div id="main-content" className="morde-page" tabIndex={-1}><Outlet context={{ panelOpen: assistantPanelOpen, panelTab: assistantPanelTab,
        workspaceWidth: assistantWorkspaceWidth, setWorkspaceWidth: setAssistantWorkspaceWidth,
        setPanelOpen: setAssistantPanelOpen, setPanelTab: setAssistantPanelTab,
        focusPanelToggle: focusAssistantPanelToggle }} /></div>
    </main>
    {logoutConfirmationOpen && <ConfirmDialog title="退出登录" description="确定要退出 Morde 吗？退出后需要重新登录才能继续使用。" confirmLabel="退出登录" danger onClose={() => setLogoutConfirmationOpen(false)} onConfirm={() => { setLogoutConfirmationOpen(false); void logout(); }} />}
  </div>;
}
