import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { BrowserRouter, Navigate, Route, Routes, useLocation } from 'react-router';
import type { ReactNode } from 'react';
import { AuthProvider } from './app/auth';
import { AdminGuard, RouteGuard } from './app/RouteGuard';
import { ToastNoticeCenter, ToastProvider } from './app/toast';
import { PortalLayout, PortalNavigationHistoryProvider } from './components/PortalLayout';
import { AccountSettings } from './routes/AccountSettings';
import { GoalsAndSchedules } from './routes/GoalsAndSchedules';
import { Assistant } from './routes/Assistant';
import { Connections } from './routes/Connections';
import { Clients } from './routes/Clients';
import { Downloads } from './routes/Downloads';
import { Hosts } from './routes/Hosts';
import { Workspaces } from './routes/Workspaces';
import { HostEntry } from './routes/HostEntry';
import { Login } from './routes/Login';
import { Register } from './routes/Register';
import { ChangePassword } from './routes/ChangePassword';
import { NewHost } from './routes/NewHost';
import { Notifications } from './routes/Notifications';
import { PersonalData } from './routes/PersonalData';
import { Projects } from './routes/Projects';
import { Codex } from './routes/Codex';
import { Reviews } from './routes/Reviews';
import { BlindEvaluationWorkspace } from './routes/BlindEvaluationWorkspace';
import { AdminAudit } from './routes/admin/AdminAudit';
import { AdminHosts } from './routes/admin/AdminHosts';
import { AdminLayout } from './routes/admin/AdminLayout';
import { AdminUsers } from './routes/admin/AdminUsers';
import { AdminOverview } from './routes/admin/AdminOverview';
import { AdminModels } from './routes/admin/AdminModels';
import { AdminUsage } from './routes/admin/AdminUsage';
import { AdminObservability } from './routes/admin/AdminObservability';
import { AdminQuality } from './routes/admin/AdminQuality';
import { AdminControls } from './routes/admin/AdminControls';
import { AdminRecords } from './routes/admin/AdminRecords';
import { AdminHostDetail } from './routes/admin/AdminHostDetail';
import { AdminUserDetail } from './routes/admin/AdminUserDetail';
import { AdminSettings } from './routes/admin/AdminSettings';
import { AdminEvaluationBatches } from './routes/admin/AdminEvaluationBatches';
import { RecoverablePageBoundary } from './app/PageErrorBoundary';
import { RecoverableError } from './components/RecoverableError';

const queryClient = new QueryClient({ defaultOptions: { queries: { retry: 1, refetchOnWindowFocus: true } } });

function NotFound() {
  return <RecoverableError title="找不到这个页面" message="地址可能已经失效，或页面暂时无法访问。" />;
}

function AdminAccessDenied() {
  return <RecoverableError title="没有管理权限" message="此账号不能查看管理后台数据。" homePath="/admin/login" homeLabel="切换管理员账号" />;
}

function projectSyncSearch(search: string) {
  const host = new URLSearchParams(search).get('host') ?? '';
  const params = new URLSearchParams({ sync: '1' });
  if (/^h[a-z0-9]{7}$/.test(host)) params.set('host', host);
  const query = params.toString();
  return query ? `?${query}` : '';
}

function LegacyLocalProjectsRedirect() {
  const location = useLocation();
  return <Navigate to={{ pathname: '/hosts', search: projectSyncSearch(location.search) }} replace />;
}

function LegacyWorkspaceRedirect() {
  const location = useLocation();
  return <Navigate to={{ pathname: '/hosts', search: projectSyncSearch(location.search) }} replace />;
}

function LegacySettingsSyncRedirect() {
  const location = useLocation();
  return <Navigate to={{ pathname: '/hosts', search: projectSyncSearch(location.search) }} replace />;
}

function HostsRoute() {
  const location = useLocation();
  if (new URLSearchParams(location.search).get('sync') === '1') {
    return <Workspaces projectSyncOnly />;
  }
  return <Hosts />;
}

function LegacyHostSyncRedirect() {
  const location = useLocation();
  return <Navigate to={{ pathname: '/hosts', search: projectSyncSearch(location.search) }} replace />;
}

function StandaloneToastPage({ children }: { children: ReactNode }) {
  return <><ToastNoticeCenter autoOpenErrors overlay />{children}</>;
}

function UserRoutes() {
  return <Routes>
    <Route path="/login" element={<StandaloneToastPage><Login /></StandaloneToastPage>} />
    <Route path="/register" element={<StandaloneToastPage><Register /></StandaloneToastPage>} />
    <Route path="/download" element={<StandaloneToastPage><Downloads /></StandaloneToastPage>} />
    <Route path="/settings/password" element={<StandaloneToastPage><ChangePassword loginPath="/login" cancelPath="/settings" /></StandaloneToastPage>} />
    <Route element={<RouteGuard />}>
      <Route path="host-entry/:id" element={<StandaloneToastPage><HostEntry /></StandaloneToastPage>} />
      <Route path="evaluation-batches/:pack_id/review" element={<StandaloneToastPage><BlindEvaluationWorkspace /></StandaloneToastPage>} />
      <Route path="evaluation-batches/:pack_id/arbitration" element={<StandaloneToastPage><BlindEvaluationWorkspace /></StandaloneToastPage>} />
      <Route element={<PortalLayout />}>
        <Route index element={<Navigate to="/assistant" replace />} />
        <Route path="assistant" element={<Assistant />} />
        <Route path="reviews" element={<Reviews />} />
        <Route path="projects" element={<Projects />} />
        <Route path="local-projects" element={<LegacyLocalProjectsRedirect />} />
        <Route path="codex" element={<Codex />} />
        <Route path="personal" element={<PersonalData />} />
        <Route path="goals" element={<GoalsAndSchedules />} />
        <Route path="hosts" element={<HostsRoute />} />
        <Route path="hosts/sync" element={<LegacyHostSyncRedirect />} />
        <Route path="hosts/new" element={<NewHost />} />
        <Route path="notifications" element={<Notifications />} />
        <Route path="workspaces" element={<LegacyWorkspaceRedirect />} />
        <Route path="connections" element={<Connections />} />
        <Route path="apikeys" element={<Navigate to="/hosts?view=apikeys" replace />} />
        <Route path="clients" element={<Clients />} />
        <Route path="settings" element={<AccountSettings />} />
        <Route path="settings/sync" element={<LegacySettingsSyncRedirect />} />
      </Route>
    </Route>
    <Route path="*" element={<NotFound />} />
  </Routes>;
}

function AdminRoutes() {
  return <div className="morde-admin-app"><Routes>
    <Route path="/admin/login" element={<StandaloneToastPage><Login defaultTo="/admin/" allowRegister={false} /></StandaloneToastPage>} />
    <Route path="/admin/change-password" element={<StandaloneToastPage><ChangePassword loginPath="/admin/login" /></StandaloneToastPage>} />
    <Route element={<RouteGuard loginPath="/admin/login" />}>
      <Route path="/admin/access-denied" element={<AdminAccessDenied />} />
      <Route element={<AdminGuard deniedTo="/admin/access-denied" />}>
        <Route path="/admin" element={<AdminLayout />}>
          <Route index element={<AdminOverview />} />
          <Route path="users" element={<AdminUsers />} />
          <Route path="users/:id/records" element={<AdminRecords />} />
          <Route path="users/:id" element={<AdminUserDetail />} />
          <Route path="settings" element={<AdminSettings />} />
          <Route path="models" element={<AdminModels />} />
          <Route path="usage" element={<AdminUsage />} />
          <Route path="observability" element={<AdminObservability />} />
          <Route path="quality" element={<AdminQuality />} />
          <Route path="evaluations" element={<AdminEvaluationBatches />} />
          <Route path="controls" element={<AdminControls />} />
          <Route path="hosts" element={<AdminHosts />} />
          <Route path="hosts/:id" element={<AdminHostDetail />} />
          <Route path="audit" element={<AdminAudit />} />
        </Route>
      </Route>
    </Route>
    <Route path="*" element={<RecoverableError title="管理后台地址无效" message="地址可能已经失效。可以返回上一页，或回到管理后台首页。" homePath="/admin" homeLabel="返回管理后台" />} />
  </Routes></div>;
}

export default function App() {
  return <QueryClientProvider client={queryClient}>
    <ToastProvider>
      <BrowserRouter><PortalNavigationHistoryProvider><RecoverablePageBoundary homePath="/assistant" homeLabel="返回助手"><AuthProvider><UserRoutes /></AuthProvider></RecoverablePageBoundary></PortalNavigationHistoryProvider></BrowserRouter>
    </ToastProvider>
  </QueryClientProvider>;
}

export function AdminApp() {
  return <QueryClientProvider client={queryClient}>
    <ToastProvider>
      <BrowserRouter><RecoverablePageBoundary homePath="/admin" homeLabel="返回管理后台"><AuthProvider><AdminRoutes /></AuthProvider></RecoverablePageBoundary></BrowserRouter>
    </ToastProvider>
  </QueryClientProvider>;
}
