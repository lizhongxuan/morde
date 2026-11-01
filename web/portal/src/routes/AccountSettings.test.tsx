import { cleanup, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { MemoryRouter } from 'react-router';
import { AccountSettings } from './AccountSettings';

vi.mock('../app/auth', () => ({ useAuth: () => ({ user: { name: 'Alice', role: 'user' } }) }));

describe('AccountSettings', () => {
  afterEach(cleanup);

  it('shows the signed-in account and links to password and installation management', () => {
    render(<MemoryRouter><AccountSettings /></MemoryRouter>);

    expect(screen.getByRole('heading', { level: 1, name: '账户设置' })).toBeInTheDocument();
    expect(screen.getByRole('heading', { level: 3, name: 'Alice' })).toBeInTheDocument();
    expect(screen.getByText('普通用户')).toBeInTheDocument();
    expect(screen.getByRole('link', { name: '修改密码' })).toHaveAttribute('href', '/settings/password');
    expect(screen.getByRole('link', { name: '管理客户端' })).toHaveAttribute('href', '/clients');
  });
});
