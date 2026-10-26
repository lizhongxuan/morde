import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider, useMutation, useQuery } from '@tanstack/react-query';
import { MemoryRouter, Route, Routes, useLocation } from 'react-router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ApiError } from '../api/client';
import { portalApi } from './api';
import { AuthProvider } from './auth';
import { AdminGuard, RouteGuard } from './RouteGuard';

vi.mock('../api/client', () => {
  class MockApiError extends Error {
    code: string;
    status: number;
    constructor(input: { message: string; code: string; status: number }) { super(input.message); this.code = input.code; this.status = input.status; }
  }
  return { ApiError: MockApiError };
});

vi.mock('./api', () => ({ portalApi: { me: vi.fn(), login: vi.fn(), logout: vi.fn() } }));

describe('RouteGuard', () => {
  afterEach(() => cleanup());

  beforeEach(() => {
    vi.mocked(portalApi.me).mockRejectedValue(new ApiError({ message: '未登录', code: 'unauthenticated', status: 401 }));
  });

  it('未登录时重定向到登录页并保留原始路由', async () => {
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={queryClient}><AuthProvider><MemoryRouter initialEntries={['/client-authorize?user_code=ABCD2345#confirm']}><Routes>
      <Route element={<RouteGuard />}><Route path="/client-authorize" element={<p>受保护页面</p>} /></Route>
      <Route path="/login" element={<LoginLocation />} />
    </Routes></MemoryRouter></AuthProvider></QueryClientProvider>);

    expect(await screen.findByText('登录页面')).toBeTruthy();
    expect(screen.getByText('/client-authorize?user_code=ABCD2345#confirm')).toBeInTheDocument();
    expect(screen.queryByText('受保护页面')).toBeNull();
  });

  it('管理 API 返回 403 后隐藏并清理已缓存的管理数据', async () => {
    vi.mocked(portalApi.me).mockResolvedValue({ id: 'u1234567', name: 'admin', role: 'admin' } as never);
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    function CachedAdminPage() {
      const { data } = useQuery({
        queryKey: ['admin', 'workspaces'],
        queryFn: async () => { throw new ApiError({ message: '权限已撤销', code: 'forbidden', status: 403 }); },
        initialData: [{ id: 'w1234567', name: '旧管理数据' }],
      });
      return <p>{data[0]?.name}</p>;
    }
    render(<QueryClientProvider client={queryClient}><AuthProvider><MemoryRouter initialEntries={['/admin']}><Routes>
      <Route element={<RouteGuard loginPath="/admin/login" />}>
        <Route path="/admin/access-denied" element={<p>没有管理权限</p>} />
        <Route element={<AdminGuard deniedTo="/admin/access-denied" />}><Route path="/admin" element={<CachedAdminPage />} /></Route>
      </Route>
      <Route path="/admin/login" element={<p>管理登录页面</p>} />
    </Routes></MemoryRouter></AuthProvider></QueryClientProvider>);

    expect(await screen.findByText('没有管理权限')).toBeInTheDocument();
    expect(screen.queryByText('旧管理数据')).toBeNull();
    await waitFor(() => expect(queryClient.getQueryData(['admin', 'workspaces'])).toBeUndefined());
  });

  it('管理写操作返回 403 后也隐藏并清理管理数据', async () => {
    vi.mocked(portalApi.me).mockResolvedValue({ id: 'u1234567', name: 'admin', role: 'admin' } as never);
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    queryClient.setQueryData(['admin', 'users'], [{ id: 'u7654321', name: '旧管理数据' }]);
    function MutatingAdminPage() {
      const mutation = useMutation({ mutationFn: async () => { throw new ApiError({ message: '权限已撤销', code: 'forbidden', status: 403 }); } });
      return <><p>旧管理数据</p><button type="button" onClick={() => mutation.mutate()}>模拟管理写入</button></>;
    }
    render(<QueryClientProvider client={queryClient}><AuthProvider><MemoryRouter initialEntries={['/admin']}><Routes>
      <Route element={<RouteGuard loginPath="/admin/login" />}>
        <Route path="/admin/access-denied" element={<p>没有管理权限</p>} />
        <Route element={<AdminGuard deniedTo="/admin/access-denied" />}><Route path="/admin" element={<MutatingAdminPage />} /></Route>
      </Route>
      <Route path="/admin/login" element={<p>管理登录页面</p>} />
    </Routes></MemoryRouter></AuthProvider></QueryClientProvider>);

    fireEvent.click(await screen.findByRole('button', { name: '模拟管理写入' }));
    expect(await screen.findByText('没有管理权限')).toBeInTheDocument();
    expect(screen.queryByText('旧管理数据')).toBeNull();
    await waitFor(() => expect(queryClient.getQueryData(['admin', 'users'])).toBeUndefined());
  });
});

function LoginLocation() {
  return <><p>登录页面</p><p>{(useLocation().state as { from: string }).from}</p></>;
}
