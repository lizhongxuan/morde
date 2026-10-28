import { cleanup, render, screen } from '@testing-library/react';
import { MemoryRouter } from 'react-router';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { PortalLayout } from './PortalLayout';
import { ToastProvider } from '../app/toast';

vi.mock('../app/auth', () => ({ useAuth: () => ({ user: { name: 'Alice', role: 'user' }, signOut: vi.fn() }) }));

describe('PortalLayout', () => {
  afterEach(cleanup);

  it('opens the cloud computer page first and links to the same-origin mobile app', () => {
    render(<ToastProvider><MemoryRouter initialEntries={['/hosts']}><PortalLayout /></MemoryRouter></ToastProvider>);

    expect(screen.getByRole('link', { name: '打开手机端' })).toHaveAttribute('href', '/app/');
    expect(screen.getByRole('heading', { level: 1, name: '云端电脑' })).toBeInTheDocument();
    expect(screen.queryByRole('link', { name: '工作台' })).not.toBeInTheDocument();
    expect(screen.getByRole('link', { name: '云端电脑' })).toHaveAttribute('href', '/hosts');
    expect(screen.queryByRole('link', { name: '任务与计划' })).not.toBeInTheDocument();
  });
});
