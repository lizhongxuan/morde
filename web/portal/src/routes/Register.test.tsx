import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter, Route, Routes } from 'react-router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { portalApi } from '../app/api';
import { AuthProvider } from '../app/auth';
import { ToastNoticeCenter, ToastProvider } from '../app/toast';
import { Login } from './Login';
import { Register } from './Register';

vi.mock('../app/api', () => ({
  portalApi: { me: vi.fn(), login: vi.fn(), register: vi.fn() },
  errorDetails: (reason: Error & { code?: string }) => ({ code: reason.code, message: reason.message }),
}));

function renderRoute(path: string, currentUser: unknown = null) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(['auth', 'me'], currentUser);
  render(<ToastProvider><ToastNoticeCenter autoOpenErrors overlay /><QueryClientProvider client={client}><AuthProvider><MemoryRouter initialEntries={[path]}><Routes>
    <Route path="/register" element={<Register />} />
    <Route path="/login" element={<Login />} />
    <Route path="/assistant" element={<p>助手页面</p>} />
    <Route path="/admin/login" element={<Login defaultTo="/admin/" allowRegister={false} />} />
  </Routes></MemoryRouter></AuthProvider></QueryClientProvider></ToastProvider>);
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

  it('shows a network error after registration is submitted and keeps the form values', async () => {
    vi.mocked(portalApi.register).mockRejectedValueOnce(new TypeError('Failed to fetch'));
    renderRoute('/register');
    const user = userEvent.setup();
    const name = screen.getByRole('textbox', { name: '用户名' });
    const password = screen.getByLabelText('密码', { exact: true });
    const confirmation = screen.getByLabelText('确认密码');
    await user.type(name, 'new-user');
    await user.type(password, 'secret-pass');
    await user.type(confirmation, 'secret-pass');
    await user.click(screen.getByRole('button', { name: '注册' }));

    const notice = await screen.findByRole('alert');
    expect(notice).toHaveTextContent('网络异常，注册结果暂时无法确认。');
    expect(notice.closest('.login-card')).toBeNull();
    expect(name).toHaveValue('new-user');
    expect(password).toHaveValue('secret-pass');
    expect(confirmation).toHaveValue('secret-pass');
  });

  it('shows a login network error only after submit and preserves credentials', async () => {
    vi.mocked(portalApi.login).mockRejectedValueOnce(new TypeError('Failed to fetch'));
    renderRoute('/login');
    const user = userEvent.setup();
    const name = screen.getByRole('textbox', { name: '用户名' });
    const password = screen.getByLabelText('密码');
    await user.type(name, 'local-user');
    await user.type(password, 'secret-pass');
    await user.click(screen.getByRole('button', { name: '登录' }));

    const notice = await screen.findByRole('alert');
    expect(notice).toHaveTextContent('网络异常，登录状态暂时无法确认。');
    expect(notice.closest('.login-card')).toBeNull();
    expect(name).toHaveValue('local-user');
    expect(password).toHaveValue('secret-pass');
  });

  it('asks only for a username and password', () => {
    renderRoute('/register');
    expect(screen.getByText('提交注册申请后，管理员审核通过即可在 Morde PC 和 iOS 客户端登录。')).toBeInTheDocument();
    expect(screen.getByLabelText('用户名')).toBeInTheDocument();
    expect(screen.getByLabelText('密码', { exact: true })).toBeInTheDocument();
    expect(screen.getByLabelText('确认密码')).toBeInTheDocument();
    expect(screen.queryByLabelText(/邀请码|短信|手机号/)).not.toBeInTheDocument();
  });

  it('uses the Morde logo on the standalone login page without the conversation shell', () => {
    renderRoute('/login');
    expect(document.querySelector('.login-brand .morde-brand-mark')).toBeInTheDocument();
    expect(document.querySelector('.morde-shell')).not.toBeInTheDocument();
  });

  it('keeps password validation and API errors beside the registration form', async () => {
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
    await waitFor(() => expect(screen.getByRole('alert')).toHaveTextContent('用户名已被使用，请更换用户名。'));
    expect(screen.getByRole('alert').closest('form')).toBeInTheDocument();
  });

  it('keeps registration off the administrator login page', () => {
    renderRoute('/admin/login');
    expect(screen.queryByRole('link', { name: '注册账号' })).not.toBeInTheDocument();
    expect(screen.queryByRole('link', { name: '下载 morde-pc' })).not.toBeInTheDocument();
  });

  it('lets a non-admin switch accounts from the administrator login page', () => {
    renderRoute('/admin/login', { id: 'u1234567', name: 'user', role: 'user' });
    expect(screen.getByText('当前账号没有管理权限，请使用管理员账号登录。')).toBeInTheDocument();
    expect(screen.getByRole('textbox', { name: '用户名' })).toBeInTheDocument();
  });

  it('keeps the PC download link on the user login page', () => {
    renderRoute('/login');
    expect(screen.getByRole('link', { name: '下载 morde-pc' })).toHaveAttribute('href', '/download');
  });

  it('ignores a backslash-based cross-origin login return address', async () => {
    vi.mocked(portalApi.login).mockResolvedValueOnce({ user: { id: 'u1234567', name: 'new-user', role: 'user' } } as never);
    const user = userEvent.setup();
    renderRoute('/login?next=%2F%5Cattacker.example');
    await user.type(screen.getByRole('textbox', { name: '用户名' }), 'new-user');
    await user.type(screen.getByLabelText('密码', { exact: true }), 'secret-pass');
    await user.click(screen.getByRole('button', { name: '登录' }));

    expect(await screen.findByText('助手页面')).toBeInTheDocument();
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
