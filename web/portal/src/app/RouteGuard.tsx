import { useEffect, useRef } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { Navigate, Outlet, useLocation } from 'react-router';
import { ApiError } from '../api/client';
import { RecoverableError } from '../components/RecoverableError';
import { portalApi } from './api';
import { useAuth } from './auth';

export function RouteGuard({ loginPath = '/login' }: { loginPath?: string }) {
  const { user, loading, error, refresh } = useAuth();
  const location = useLocation();
  if (loading) return <main className="loading-page" aria-live="polite">正在检查登录状态…</main>;
  if (error && !user) return <RecoverableError title="暂时无法连接服务" message="登录状态暂时无法确认。请检查连接后重试，或返回上一页。" homePath={loginPath} homeLabel="前往登录" onRetry={() => { void refresh(); }} />;
  if (!user) return <Navigate to={loginPath} replace state={{ from: `${location.pathname}${location.search}${location.hash}` }} />;
  if (user.must_change_password && loginPath === '/login') return <Navigate to="/settings/password" replace />;
  return <Outlet />;
}

export function AdminGuard({ deniedTo = '/assistant' }: { deniedTo?: string }) {
  const { user } = useAuth();
  const queryClient = useQueryClient();
  const permissionCheckInFlight = useRef(false);
  useEffect(() => {
    const verifyRoleAfter403 = (error: unknown) => {
      if (!(error instanceof ApiError && error.status === 403) || permissionCheckInFlight.current) return;
      permissionCheckInFlight.current = true;
      void portalApi.me()
        .then((freshUser) => queryClient.setQueryData(['auth', 'me'], freshUser))
        .catch(() => undefined)
        .finally(() => { permissionCheckInFlight.current = false; });
    };
    const stopQueries = queryClient.getQueryCache().subscribe((event) => {
      if (event.type === 'updated' && event.action.type === 'error' && event.query.queryKey[0] === 'admin') {
        verifyRoleAfter403(event.query.state.error);
      }
    });
    const stopMutations = queryClient.getMutationCache().subscribe((event) => {
      if (event.type === 'updated' && event.action.type === 'error') verifyRoleAfter403(event.mutation.state.error);
    });
    return () => { stopQueries(); stopMutations(); };
  }, [queryClient]);
  useEffect(() => {
    if (!user || user.must_change_password || user.role === 'admin' || user.role === 'super_admin') return;
    void queryClient.cancelQueries({ queryKey: ['admin'] }).then(() => queryClient.removeQueries({ queryKey: ['admin'] }));
  }, [queryClient, user]);
  if (user?.must_change_password) return <Navigate to="/admin/change-password" replace />;
  if (user?.role !== 'admin' && user?.role !== 'super_admin') return <Navigate to={deniedTo} replace />;
  return <Outlet />;
}
