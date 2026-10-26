import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { MemoryRouter, Route, Routes } from 'react-router';
import { portalApi } from '../app/api';
import { ChangePassword } from './ChangePassword';

vi.mock('../app/api', async (importOriginal) => {
  const original = await importOriginal<typeof import('../app/api')>();
  return { ...original, portalApi: { ...original.portalApi, changePassword: vi.fn() } };
});

function renderPage() {
  const client = new QueryClient({ defaultOptions: { mutations: { retry: false } } });
  client.setQueryData(['auth', 'me'], { id: 'uadmin01', name: 'admin', role: 'super_admin' });
  render(<QueryClientProvider client={client}><MemoryRouter initialEntries={['/admin/change-password']}><Routes>
    <Route path="/admin/change-password" element={<ChangePassword />} />
  </Routes></MemoryRouter></QueryClientProvider>);
  return client;
}

describe('change password page', () => {
  afterEach(() => { cleanup(); vi.restoreAllMocks(); });

  it('checks confirmation before calling the API', async () => {
    const input = userEvent.setup();
    const changePassword = vi.spyOn(portalApi, 'changePassword').mockResolvedValue(undefined);
    renderPage();
    await input.type(screen.getByLabelText('当前密码'), 'admin');
    await input.type(screen.getByLabelText('新密码'), 'new-password-1');
    await input.type(screen.getByLabelText('确认新密码'), 'new-password-2');
    await input.click(screen.getByRole('button', { name: '修改密码' }));

    expect(await screen.findByRole('alert')).toHaveTextContent('两次输入的新密码不一致');
    expect(changePassword).not.toHaveBeenCalled();
  });

  it('changes the password, then directs the administrator to log in again', async () => {
    const input = userEvent.setup();
    const changePassword = vi.spyOn(portalApi, 'changePassword').mockResolvedValue(undefined);
    const client = renderPage();
    await input.type(screen.getByLabelText('当前密码'), 'admin');
    await input.type(screen.getByLabelText('新密码'), 'new-password-1');
    await input.type(screen.getByLabelText('确认新密码'), 'new-password-1');
    await input.click(screen.getByRole('button', { name: '修改密码' }));

    expect(changePassword.mock.calls[0]?.[0]).toEqual({ current_password: 'admin', new_password: 'new-password-1' });
    expect(await screen.findByRole('heading', { name: '密码已修改' })).toBeInTheDocument();
    expect(screen.getByRole('link', { name: '前往管理后台登录' })).toHaveAttribute('href', '/admin/login');
    expect(client.getQueryData(['auth', 'me'])).toBeNull();
  });

  it('keeps the form and reports server errors', async () => {
    const input = userEvent.setup();
    vi.spyOn(portalApi, 'changePassword').mockRejectedValue(new Error('当前密码不正确。'));
    renderPage();
    await input.type(screen.getByLabelText('当前密码'), 'wrong-password');
    await input.type(screen.getByLabelText('新密码'), 'new-password-1');
    await input.type(screen.getByLabelText('确认新密码'), 'new-password-1');
    await input.click(screen.getByRole('button', { name: '修改密码' }));

    expect(await screen.findByRole('alert')).toHaveTextContent('当前密码不正确');
    expect(screen.getByRole('heading', { name: '修改密码' })).toBeInTheDocument();
  });
});
