import { useState } from 'react';
import type { FormEvent } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { errorDetails, portalApi } from '../app/api';
import type { HostMember, HostMemberPermission } from '../app/api';
import { formatDate } from '../app/format';
import { useToast } from '../app/toast';
import { ConfirmDialog } from './ConfirmDialog';
import { FloatingError } from './FloatingError';

const permissionOptions: { value: HostMemberPermission; title: string; description: string }[] = [
  { value: 'desktop:view', title: '查看桌面', description: '只能查看画面，不能点击或输入。' },
  { value: 'desktop:control', title: '操作桌面', description: '可以点击和输入，也包含桌面查看权限。' },
  { value: 'terminal:control', title: '终端操作', description: '可以打开终端并运行命令。' },
  { value: 'mcp:control', title: 'MCP 操作', description: '可以通过 MCP 使用主机能力；对方还需要自己的有效 API Key。' },
  { value: 'workspace:file:read', title: '读取电脑文件', description: '可以浏览和下载这台云端电脑已应用的文件；不包含桌面、终端或 MCP 权限。' },
];

const permissionTitles = Object.fromEntries(permissionOptions.map(({ value, title }) => [value, title])) as Record<HostMemberPermission, string>;
const validUserID = (value: string) => /^u[a-z0-9]{7}$/.test(value);

type InviteConfirmation = { userID: string; permissions: HostMemberPermission[] };

export function HostMemberManager({ hostId }: { hostId: string }) {
  const queryClient = useQueryClient();
  const { notify } = useToast();
  const [expanded, setExpanded] = useState(false);
  const [userID, setUserID] = useState('');
  const [permissions, setPermissions] = useState<HostMemberPermission[]>([]);
  const [inviteConfirmation, setInviteConfirmation] = useState<InviteConfirmation | null>(null);
  const [revokeConfirmation, setRevokeConfirmation] = useState<HostMember | null>(null);

  const members = useQuery({
    queryKey: ['host-members', hostId],
    queryFn: () => portalApi.hostMembers(hostId),
    enabled: expanded,
    retry: false,
  });

  const invite = useMutation({
    mutationFn: ({ userID: memberUserID, permissions: granted }: InviteConfirmation) =>
      portalApi.inviteHostMember(hostId, { user_id: memberUserID, permissions: granted }),
    onSuccess: async (member) => {
      setInviteConfirmation(null);
      setUserID('');
      setPermissions([]);
      notify(`已为账号 ${member.user_id} 授予共享权限。`, 'success');
      await queryClient.invalidateQueries({ queryKey: ['host-members', hostId] });
    },
    onError: (error) => notify(errorDetails(error).message, 'error'),
  });

  const revoke = useMutation({
    mutationFn: (member: HostMember) => portalApi.revokeHostMember(hostId, member.user_id),
    onSuccess: async (_, member) => {
      setRevokeConfirmation(null);
      notify(`已撤销账号 ${member.user_id} 的共享权限。`, 'success');
      await queryClient.invalidateQueries({ queryKey: ['host-members', hostId] });
    },
    onError: (error) => notify(errorDetails(error).message, 'error'),
  });

  const canInvite = validUserID(userID) && permissions.length > 0 && !invite.isPending;
  const grantsWorkspaceRead = inviteConfirmation?.permissions.includes('workspace:file:read') ?? false;
  const togglePermission = (permission: HostMemberPermission) => {
    setPermissions((current) => current.includes(permission)
      ? current.filter((value) => value !== permission)
      : [...current, permission]);
  };
  const submitInvite = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (!canInvite) return;
    setInviteConfirmation({ userID, permissions: [...permissions] });
  };

  return <section className="host-member-manager">
    <div className="host-actions">
      <button type="button" className="button button-secondary" aria-expanded={expanded} onClick={() => setExpanded((value) => !value)}>
        {expanded ? '收起共享成员' : '管理共享成员'}
      </button>
    </div>
    {expanded && <div className="host-control">
      <h3>共享成员</h3>
      <form className="connection-form" onSubmit={submitInvite}>
        <div className="connection-form-grid">
          <div className="connection-form-wide">
            <label htmlFor={`host-member-user-${hostId}`}>对方账号 ID</label>
            <input
              id={`host-member-user-${hostId}`}
              value={userID}
              onChange={(event) => setUserID(event.target.value)}
              placeholder="例如：u1234567"
              autoComplete="off"
              maxLength={8}
              pattern="u[a-z0-9]{7}"
              disabled={invite.isPending || inviteConfirmation !== null}
              aria-describedby={`host-member-user-help-${hostId}`}
            />
            <small id={`host-member-user-help-${hostId}`} className="muted">请输入现有 Morde 账号 ID（u 加 7 位小写字母或数字）；账号是否存在由服务端验证。</small>
            {userID && !validUserID(userID) && <FloatingError>账号 ID 格式应为 u 加 7 位小写字母或数字。</FloatingError>}
          </div>
        </div>
        <fieldset className="fieldset">
          <legend>授予权限（至少选择一项）</legend>
          {permissionOptions.map(({ value, title, description }) => (
            <label className="checkbox-row" key={value}>
              <input
                type="checkbox"
                checked={permissions.includes(value)}
                onChange={() => togglePermission(value)}
                disabled={invite.isPending || inviteConfirmation !== null}
                aria-label={`${value} — ${title}`}
                aria-describedby={`host-member-permission-help-${hostId}-${value.replace(':', '-')}`}
              />
              <span>
                <strong>{value}</strong> — {title}<br />
                <small id={`host-member-permission-help-${hostId}-${value.replace(':', '-')}`} className="muted">
                  {description}
                </small>
              </span>
            </label>
          ))}
        </fieldset>
        <div className="form-actions">
          <button type="submit" className="button button-primary" disabled={!canInvite}>邀请并授权</button>
        </div>
      </form>

      {members.isPending ? <div className="loading-card" role="status">正在加载共享成员…</div>
        : members.isError ? <FloatingError className="error-card" role="alert">{errorDetails(members.error).message}<button type="button" className="button button-secondary" onClick={() => void members.refetch()}>重试</button></FloatingError>
          : !members.data.items.length ? <div className="empty-card compact"><p>还没有共享成员。</p></div>
            : <div className="device-manager-list">{members.data.items.map((member) => (
              <article className="device-manager-card" key={member.user_id}>
                <div className="host-card-heading"><div><h4>{member.user_id}</h4><p className="muted">账号 ID</p></div></div>
                <p className="device-manager-meta">权限：{member.permissions.map((permission) => `${permission}（${permissionTitles[permission]}）`).join('、')}</p>
                <p className="device-manager-meta">授权于 {formatDate(member.created_at)}</p>
                <div className="device-manager-actions">
                  <button type="button" className="button button-danger-quiet" onClick={() => setRevokeConfirmation(member)}>撤销权限</button>
                </div>
              </article>
            ))}</div>}
    </div>}

    {inviteConfirmation && <ConfirmDialog
      title="确认邀请共享成员"
      description={`确认向账号 ${inviteConfirmation.userID} 授予：${inviteConfirmation.permissions.map((permission) => `${permission}（${permissionTitles[permission]}）`).join('、')}。桌面控制包含查看权限。${grantsWorkspaceRead ? '电脑文件权限仅允许浏览和下载已应用的云端电脑文件；不包含桌面、终端或 MCP 权限，也不包含主机管理或再次分享权限。' : '此授权不包含云端电脑文件访问、主机管理或再次分享权限。'}`}
      confirmLabel="确认授权"
      busy={invite.isPending}
      onClose={() => { if (!invite.isPending) setInviteConfirmation(null); }}
      onConfirm={() => invite.mutate(inviteConfirmation)}
    />}
    {revokeConfirmation && <ConfirmDialog
      title="确认撤销共享权限"
      description={`确定撤销账号 ${revokeConfirmation.user_id} 对这台主机的共享权限吗？撤销后，该账号将无法再通过这些权限访问主机。`}
      confirmLabel="撤销权限"
      danger
      busy={revoke.isPending}
      onClose={() => { if (!revoke.isPending) setRevokeConfirmation(null); }}
      onConfirm={() => revoke.mutate(revokeConfirmation)}
    />}
  </section>;
}
