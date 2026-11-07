import type { ReactNode } from 'react';
import { cleanup, render, screen } from '@testing-library/react';
import { afterEach, expect, it, vi } from 'vitest';
import { AdminApp } from './App';

vi.mock('./app/auth', () => ({
  AuthProvider: ({ children }: { children: ReactNode }) => children,
  useAuth: () => ({ user: { name: 'Admin', role: 'admin' }, signOut: vi.fn() }),
}));
afterEach(cleanup);

it.each(['/admin/invites', '/admin/sites', '/admin/activity'])('does not expose removed page %s', (path) => {
  window.history.pushState({}, '', path);
  render(<AdminApp />);
  expect(screen.getByRole('heading', { name: '管理后台地址无效' })).toBeInTheDocument();
});
