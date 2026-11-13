import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { QueryClient, QueryClientProvider, useMutation, useQuery } from '@tanstack/react-query';
import { MemoryRouter, Route, Routes, useLocation } from 'react-router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ApiError } from '../api/client';
import { portalApi } from './api';
import { AuthProvider, useAuth } from './auth';
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
    render(<QueryClientProvider client={queryClient}><AuthProvider><MemoryRouter initialEntries={['/assistant?view=recent#chat']}><Routes>
      <Route element={<RouteGuard />}><Route path="/assistant" element={<p>受保护页面</p>} /></Route>
      <Route path="/login" element={<LoginLocation />} />
    </Routes></MemoryRouter></AuthProvider></QueryClientProvider>);

    expect(await screen.findByText('登录页面')).toBeTruthy();
    expect(screen.getByText('/assistant?view=recent#chat')).toBeInTheDocument();
    expect(screen.queryByText('受保护页面')).toBeNull();
  });

  it('首次登录用户端时引导修改密码', async () => {
    vi.mocked(portalApi.me).mockResolvedValue({ id: 'u1234567', name: 'admin', role: 'super_admin', must_change_password: true } as never);
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={queryClient}><AuthProvider><MemoryRouter initialEntries={['/assistant']}><Routes>
      <Route element={<RouteGuard />}><Route path="/assistant" element={<p>助手页面</p>} /></Route>
      <Route path="/settings/password" element={<p>修改密码页面</p>} />
    </Routes></MemoryRouter></AuthProvider></QueryClientProvider>);

    expect(await screen.findByText('修改密码页面')).toBeInTheDocument();
    expect(screen.queryByText('助手页面')).toBeNull();
  });

  it('保留已确认登录的页面处理临时网络错误，并在服务确认401后回到登录页', async () => {
    vi.mocked(portalApi.me)
      .mockRejectedValueOnce(new TypeError('Failed to fetch'))
      .mockRejectedValueOnce(new ApiError({ message: '未登录', code: 'unauthenticated', status: 401 }));
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    queryClient.setQueryData(['auth', 'me'], { id: 'u1234567', name: 'demo', role: 'member' });
    function RefreshAuth() {
      const { refresh } = useAuth();
      return <button type="button" onClick={() => { void refresh(); }}>刷新登录状态</button>;
    }
    const user = userEvent.setup();
    render(<QueryClientProvider client={queryClient}><AuthProvider><MemoryRouter initialEntries={['/assistant']}><Routes>
      <Route element={<RouteGuard />}><Route path="/assistant" element={<><p>当前对话仍可使用</p><RefreshAuth /></>} /></Route>
      <Route path="/login" element={<LoginLocation />} />
    </Routes></MemoryRouter></AuthProvider></QueryClientProvider>);

    await user.click(screen.getByRole('button', { name: '刷新登录状态' }));
    await waitFor(() => expect(portalApi.me).toHaveBeenCalledTimes(1));
    expect(await screen.findByText('当前对话仍可使用')).toBeInTheDocument();
    expect(screen.queryByRole('heading', { name: '暂时无法连接服务' })).toBeNull();

    await user.click(screen.getByRole('button', { name: '刷新登录状态' }));
    expect(await screen.findByText('登录页面')).toBeInTheDocument();
    expect(screen.getByText('/assistant')).toBeInTheDocument();
    expect(screen.queryByText('当前对话仍可使用')).toBeNull();
  });

  it('管理 API 单项返回 403 时留在当前页并显示页面错误', async () => {
    vi.mocked(portalApi.me).mockResolvedValue({ id: 'u1234567', name: 'admin', role: 'admin' } as never);
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    function CachedAdminPage() {
      const query = useQuery({
        queryKey: ['admin', 'workspaces'],
        queryFn: async () => { throw new ApiError({ message: '权限已撤销', code: 'forbidden', status: 403 }); },
        initialData: [{ id: 'w1234567', name: '旧管理数据' }],
      });
      return <p>{query.isError ? '此项数据暂时无法读取' : query.data[0]?.name}</p>;
    }
    render(<QueryClientProvider client={queryClient}><AuthProvider><MemoryRouter initialEntries={['/admin']}><Routes>
      <Route element={<RouteGuard loginPath="/admin/login" />}>
        <Route path="/admin/access-denied" element={<p>没有管理权限</p>} />
        <Route element={<AdminGuard deniedTo="/admin/access-denied" />}><Route path="/admin" element={<CachedAdminPage />} /></Route>
      </Route>
      <Route path="/admin/login" element={<p>管理登录页面</p>} />
    </Routes></MemoryRouter></AuthProvider></QueryClientProvider>);

    expect(await screen.findByText('此项数据暂时无法读取')).toBeInTheDocument();
    expect(screen.queryByText('没有管理权限')).toBeNull();
    expect(queryClient.getQueryData(['admin', 'workspaces'])).toEqual([{ id: 'w1234567', name: '旧管理数据' }]);
  });

  it('仅在复核发现管理员角色已撤销后跳转并清理管理数据', async () => {
    vi.mocked(portalApi.me)
      .mockResolvedValueOnce({ id: 'u1234567', name: 'admin', role: 'admin' } as never)
      .mockResolvedValueOnce({ id: 'u1234567', name: 'admin', role: 'user' } as never);
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

  it('super admin 首次登录必须先改密', async () => {
    vi.mocked(portalApi.me).mockResolvedValue({ id: 'u1234567', name: 'admin', role: 'super_admin', must_change_password: true } as never);
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={queryClient}><AuthProvider><MemoryRouter initialEntries={['/admin']}><Routes>
      <Route element={<RouteGuard loginPath="/admin/login" />}>
        <Route path="/admin/change-password" element={<p>修改密码页面</p>} />
        <Route element={<AdminGuard deniedTo="/admin/access-denied" />}><Route path="/admin" element={<p>管理后台数据</p>} /></Route>
      </Route>
      <Route path="/admin/login" element={<p>管理登录页面</p>} />
    </Routes></MemoryRouter></AuthProvider></QueryClientProvider>);

    expect(await screen.findByText('修改密码页面')).toBeInTheDocument();
    expect(screen.queryByText('管理后台数据')).toBeNull();
  });

  it('super admin 改密后可进入管理后台', async () => {
    vi.mocked(portalApi.me).mockResolvedValue({ id: 'u1234567', name: 'admin', role: 'super_admin', must_change_password: false } as never);
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={queryClient}><AuthProvider><MemoryRouter initialEntries={['/admin']}><Routes>
      <Route element={<RouteGuard loginPath="/admin/login" />}>
        <Route path="/admin/change-password" element={<p>修改密码页面</p>} />
        <Route element={<AdminGuard deniedTo="/admin/access-denied" />}><Route path="/admin" element={<p>管理后台数据</p>} /></Route>
        <Route path="/admin/access-denied" element={<p>没有管理权限</p>} />
      </Route>
      <Route path="/admin/login" element={<p>管理登录页面</p>} />
    </Routes></MemoryRouter></AuthProvider></QueryClientProvider>);

    expect(await screen.findByText('管理后台数据')).toBeInTheDocument();
    expect(screen.queryByText('没有管理权限')).toBeNull();
  });
});

function LoginLocation() {
  return <><p>登录页面</p><p>{(useLocation().state as { from: string }).from}</p></>;
}
