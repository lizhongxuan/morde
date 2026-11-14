import { cleanup, render, screen } from '@testing-library/react';
import { MemoryRouter } from 'react-router';
import { afterEach, expect, it, vi } from 'vitest';
import { ToastProvider } from '../../app/toast';
import { AdminLayout } from './AdminLayout';

vi.mock('../../app/auth', () => ({ useAuth: () => ({ user: { name: 'Admin' }, signOut: vi.fn() }) }));
afterEach(cleanup);

it('only shows the retained administration navigation', () => {
  render(<ToastProvider><MemoryRouter initialEntries={['/admin']}><AdminLayout /></MemoryRouter></ToastProvider>);
  expect(screen.getByRole('link', { name: '配置与预算' })).toHaveAttribute('href', '/admin/settings');
  for (const name of ['邀请', '站点与提示', '活动']) {
    expect(screen.queryByRole('link', { name })).not.toBeInTheDocument();
  }
  expect(screen.queryByRole('link', { name: /工作区/ })).not.toBeInTheDocument();
  expect(screen.queryByText('管理控制台')).not.toBeInTheDocument();
});
