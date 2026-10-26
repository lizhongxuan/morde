import { useEffect, useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { Navigate, Outlet, useLocation } from 'react-router';
import { ApiError } from '../api/client';
import { useAuth } from './auth';

export function RouteGuard({ loginPath = '/login' }: { loginPath?: string }) {
  const { user, loading, error, refresh } = useAuth();
  const location = useLocation();
  if (loading) return <main className="loading-page" aria-live="polite">正在检查登录状态…</main>;
  if (error) return <main className="loading-page"><div className="error-card" role="alert">{error}<button className="button button-secondary" type="button" onClick={() => { void refresh(); }}>重试</button></div></main>;
  if (!user) return <Navigate to={loginPath} replace state={{ from: `${location.pathname}${location.search}${location.hash}` }} />;
  return <Outlet />;
}

export function AdminGuard({ deniedTo = '/assistant' }: { deniedTo?: string }) {
  const { user, refresh } = useAuth();
  const queryClient = useQueryClient();
  const [revoked, setRevoked] = useState(false);
  useEffect(() => {
    const revokeOn403 = (error: unknown) => {
      if (error instanceof ApiError && error.status === 403) setRevoked(true);
    };
    const stopQueries = queryClient.getQueryCache().subscribe((event) => {
      if (event.type === 'updated' && event.action.type === 'error' && event.query.queryKey[0] === 'admin') {
        revokeOn403(event.query.state.error);
      }
    });
    const stopMutations = queryClient.getMutationCache().subscribe((event) => {
      if (event.type === 'updated' && event.action.type === 'error') revokeOn403(event.mutation.state.error);
    });
    return () => { stopQueries(); stopMutations(); };
  }, [queryClient]);
  useEffect(() => {
    if (!revoked) return;
    void queryClient.cancelQueries({ queryKey: ['admin'] }).then(() => queryClient.removeQueries({ queryKey: ['admin'] }));
    void refresh();
  }, [queryClient, refresh, revoked]);
  if (revoked || user?.role !== 'admin') return <Navigate to={deniedTo} replace />;
  return <Outlet />;
}
