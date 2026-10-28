import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { BrowserRouter, Link, Navigate, Route, Routes } from 'react-router';
import { AuthProvider } from './app/auth';
import { AdminGuard, RouteGuard } from './app/RouteGuard';
import { ToastProvider } from './app/toast';
import { PortalLayout } from './components/PortalLayout';
import { ApiKeys } from './routes/ApiKeys';
import { Assistant } from './routes/Assistant';
import { Connections } from './routes/Connections';
import { Clients } from './routes/Clients';
import { Downloads } from './routes/Downloads';
import { Hosts } from './routes/Hosts';
import { HostEntry } from './routes/HostEntry';
import { Login } from './routes/Login';
import { Register } from './routes/Register';
import { ChangePassword } from './routes/ChangePassword';
import { NewHost } from './routes/NewHost';
import { Notifications } from './routes/Notifications';
import { Workspaces } from './routes/Workspaces';
import { AdminAudit } from './routes/admin/AdminAudit';
import { AdminCapacity } from './routes/admin/AdminCapacity';
import { AdminHosts } from './routes/admin/AdminHosts';
import { AdminLayout } from './routes/admin/AdminLayout';
import { AdminUsers } from './routes/admin/AdminUsers';
import { AdminHostDetail } from './routes/admin/AdminHostDetail';
import { AdminUserDetail } from './routes/admin/AdminUserDetail';
import { AdminWorkspaces } from './routes/admin/AdminWorkspaces';
import { AdminWorkspaceDetail } from './routes/admin/AdminWorkspaceDetail';

const queryClient = new QueryClient({ defaultOptions: { queries: { retry: 1, refetchOnWindowFocus: true } } });

function NotFound() {
  return <main className="empty-card not-found"><h1>找不到这个页面</h1><p>地址可能已经失效，或内容已被移除。</p><Link className="button button-primary" to="/assistant">返回助手</Link></main>;
}

function AdminAccessDenied() {
  return <main className="loading-page"><section className="empty-card"><h1>没有管理权限</h1><p>此账号不能查看管理后台数据。</p><a className="button button-primary" href="/">返回 PC 用户端</a></section></main>;
}

function UserRoutes() {
  return <Routes>
    <Route path="/login" element={<Login />} />
    <Route path="/register" element={<Register />} />
    <Route path="/download" element={<Downloads />} />
    <Route element={<RouteGuard />}>
      <Route path="host-entry/:id" element={<HostEntry />} />
      <Route element={<PortalLayout />}>
        <Route index element={<Navigate to="/hosts" replace />} />
        <Route path="assistant" element={<Assistant />} />
        <Route path="hosts" element={<Hosts />} />
        <Route path="hosts/new" element={<NewHost />} />
        <Route path="notifications" element={<Notifications />} />
        <Route path="workspaces" element={<Workspaces />} />
        <Route path="connections" element={<Connections />} />
        <Route path="apikeys" element={<ApiKeys />} />
        <Route path="clients" element={<Clients />} />
      </Route>
    </Route>
    <Route path="*" element={<NotFound />} />
  </Routes>;
}

function AdminRoutes() {
  return <Routes>
    <Route path="/admin/login" element={<Login defaultTo="/admin/" allowRegister={false} />} />
    <Route path="/admin/change-password" element={<ChangePassword />} />
    <Route element={<RouteGuard loginPath="/admin/login" />}>
      <Route path="/admin/access-denied" element={<AdminAccessDenied />} />
      <Route element={<AdminGuard deniedTo="/admin/access-denied" />}>
        <Route path="/admin" element={<AdminLayout />}>
          <Route index element={<AdminCapacity />} />
          <Route path="users" element={<AdminUsers />} />
          <Route path="users/:id" element={<AdminUserDetail />} />
          <Route path="hosts" element={<AdminHosts />} />
          <Route path="hosts/:id" element={<AdminHostDetail />} />
          <Route path="workspaces" element={<AdminWorkspaces />} />
          <Route path="workspaces/:id" element={<AdminWorkspaceDetail />} />
          <Route path="audit" element={<AdminAudit />} />
        </Route>
      </Route>
    </Route>
    <Route path="*" element={<main className="loading-page"><section className="empty-card"><h1>管理后台地址无效</h1><p>请从 Morde PC 用户端重新打开管理后台。</p><a className="button button-primary" href="/">返回用户端</a></section></main>} />
  </Routes>;
}

export default function App() {
  return <QueryClientProvider client={queryClient}>
    <ToastProvider>
      <AuthProvider>
        <BrowserRouter><UserRoutes /></BrowserRouter>
      </AuthProvider>
    </ToastProvider>
  </QueryClientProvider>;
}

export function AdminApp() {
  return <QueryClientProvider client={queryClient}>
    <ToastProvider>
      <AuthProvider>
        <BrowserRouter><AdminRoutes /></BrowserRouter>
      </AuthProvider>
    </ToastProvider>
  </QueryClientProvider>;
}
