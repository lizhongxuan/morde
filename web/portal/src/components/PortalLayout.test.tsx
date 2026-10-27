import { cleanup, render, screen } from '@testing-library/react';
import { MemoryRouter } from 'react-router';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { PortalLayout } from './PortalLayout';
import { ToastProvider } from '../app/toast';

vi.mock('../app/auth', () => ({ useAuth: () => ({ user: { name: 'Alice', role: 'user' }, signOut: vi.fn() }) }));

describe('PortalLayout', () => {
  afterEach(cleanup);

  it('links the PC portal to the same-origin mobile app', () => {
    render(<ToastProvider><MemoryRouter><PortalLayout /></MemoryRouter></ToastProvider>);

    expect(screen.getByRole('link', { name: '打开手机端' })).toHaveAttribute('href', '/app/');
    expect(screen.getByRole('heading', { level: 1, name: '工作台' })).toBeInTheDocument();
    expect(screen.queryByRole('link', { name: '任务与计划' })).not.toBeInTheDocument();
  });
});
