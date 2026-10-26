import { useMemo, useState } from 'react';
import type { FormEvent } from 'react';
import { Link, useSearchParams } from 'react-router';
import { useMutation, useQuery } from '@tanstack/react-query';
import { errorDetails, portalApi } from '../app/api';
import { formatBytes, formatDate } from '../app/format';

const platformName: Record<string, string> = { desktop: '电脑', android: 'Android 手机', ios: 'iPhone' };
const normalizeCode = (value: string) => value.replace(/[\s-]/g, '').toUpperCase();

export function ClientAuthorization() {
  const [searchParams] = useSearchParams();
  const [code, setCode] = useState(() => searchParams.get('user_code') ?? '');
  const [selectedWorkspaces, setSelectedWorkspaces] = useState<string[]>([]);
  const userCode = useMemo(() => normalizeCode(code), [code]);
  const inspected = useQuery({ queryKey: ['client-authorization', userCode], queryFn: () => portalApi.inspectClientAuthorization(userCode), enabled: userCode.length === 8, retry: false });
  const workspaces = useQuery({ queryKey: ['workspaces'], queryFn: portalApi.workspaces, enabled: inspected.data?.wants_workspace_sync === true });
  const renewalInstallationId = inspected.data?.renewal_installation_id;
  const renewalClients = useQuery({ queryKey: ['clients', 'renewal', renewalInstallationId], queryFn: portalApi.clients, enabled: Boolean(renewalInstallationId), retry: false });
  const approve = useMutation({ mutationFn: portalApi.approveClientAuthorization });
  const deny = useMutation({ mutationFn: portalApi.denyClientAuthorization });
  const readyWorkspaces = workspaces.data?.items.filter((workspace) => workspace.state === 'ready') ?? [];
  const renewalInstallation = renewalClients.data?.items.find((client) => client.id === renewalInstallationId);

  const updateCode = (value: string) => { setCode(value.toUpperCase().slice(0, 10)); setSelectedWorkspaces([]); approve.reset(); deny.reset(); };
  const submit = (event: FormEvent<HTMLFormElement>) => { event.preventDefault(); if (userCode.length === 8) void inspected.refetch(); };
  const approveRequest = () => approve.mutate({ user_code: userCode, workspace_ids: inspected.data?.wants_workspace_sync ? selectedWorkspaces : [] }, { onSuccess: () => deny.reset() });
  const denyRequest = () => deny.mutate(userCode, { onSuccess: () => approve.reset() });

  return <section className="device-authorization-page">
    <div className="page-heading"><div><p className="eyebrow">客户端连接</p><h1>确认客户端配对</h1><p className="muted">核对安装名称和平台。只有电脑客户端可以获得明确选择的工作区同步权限。</p></div></div>
    {approve.data ? <div className="notice notice-success" role="status"><h2>客户端已连接</h2><p><strong>{approve.data.display_name}</strong> 已获得你确认的权限。</p><p>凭证到期：{formatDate(approve.data.expires_at)}</p><Link className="button button-secondary" to="/clients">查看客户端</Link></div>
      : deny.isSuccess ? <div className="notice notice-info" role="status"><h2>已拒绝此请求</h2><p>该安装不能访问你的 Morde 账号。</p></div>
        : <>
          <div className="notice notice-info"><strong>请确认这是你正在连接的设备。</strong><p>不认识安装名称或平台时，请拒绝请求。</p></div>
          <form className="device-code-card" onSubmit={submit}>
            <label htmlFor="client-user-code">客户端验证码</label>
            <div className="device-code-input"><input id="client-user-code" value={code} onChange={(event) => updateCode(event.target.value)} autoComplete="one-time-code" spellCheck={false} aria-describedby="client-code-help" /><button className="button button-secondary" type="submit" disabled={userCode.length !== 8 || inspected.isFetching}>核对客户端</button></div>
            <small id="client-code-help" className="muted">输入客户端显示的 8 位验证码，验证码在 10 分钟后失效。</small>
          </form>
          {inspected.isFetching && <div className="loading-card" role="status">正在核对客户端…</div>}
          {inspected.isError && <div className="error-card" role="alert">{errorDetails(inspected.error).message}<button className="button button-secondary" type="button" onClick={() => void inspected.refetch()}>重试</button></div>}
          {inspected.data && <div className="device-approval-card">
            <h2>请求来自：{inspected.data.display_name}</h2><p>平台：{platformName[inspected.data.platform] ?? inspected.data.platform}</p><p className="muted">验证码到期时间：{formatDate(inspected.data.expires_at)}</p>
            {renewalInstallationId && <div className="notice notice-info" role="status">
              <strong>这是已配对客户端的授权续期。</strong>
              <p className="mono">安装 ID：{renewalInstallationId}</p>
              {renewalClients.isPending && <p>正在核对已登记的安装信息…</p>}
              {renewalClients.isError && <p role="alert">{errorDetails(renewalClients.error).message}<button className="button button-secondary" type="button" onClick={() => void renewalClients.refetch()}>重试</button></p>}
              {renewalClients.isSuccess && !renewalInstallation && <p role="alert">找不到这台已配对安装，无法核对。请拒绝请求并重新发起配对。</p>}
              {renewalInstallation && <>
                <p>已登记名称：{renewalInstallation.display_name || '未命名客户端'}</p>
                <p>本次请求名称：{inspected.data.display_name}</p>
                {renewalInstallation.revoked && <p role="alert">此安装已撤销，不能通过续期重新启用。请拒绝请求并重新配对。</p>}
              </>}
            </div>}
            {inspected.data.wants_workspace_sync && (workspaces.isPending ? <div className="loading-card">正在加载工作区…</div>
              : workspaces.isError ? <div className="error-card" role="alert">{errorDetails(workspaces.error).message}<button className="button button-secondary" type="button" onClick={() => void workspaces.refetch()}>重试</button></div>
                : <><fieldset className="device-workspace-grants"><legend>允许此电脑同步哪些工作区</legend>
                  {!readyWorkspaces.length ? <p className="muted">目前没有可授权的工作区。可以先配对，之后重新授权。</p> : readyWorkspaces.map((workspace) => <label className="checkbox-row" key={workspace.id}>
                    <input type="checkbox" checked={selectedWorkspaces.includes(workspace.id)} onChange={(event) => setSelectedWorkspaces((current) => event.target.checked ? [...current, workspace.id] : current.filter((id) => id !== workspace.id))} />
                    <span>{workspace.name || workspace.id}<small className="table-sub">{formatBytes(workspace.used_bytes)} · 版本 {workspace.revision}</small></span>
                  </label>)}
                </fieldset>{!selectedWorkspaces.length && <p className="notice notice-info">未选择工作区时，客户端可以使用任务和云端电脑，但不能同步文件。</p>}</>)}
            {(approve.isError || deny.isError) && <div className="form-error" role="alert">{errorDetails(approve.error ?? deny.error).message}</div>}
            <div className="device-approval-actions"><button className="button button-primary" type="button" disabled={approve.isPending || deny.isPending || (inspected.data.wants_workspace_sync && !workspaces.isSuccess) || Boolean(renewalInstallationId && (!renewalClients.isSuccess || !renewalInstallation || renewalInstallation.revoked))} onClick={approveRequest}>{approve.isPending ? '正在授权…' : '确认并连接'}</button><button className="button button-danger-quiet" type="button" disabled={approve.isPending || deny.isPending} onClick={denyRequest}>{deny.isPending ? '正在拒绝…' : '拒绝请求'}</button></div>
          </div>}
        </>}
  </section>;
}
