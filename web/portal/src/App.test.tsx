import type { ReactNode } from 'react';
import { cleanup, render, screen } from '@testing-library/react';
import { afterEach, expect, it, vi } from 'vitest';
import { AdminApp } from './App';

const authState = vi.hoisted(() => ({ user: { name: 'Admin', role: 'admin' as string } }));

vi.mock('./app/auth', () => ({
  AuthProvider: ({ children }: { children: ReactNode }) => children,
  useAuth: () => ({ user: authState.user, signOut: vi.fn() }),
}));
afterEach(() => { cleanup(); authState.user = { name: 'Admin', role: 'admin' }; });

it.each(['/admin/invites', '/admin/sites', '/admin/activity'])('does not expose removed page %s', (path) => {
  window.history.pushState({}, '', path);
  render(<AdminApp />);
  expect(screen.getByRole('heading', { name: '管理后台地址无效' })).toBeInTheDocument();
});

it('keeps a non-admin on the administration login route', () => {
  authState.user = { name: 'Alice', role: 'user' };
  window.history.pushState({}, '', '/admin');
  render(<AdminApp />);
  expect(screen.getByRole('link', { name: '切换管理员账号' })).toHaveAttribute('href', '/admin/login');
});
