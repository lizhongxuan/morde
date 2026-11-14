import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { portalApi } from '../app/api';
import type { HostMember } from '../app/api';
import { ToastNoticeCenter, ToastProvider } from '../app/toast';
import { HostMemberManager } from './HostMemberManager';

const hostId = 'h1234567';
const ownerID = 'u1234567';
const member: HostMember = {
  user_id: 'u7654321', permissions: ['desktop:view', 'terminal:control'], created_at: '2026-10-22T08:00:00Z',
};

function renderManager(hostId: string) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(<QueryClientProvider client={queryClient}><ToastProvider><ToastNoticeCenter /><HostMemberManager hostId={hostId} /></ToastProvider></QueryClientProvider>);
}

async function loginOwner() {
  vi.spyOn(portalApi, 'login').mockResolvedValue({ user: { id: ownerID } as never });
  await portalApi.login('demo', 'demo');
  return { hostId, ownerID };
}

async function expandManager(user: ReturnType<typeof userEvent.setup>) {
  await user.click(screen.getByRole('button', { name: '管理共享成员' }));
}

async function prepareInvite(user: ReturnType<typeof userEvent.setup>, userID = member.user_id, permissions: string[] = ['desktop:view']) {
  await user.type(screen.getByLabelText('对方账号 ID'), userID);
  for (const permission of permissions) {
    await user.click(screen.getByRole('checkbox', { name: new RegExp(permission) }));
  }
}

describe('HostMemberManager', () => {
  afterEach(() => { cleanup(); vi.restoreAllMocks(); });

  it('loads members only after expansion and shows the empty state', async () => {
    const { hostId } = await loginOwner();
    type MemberList = Awaited<ReturnType<typeof portalApi.hostMembers>>;
    let resolveMembers: ((value: MemberList) => void) | undefined;
    const load = vi.spyOn(portalApi, 'hostMembers').mockImplementation(() => new Promise((resolve) => { resolveMembers = resolve; }));
    const user = userEvent.setup();
    renderManager(hostId);

    expect(load).not.toHaveBeenCalled();
    await expandManager(user);
    expect(await screen.findByRole('status')).toHaveTextContent('正在加载共享成员');
    expect(load).toHaveBeenCalledWith(hostId);
    resolveMembers?.({ items: [] });
    expect(await screen.findByText('还没有共享成员。')).toBeInTheDocument();
  });

  it('shows member loading errors and retries the API request', async () => {
    vi.spyOn(portalApi, 'hostMembers').mockRejectedValueOnce(new Error('成员列表暂不可用')).mockResolvedValue({ items: [] });
    const user = userEvent.setup();
    renderManager(hostId);

    await expandManager(user);
    expect(await screen.findByRole('alert')).toHaveTextContent('成员列表暂不可用');
    await user.click(screen.getByRole('button', { name: '重试' }));
    expect(await screen.findByText('还没有共享成员。')).toBeInTheDocument();
  });

  it('does not open confirmation or call the API without at least one permission', async () => {
    await loginOwner();
    vi.spyOn(portalApi, 'hostMembers').mockResolvedValue({ items: [] });
    const invite = vi.spyOn(portalApi, 'inviteHostMember');
    const user = userEvent.setup();
    renderManager(hostId);

    await expandManager(user);
    await screen.findByText('还没有共享成员。');
    await user.type(screen.getByLabelText('对方账号 ID'), member.user_id);
    expect(screen.getByRole('button', { name: '邀请并授权' })).toBeDisabled();
    await user.click(screen.getByRole('button', { name: '邀请并授权' }));
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    expect(invite).not.toHaveBeenCalled();
  });

  it('announces each permission description with its checkbox', async () => {
    vi.spyOn(portalApi, 'hostMembers').mockResolvedValue({ items: [] });
    const user = userEvent.setup();
    renderManager(hostId);

    await expandManager(user);
    const checkbox = screen.getByRole('checkbox', { name: /desktop:control/ });
    const descriptionID = `host-member-permission-help-${hostId}-desktop-control`;

    expect(checkbox).toHaveAttribute('aria-describedby', descriptionID);
    expect(document.getElementById(descriptionID)).toHaveTextContent('可以点击和输入，也包含桌面查看权限。');
  });

  it('confirms the exact account and permissions before granting access', async () => {
    await loginOwner();
    vi.spyOn(portalApi, 'hostMembers').mockResolvedValueOnce({ items: [] }).mockResolvedValue({ items: [member] });
    const invite = vi.spyOn(portalApi, 'inviteHostMember').mockResolvedValue(member);
    const user = userEvent.setup();
    renderManager(hostId);

    await expandManager(user);
    await screen.findByText('还没有共享成员。');
    await prepareInvite(user, member.user_id, ['desktop:control', 'terminal:control']);
    await user.click(screen.getByRole('button', { name: '邀请并授权' }));

    const dialog = screen.getByRole('dialog', { name: '确认邀请共享成员' });
    expect(dialog).toHaveTextContent(member.user_id);
    expect(dialog).toHaveTextContent('desktop:control');
    expect(dialog).toHaveTextContent('terminal:control');
    expect(dialog).toHaveTextContent('不包含工作区访问、主机管理或再次分享权限');
    expect(invite).not.toHaveBeenCalled();

    await user.click(within(dialog).getByRole('button', { name: '确认授权' }));
    await waitFor(() => expect(invite).toHaveBeenCalledWith(hostId, {
      user_id: member.user_id, permissions: ['desktop:control', 'terminal:control'],
    }));
    expect(await screen.findByText(`已为账号 ${member.user_id} 授予共享权限。`)).toBeInTheDocument();
    expect(await screen.findByText(member.user_id)).toBeInTheDocument();
  });

  it('can grant Workspace file reading without granting desktop, terminal, or MCP access', async () => {
    await loginOwner();
    const workspaceMember: HostMember = { ...member, permissions: ['workspace:file:read'] };
    vi.spyOn(portalApi, 'hostMembers').mockResolvedValueOnce({ items: [] }).mockResolvedValue({ items: [workspaceMember] });
    const invite = vi.spyOn(portalApi, 'inviteHostMember').mockResolvedValue(workspaceMember);
    const user = userEvent.setup();
    renderManager(hostId);

    await expandManager(user);
    await screen.findByText('还没有共享成员。');
    await prepareInvite(user, member.user_id, ['workspace:file:read']);
    await user.click(screen.getByRole('button', { name: '邀请并授权' }));

    const dialog = screen.getByRole('dialog', { name: '确认邀请共享成员' });
    expect(dialog).toHaveTextContent('workspace:file:read');
    expect(dialog).toHaveTextContent('仅允许浏览和下载已应用的文件');
    expect(dialog).toHaveTextContent('不包含桌面、终端或 MCP 权限');
    expect(dialog).toHaveTextContent('不包含主机管理或再次分享权限');
    await user.click(within(dialog).getByRole('button', { name: '确认授权' }));
    await waitFor(() => expect(invite).toHaveBeenCalledWith(hostId, {
      user_id: member.user_id, permissions: ['workspace:file:read'],
    }));
    expect(screen.getByText(member.user_id).closest('article')).toHaveTextContent('workspace:file:read（读取工作区文件）');
  });

  it('reports an invite API error and keeps the reviewed request available', async () => {
    await loginOwner();
    vi.spyOn(portalApi, 'hostMembers').mockResolvedValue({ items: [] });
    const invite = vi.spyOn(portalApi, 'inviteHostMember').mockRejectedValue(new Error('账号不存在'));
    const user = userEvent.setup();
    renderManager(hostId);

    await expandManager(user);
    await screen.findByText('还没有共享成员。');
    await prepareInvite(user);
    await user.click(screen.getByRole('button', { name: '邀请并授权' }));
    const dialog = screen.getByRole('dialog', { name: '确认邀请共享成员' });
    await user.click(within(dialog).getByRole('button', { name: '确认授权' }));

    expect(await screen.findByRole('alert')).toHaveTextContent('账号不存在');
    expect(invite).toHaveBeenCalledWith(hostId, { user_id: member.user_id, permissions: ['desktop:view'] });
    expect(screen.getByRole('dialog', { name: '确认邀请共享成员' })).toBeInTheDocument();
  });

  it('requires confirmation for revocation and leaves access unchanged when cancelled', async () => {
    await loginOwner();
    vi.spyOn(portalApi, 'hostMembers').mockResolvedValueOnce({ items: [member] }).mockResolvedValue({ items: [] });
    const revoke = vi.spyOn(portalApi, 'revokeHostMember').mockResolvedValue(undefined);
    const user = userEvent.setup();
    renderManager(hostId);

    await expandManager(user);
    expect(await screen.findByText(member.user_id)).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: '撤销权限' }));
    let dialog = screen.getByRole('dialog', { name: '确认撤销共享权限' });
    expect(dialog).toHaveTextContent(member.user_id);
    await user.click(within(dialog).getByRole('button', { name: '取消' }));
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    expect(revoke).not.toHaveBeenCalled();

    await user.click(screen.getByRole('button', { name: '撤销权限' }));
    dialog = screen.getByRole('dialog', { name: '确认撤销共享权限' });
    await user.click(within(dialog).getByRole('button', { name: '撤销权限' }));
    await waitFor(() => expect(revoke).toHaveBeenCalledWith(hostId, member.user_id));
    expect(await screen.findByText(`已撤销账号 ${member.user_id} 的共享权限。`)).toBeInTheDocument();
    expect(await screen.findByText('还没有共享成员。')).toBeInTheDocument();
  });
});
