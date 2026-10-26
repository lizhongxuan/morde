import { afterEach, describe, expect, it, vi } from 'vitest';
import { requestFetch } from './client';

afterEach(() => vi.unstubAllGlobals());

describe('requestFetch authentication redirects', () => {
  it('leaves the unauthenticated session probe to AuthProvider', async () => {
    const assign = vi.fn();
    vi.stubGlobal('window', { location: { origin: 'https://portal.example', pathname: '/download', search: '', assign } });
    vi.stubGlobal('fetch', vi.fn(async () => new Response(null, { status: 401 })));

    await expect(requestFetch('/api/me')).resolves.toMatchObject({ status: 401 });
    expect(assign).not.toHaveBeenCalled();
  });

  it('redirects a protected request that loses its session', async () => {
    const assign = vi.fn();
    vi.stubGlobal('window', { location: { origin: 'https://portal.example', pathname: '/workspaces', search: '?tab=local', assign } });
    vi.stubGlobal('fetch', vi.fn(async () => new Response(null, { status: 401 })));

    await requestFetch('/api/workspaces');
    expect(assign).toHaveBeenCalledWith('/login?next=%2Fworkspaces%3Ftab%3Dlocal');
  });
});
