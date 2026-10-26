import { createContext, useCallback, useContext, useMemo } from 'react';
import type { ReactNode } from 'react';
import { useQuery, useQueryClient, type QueryClient } from '@tanstack/react-query';
import { ApiError } from '../api/client';
import { portalApi } from './api';
import type { PortalUser } from './types';

interface AuthContextValue {
  user: PortalUser | null;
  loading: boolean;
  error: string | null;
  signIn: (name: string, password: string) => Promise<void>;
  signOut: () => Promise<void>;
  refresh: () => Promise<void>;
}
const AuthContext = createContext<AuthContextValue | null>(null);

async function clearUserQueries(queryClient: QueryClient) {
  const isUserQuery = (query: { queryKey: readonly unknown[] }) => query.queryKey[0] !== 'auth' || query.queryKey[1] !== 'me';
  await queryClient.cancelQueries({ predicate: isUserQuery });
  queryClient.removeQueries({ predicate: isUserQuery });
}

export function AuthProvider({ children }: { children: ReactNode }) {
  const queryClient = useQueryClient();
  const { data, isPending, error: queryError, refetch } = useQuery<PortalUser | null>({
    queryKey: ['auth', 'me'],
    queryFn: () => portalApi.me(),
    retry: false,
    staleTime: Infinity,
  });
  const user = data ?? null;
  const loading = isPending;
  const error = queryError && !(queryError instanceof ApiError && queryError.status === 401)
    ? queryError.message || '无法连接服务，请稍后重试。'
    : null;

  const refresh = useCallback(async () => { await refetch(); }, [refetch]);

  const signIn = useCallback(async (name: string, password: string) => {
    const result = await portalApi.login(name, password);
    await clearUserQueries(queryClient);
    queryClient.setQueryData(['auth', 'me'], result.user);
  }, [queryClient]);

  const signOut = useCallback(async () => {
    await portalApi.logout();
    await clearUserQueries(queryClient);
    queryClient.setQueryData(['auth', 'me'], null);
  }, [queryClient]);

  const value = useMemo(() => ({ user, loading, error, signIn, signOut, refresh }), [user, loading, error, signIn, signOut, refresh]);
  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}

export function useAuth() {
  const value = useContext(AuthContext);
  if (!value) throw new Error('useAuth 必须在 AuthProvider 内使用');
  return value;
}
