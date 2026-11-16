import { createContext, useContext, useEffect, useRef, useState, type ReactNode } from 'react';
import { useQuery } from '@tanstack/react-query';
import { Link, NavLink, Outlet, useLocation, useNavigate, useNavigationType } from 'react-router';
import { useAuth } from '../app/auth';
import { errorDetails, portalApi } from '../app/api';
import { isTransientNetworkError } from '../api/errors';
import { ToastNoticeCenter, useToast } from '../app/toast';
import { ConfirmDialog } from './ConfirmDialog';
import { MordeIcon, type MordeIconName } from './MordeIcon';

const primaryLinks: { to: string; label: string; icon: MordeIconName; end?: boolean }[] = [
  { to: '/assistant', label: '对话', icon: 'chat' },
  { to: '/reviews', label: '审核', icon: 'shield' },
  { to: '/personal', label: '个人数据', icon: 'user' },
  { to: '/goals', label: '目标与计划', icon: 'target' },
];

const settingLinks: { to: string; label: string; icon: MordeIconName }[] = [
  { to: '/connections', label: '模型与工具', icon: 'link' },
  { to: '/hosts', label: '云端电脑', icon: 'monitor' },
  { to: '/clients', label: '客户端', icon: 'panel' },
  { to: '/apikeys', label: 'API Key', icon: 'key' },
  { to: '/settings', label: '账户', icon: 'user' },
];
const settingsEntry = { to: '/connections', label: '设置与授权', icon: 'settings' as const };

type PageNavigationItem = { label: string; to: string; icon: MordeIconName; query?: [string, string] };
type PageNavigation = { title: string; items: PageNavigationItem[]; queryDefault?: [string, string] };
type RouteHistoryEntry = { key: string; url: string };
type RouteHistoryState = { canGoBack: boolean; canGoForward: boolean; goBack: () => void; goForward: () => void };
type AssistantPanelTab = 'tasks' | 'files' | 'computer';

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
  if (settingLinks.some(({ to }) => to === pathname)) return { title: '设置与授权', items: settingLinks };
  if (pathname === '/reviews') return { title: '审核', items: [{ label: '审核事项', icon: 'shield', to: '/reviews' }] };
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
  if (target.pathname !== pathname) return false;
  if (!item.query) return true;
  const params = new URLSearchParams(search);
  const fallbackValue = fallback?.[0] === item.query[0] ? fallback[1] : undefined;
  return (params.get(item.query[0]) ?? fallbackValue) === item.query[1];
}

export function PortalLayout() {
  const { user, signOut } = useAuth();
  const { notify, dismiss, closeNotices } = useToast();
  const navigate = useNavigate();
  const location = useLocation();
  const { canGoBack, canGoForward, goBack, goForward } = useContext(RouteHistoryContext);
  const approvalCount = useQuery({
    queryKey: ['task-approvals', 'count', user?.id],
    queryFn: () => portalApi.taskApprovals(),
    enabled: Boolean(user), staleTime: 15_000, refetchInterval: 30_000,
  });
  const refetchApprovalCount = approvalCount.refetch;
  const approvalCountErrorToast = useRef<number | null>(null);
  const [contextSidebarCollapsed, setContextSidebarCollapsed] = useState(readContextSidebarCollapsed);
  const [logoutConfirmationOpen, setLogoutConfirmationOpen] = useState(false);
  const [assistantPanelOpen, setAssistantPanelOpen] = useState(false);
  const [assistantPanelTab, setAssistantPanelTab] = useState<AssistantPanelTab>('computer');
  const [assistantNewChatRequest, setAssistantNewChatRequest] = useState(0);
  const pageNavigation = pageNavigationFor(location.pathname);
  const hasCollapsibleSidebar = Boolean(pageNavigation) || location.pathname === '/assistant';
  const isAssistantPage = location.pathname === '/assistant';

  useEffect(() => { closeNotices(); }, [closeNotices, location.pathname, location.search]);

  const approvalCountNetworkError = approvalCount.isError && isTransientNetworkError(approvalCount.error);
  useEffect(() => {
    if (!approvalCountNetworkError || location.pathname === '/assistant' || location.pathname === '/reviews') {
      if (approvalCountErrorToast.current !== null) dismiss(approvalCountErrorToast.current);
      approvalCountErrorToast.current = null;
      return;
    }
    if (approvalCountErrorToast.current === null) {
      approvalCountErrorToast.current = notify('审核状态暂时无法更新。', 'error', {
        label: '重试', onClick: () => { void refetchApprovalCount(); },
      });
    }
  }, [approvalCountNetworkError, dismiss, location.pathname, notify, refetchApprovalCount]);

  useEffect(() => () => {
    if (approvalCountErrorToast.current !== null) dismiss(approvalCountErrorToast.current);
  }, [dismiss]);

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

  return <div className={`morde-shell${contextSidebarCollapsed ? ' morde-shell-context-collapsed' : ''}${pageNavigation ? ' morde-shell-with-page-navigation' : ''}`}>
    <a className="skip-link" href="#main-content">跳到主要内容</a>
    <header className="morde-topbar"><div className="morde-top-navigation">
      <button type="button" className="morde-top-control" aria-label="返回上一页" title="返回上一页" disabled={!canGoBack} onClick={goBack}><MordeIcon name="back" /></button>
      <button type="button" className="morde-top-control" aria-label="前进" title="前进" disabled={!canGoForward} onClick={goForward}><MordeIcon name="forward" /></button>
      <span className="morde-top-divider" aria-hidden="true" />
      {hasCollapsibleSidebar && <button type="button" className="morde-top-control" aria-label={contextSidebarCollapsed ? '展开侧边栏' : '折叠侧边栏'} title={contextSidebarCollapsed ? '展开侧边栏' : '折叠侧边栏'} aria-expanded={!contextSidebarCollapsed} aria-controls="context-sidebar" onClick={() => setContextSidebarCollapsed((collapsed) => !collapsed)}><MordeIcon name="panel" /></button>}
    </div><div className="morde-top-actions"><ToastNoticeCenter autoOpenErrors />{isAssistantPage ? <nav className="morde-workspace-tabs" aria-label="对话工作区">
      {([
        ['computer', '云端电脑', 'monitor'], ['tasks', '任务动态', 'tasks'], ['files', '文件', 'folder'],
      ] as const).map(([tab, label, icon]) => <button key={tab} type="button" className={`morde-workspace-tab${assistantPanelOpen && assistantPanelTab === tab ? ' active' : ''}`}
        aria-pressed={assistantPanelOpen && assistantPanelTab === tab} onClick={() => { setAssistantPanelTab(tab); setAssistantPanelOpen(true); }}>
        <MordeIcon name={icon} />{label}{tab === 'computer' && assistantPanelOpen && assistantPanelTab === tab && <span className="morde-workspace-tab-close" aria-hidden="true">×</span>}
      </button>)}
      <button type="button" className="morde-workspace-icon" aria-label="新建旁聊" title="新建旁聊" onClick={() => setAssistantNewChatRequest((request) => request + 1)}><MordeIcon name="plus" /></button>
      <button type="button" className="morde-workspace-icon" aria-label="切换全屏" title="切换全屏" onClick={() => void toggleFullscreen()}><MordeIcon name="fullscreen" /></button>
      <button type="button" className="morde-workspace-icon" aria-label={assistantPanelOpen ? '隐藏工作区侧栏' : '显示工作区侧栏'} title={assistantPanelOpen ? '隐藏工作区侧栏' : '显示工作区侧栏'}
        aria-expanded={assistantPanelOpen} onClick={() => setAssistantPanelOpen((open) => !open)}><MordeIcon name="panel" /></button>
    </nav> : <><Link className="morde-notification-link" to="/clients" aria-label="查看客户端与设备"><MordeIcon name="monitor" /></Link><Link className="morde-notification-link" to="/notifications" aria-label="查看通知"><MordeIcon name="bell" /></Link><span className="morde-avatar" aria-hidden="true">{user?.name.slice(0, 1) || 'M'}</span><span className="morde-top-user">{user?.name}</span></>}</div></header>
    <aside className="morde-sidebar" aria-label="PC 用户端导航">
      <Link to="/assistant" className="morde-brand" aria-label="Morde 对话" title="Morde 对话"><span className="morde-brand-mark" aria-hidden="true">M</span><span><strong>Morde</strong><small>PC 用户端</small></span></Link>
      <nav id="primary-navigation" className="morde-navigation" aria-label="工作导航">
        <span className="morde-nav-heading">工作</span>
        {primaryLinks.map(({ to, label, icon, end }) => {
          const count = approvalCount.data?.pending_count;
          const accessibleLabel = to !== '/reviews' ? label : approvalCount.isError && !approvalCountNetworkError ? '审核状态暂不可用'
            : count === undefined ? '审核' : count > 0 ? `审核，有 ${count} 项待处理` : '审核';
          return <NavLink className={({ isActive }) => `morde-nav-link${isActive ? ' active' : ''}`} to={to} end={end} key={to}
            aria-label={accessibleLabel} title={accessibleLabel}>
            <MordeIcon name={icon} /><span>{label}</span>
            {to === '/reviews' && count !== undefined && count > 0 && <span className="morde-nav-count" aria-hidden="true">{count > 99 ? '99+' : count}</span>}
            {to === '/reviews' && approvalCount.isError && !approvalCountNetworkError && <span className="morde-nav-count morde-nav-count-error" aria-hidden="true">!</span>}
          </NavLink>;
        })}
      </nav>
      <div className="morde-sidebar-bottom">
        <nav className="morde-navigation" aria-label="设置导航">
          <NavLink className={`morde-nav-link${settingLinks.some(({ to }) => to === location.pathname) ? ' active' : ''}`} to={settingsEntry.to} aria-label={settingsEntry.label} title={settingsEntry.label}><MordeIcon name={settingsEntry.icon} /><span>{settingsEntry.label}</span></NavLink>
        </nav>
        <div className="morde-profile">
          <details className="morde-account-menu">
            <summary className="morde-avatar" aria-label="账号菜单" title="账号菜单">{user?.name.slice(0, 1) || 'M'}</summary>
            <div className="morde-account-menu-popover">
              <strong>{user?.name}</strong><small>PC 用户端</small>
              <NavLink to="/settings" className="morde-account-menu-action" aria-label="账户设置" title="账户设置"><MordeIcon name="settings" />账户设置</NavLink>
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
    {pageNavigation && <aside id="context-sidebar" className="page-navigation" aria-label={`${pageNavigation.title}子菜单`}>
      <nav aria-label={`${pageNavigation.title}分类`}>
        {pageNavigation.items.map((item) => {
          const active = matchesPageItem(item, location.pathname, location.search, pageNavigation.queryDefault);
          return <Link key={item.to} className={`page-navigation-link${active ? ' active' : ''}`} aria-current={active ? 'page' : undefined} to={item.to} title={item.label}><MordeIcon name={item.icon} /><span>{item.label}</span></Link>;
        })}
      </nav>
    </aside>}
    <main className="morde-main">
      <div id="main-content" className="morde-page" tabIndex={-1}><Outlet context={{ panelOpen: assistantPanelOpen, panelTab: assistantPanelTab,
        setPanelOpen: setAssistantPanelOpen, setPanelTab: setAssistantPanelTab, newChatRequest: assistantNewChatRequest }} /></div>
    </main>
    {logoutConfirmationOpen && <ConfirmDialog title="退出登录" description="确定要退出 Morde 吗？退出后需要重新登录才能继续使用。" confirmLabel="退出登录" danger onClose={() => setLogoutConfirmationOpen(false)} onConfirm={() => { setLogoutConfirmationOpen(false); void logout(); }} />}
  </div>;
}
