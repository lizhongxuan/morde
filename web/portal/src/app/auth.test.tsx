import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { portalApi } from './api';
import { AuthProvider, useAuth } from './auth';

vi.mock('../api/client', () => ({
  ApiError: class extends Error { status = 401; },
}));

vi.mock('./api', () => ({
  portalApi: { me: vi.fn(), login: vi.fn(), logout: vi.fn() },
}));

const userA = { id: 'u-a', name: '用户 A', role: 'user' };
const userB = { id: 'u-b', name: '用户 B', role: 'user' };

function AuthControls() {
  const { user, signIn, signOut } = useAuth();
  return <>
    <output data-testid="current-user">{user?.name ?? '未登录'}</output>
    <button type="button" onClick={() => void signOut()}>登出</button>
    <button type="button" onClick={() => void signIn('user-b', 'password')}>登录用户 B</button>
  </>;
}

describe('AuthProvider query cache isolation', () => {
  beforeEach(() => {
    vi.mocked(portalApi.me).mockReset();
    vi.mocked(portalApi.login).mockReset();
    vi.mocked(portalApi.logout).mockReset();
  });

  it('clears user A data on logout before user B signs in', async () => {
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    queryClient.setQueryData(['auth', 'me'], userA);
    queryClient.setQueryData(['hosts'], { items: [{ id: 'host-private-to-a' }] });
    queryClient.setQueryData(['workspaces'], { items: [{ id: 'workspace-private-to-a' }] });
    queryClient.setQueryData(['apikeys'], { items: [{ id: 'key-private-to-a' }] });
    let activeQuerySignal: AbortSignal | undefined;
    void queryClient.fetchQuery({
      queryKey: ['audit'],
      queryFn: ({ signal }) => {
        activeQuerySignal = signal;
        return new Promise<never>(() => {});
      },
    }).catch(() => {});
    vi.mocked(portalApi.logout).mockResolvedValueOnce(undefined);
    vi.mocked(portalApi.login).mockResolvedValueOnce({ user: userB as never });
    const user = userEvent.setup();

    render(<QueryClientProvider client={queryClient}><AuthProvider><AuthControls /></AuthProvider></QueryClientProvider>);

    expect(screen.getByTestId('current-user')).toHaveTextContent('用户 A');
    await user.click(screen.getByRole('button', { name: '登出' }));
    await waitFor(() => expect(screen.getByTestId('current-user')).toHaveTextContent('未登录'));
    expect(queryClient.getQueryData(['auth', 'me'])).toBeNull();
    expect(queryClient.getQueryData(['hosts'])).toBeUndefined();
    expect(queryClient.getQueryData(['workspaces'])).toBeUndefined();
    expect(queryClient.getQueryData(['apikeys'])).toBeUndefined();
    expect(activeQuerySignal?.aborted).toBe(true);
    expect(queryClient.getQueryData(['audit'])).toBeUndefined();

    await user.click(screen.getByRole('button', { name: '登录用户 B' }));
    await waitFor(() => expect(screen.getByTestId('current-user')).toHaveTextContent('用户 B'));
    expect(queryClient.getQueryData(['auth', 'me'])).toEqual(userB);
    expect(queryClient.getQueryData(['hosts'])).toBeUndefined();
    expect(queryClient.getQueryData(['workspaces'])).toBeUndefined();
    expect(queryClient.getQueryData(['apikeys'])).toBeUndefined();
    expect(portalApi.logout).toHaveBeenCalledOnce();
    expect(portalApi.login).toHaveBeenCalledWith('user-b', 'password');
  });
});
