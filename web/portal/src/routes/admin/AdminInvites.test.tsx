import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { MemoryRouter } from 'react-router';
import { portalApi } from '../../app/api';
import { AdminInvites } from './AdminInvites';

type InvitePage = Awaited<ReturnType<typeof portalApi.adminInvites>>;
const firstPage: InvitePage = {
  items: [{ inviter_id: 'u-inviter', inviter_name: 'Alice', invitee_id: 'u-invitee', invitee_name: 'Bob', invitee_status: 'pending', created_at: '2026-10-31T10:00:00Z', inviter_invitees: 4 }],
  total: 30, total_inviters: 8, limit: 25, offset: 0,
};
const secondPage: InvitePage = {
  items: [{ inviter_id: 'u-inviter', inviter_name: 'Alice', invitee_id: 'u-second', invitee_name: 'Carol', invitee_status: 'approved', created_at: '2026-10-30T10:00:00Z', inviter_invitees: 4 }],
  total: 30, total_inviters: 8, limit: 25, offset: 25,
};

function renderPage() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={client}><MemoryRouter><AdminInvites /></MemoryRouter></QueryClientProvider>);
}

describe('admin invites', () => {
  afterEach(() => { cleanup(); vi.restoreAllMocks(); });

  it('filters by inviter or invitee and pages through read-only invite history', async () => {
    const user = userEvent.setup();
    const request = vi.spyOn(portalApi, 'adminInvites').mockImplementation(async (search, _limit, offset) => {
      if (search) return offset ? secondPage : { ...firstPage, items: [{ ...firstPage.items[0]!, invitee_name: 'Bob' }] };
      return offset ? secondPage : firstPage;
    });
    renderPage();

    const inviteRow = await screen.findByRole('row', { name: /Alice.*Bob/ });
    expect(within(inviteRow).getByText('待审核')).toBeInTheDocument();
    expect(within(inviteRow).getByRole('link', { name: /Alice/ })).toHaveAttribute('href', '/admin/users/u-inviter');
    expect(screen.getByText(/邀请不绕过审核，本期不会自动发放奖励/)).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /奖励|删除/ })).not.toBeInTheDocument();

    await user.type(screen.getByRole('searchbox', { name: '搜索邀请人或被邀请人' }), 'u-invitee');
    await user.click(screen.getByRole('button', { name: '搜索' }));
    expect(await screen.findByRole('row', { name: /Alice.*Bob/ })).toBeInTheDocument();
    expect(request).toHaveBeenLastCalledWith('u-invitee', 25, 0);

    await user.click(screen.getByRole('button', { name: '下一页' }));
    expect(await screen.findByRole('row', { name: /Alice.*Carol/ })).toBeInTheDocument();
    expect(request).toHaveBeenLastCalledWith('u-invitee', 25, 25);
  });

  it('distinguishes a genuine empty list from no search matches', async () => {
    vi.spyOn(portalApi, 'adminInvites').mockResolvedValue({ items: [], total: 0, total_inviters: 0, limit: 25, offset: 0 });
    renderPage();

    expect(await screen.findByText('当前没有邀请记录。')).toBeInTheDocument();
    expect(screen.getByText(/邀请关系总数/).closest('article')).toHaveTextContent('0');
  });
});
