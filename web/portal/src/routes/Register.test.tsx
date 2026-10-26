import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter, Route, Routes } from 'react-router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { portalApi } from '../app/api';
import { AuthProvider } from '../app/auth';
import { Login } from './Login';
import { Register } from './Register';

vi.mock('../app/api', () => ({
  portalApi: { me: vi.fn(), login: vi.fn(), register: vi.fn() },
  errorDetails: (reason: Error & { code?: string }) => ({ code: reason.code, message: reason.message }),
}));

function renderRoute(path: string) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(['auth', 'me'], null);
  render(<QueryClientProvider client={client}><AuthProvider><MemoryRouter initialEntries={[path]}><Routes>
    <Route path="/register" element={<Register />} />
    <Route path="/login" element={<Login />} />
    <Route path="/admin/login" element={<Login defaultTo="/admin/" allowRegister={false} />} />
  </Routes></MemoryRouter></AuthProvider></QueryClientProvider>);
  return client;
}

describe('registration page', () => {
  beforeEach(() => vi.clearAllMocks());
  afterEach(cleanup);

  it('registers a user, then asks for login without creating a session', async () => {
    vi.mocked(portalApi.register).mockResolvedValueOnce({ user: { id: 'u1234567', name: 'new-user', role: 'user', status: 'pending' } } as never);
    const client = renderRoute('/register');
    const user = userEvent.setup();
    await user.type(screen.getByRole('textbox', { name: '用户名' }), 'new-user');
    await user.type(screen.getByLabelText('密码', { exact: true }), 'secret-pass');
    await user.type(screen.getByLabelText('确认密码'), 'secret-pass');
    await user.click(screen.getByRole('button', { name: '注册' }));

    await waitFor(() => expect(portalApi.register).toHaveBeenCalledWith('new-user', 'secret-pass'));
    expect(await screen.findByRole('status')).toHaveTextContent('申请已提交，等待管理员审核通过后才能登录。');
    expect(portalApi.login).not.toHaveBeenCalled();
    expect(client.getQueryData(['auth', 'me'])).toBeNull();
  });

  it('shows form and conflict errors without submitting a mismatched password', async () => {
    const user = userEvent.setup();
    renderRoute('/register');
    await user.type(screen.getByRole('textbox', { name: '用户名' }), 'new-user');
    await user.type(screen.getByLabelText('密码', { exact: true }), 'secret-pass');
    await user.type(screen.getByLabelText('确认密码'), 'other-pass');
    await user.click(screen.getByRole('button', { name: '注册' }));
    expect(screen.getByRole('alert')).toHaveTextContent('两次输入的密码不一致');
    expect(portalApi.register).not.toHaveBeenCalled();

    vi.mocked(portalApi.register).mockRejectedValueOnce(Object.assign(new Error('用户名已被使用'), { code: 'conflict' }));
    await user.clear(screen.getByLabelText('确认密码'));
    await user.type(screen.getByLabelText('确认密码'), 'secret-pass');
    await user.click(screen.getByRole('button', { name: '注册' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('用户名已被使用，请更换用户名。');
  });

  it('keeps registration off the administrator login page', () => {
    renderRoute('/admin/login');
    expect(screen.queryByRole('link', { name: '注册账号' })).not.toBeInTheDocument();
  });

  it.each([
    ['account_pending', '账号正在等待管理员审核，通过后才能登录。'],
    ['account_rejected', '账号审核未通过，请联系管理员。'],
    ['account_disabled', '账号已被禁用，请联系管理员。'],
  ])('shows the server account status for %s', async (code, message) => {
    vi.mocked(portalApi.login).mockRejectedValueOnce(Object.assign(new Error('account unavailable'), { code }));
    const user = userEvent.setup();
    renderRoute('/login');
    await user.type(screen.getByRole('textbox', { name: '用户名' }), 'new-user');
    await user.type(screen.getByLabelText('密码', { exact: true }), 'secret-pass');
    await user.click(screen.getByRole('button', { name: '登录' }));

    expect(await screen.findByRole('alert')).toHaveTextContent(message);
    expect(screen.getByRole('heading', { name: '登录' })).toBeInTheDocument();
  });

  it('does not reveal an account status when credentials are invalid', async () => {
    vi.mocked(portalApi.login).mockRejectedValueOnce(Object.assign(new Error('invalid credentials'), { code: 'unauthenticated' }));
    const user = userEvent.setup();
    renderRoute('/login');
    await user.type(screen.getByRole('textbox', { name: '用户名' }), 'unknown-user');
    await user.type(screen.getByLabelText('密码', { exact: true }), 'wrong-password');
    await user.click(screen.getByRole('button', { name: '登录' }));

    expect(await screen.findByRole('alert')).toHaveTextContent('用户名或密码错误，请重试。');
    expect(screen.getByRole('alert')).not.toHaveTextContent(/审核|禁用/);
  });
});
