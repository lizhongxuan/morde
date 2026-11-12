import { useRef, useState } from 'react';
import type { FormEvent } from 'react';
import { Link, useNavigate } from 'react-router';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { ApiError } from '../api/client';
import { errorDetails, newIdempotencyKey, portalApi } from '../app/api';
import { useAuth } from '../app/auth';
import { useToast } from '../app/toast';

type HostCreateInput = { name?: string; workspace_mode: 'new' | 'existing' | 'automatic'; workspace_ids: string[] };
type PendingHostCreate = {
  version: 1;
  fingerprint: string;
  input: HostCreateInput;
  hostIdempotencyKey: string;
  workspaceIdempotencyKey?: string;
  workspaceID?: string;
};

function pendingCreateKey(userID: string) {
  return `morde.pending-host-create.${userID}`;
}

function isPendingHostCreate(value: unknown): value is PendingHostCreate {
  if (!value || typeof value !== 'object') return false;
  const pending = value as Partial<PendingHostCreate>;
  const input = pending.input;
  const validKey = (key: unknown) => typeof key === 'string' && /^[A-Za-z0-9._~-]{8,128}$/.test(key);
  return pending.version === 1 && !!input && typeof input === 'object' &&
    (input.name === undefined || typeof input.name === 'string') &&
    (input.workspace_mode === 'new' || input.workspace_mode === 'existing' || input.workspace_mode === 'automatic') &&
    Array.isArray(input.workspace_ids) && input.workspace_ids.every((id) => typeof id === 'string') &&
    pending.fingerprint === JSON.stringify(input) && validKey(pending.hostIdempotencyKey) &&
    (input.workspace_mode !== 'new' || validKey(pending.workspaceIdempotencyKey)) &&
    (pending.workspaceID === undefined || typeof pending.workspaceID === 'string');
}

function readPendingHostCreate(userID?: string): { pending: PendingHostCreate | null; invalid: boolean } {
  if (!userID) return { pending: null, invalid: false };
  try {
    const raw = localStorage.getItem(pendingCreateKey(userID));
    if (!raw) return { pending: null, invalid: false };
    const value: unknown = JSON.parse(raw);
    return isPendingHostCreate(value) ? { pending: value, invalid: false } : { pending: null, invalid: true };
  } catch {
    return { pending: null, invalid: true };
  }
}

export function NewHost() {
  const navigate = useNavigate();
  const client = useQueryClient();
  const { user } = useAuth();
  const { notify } = useToast();
  const hosts = useQuery({ queryKey: ['hosts'], queryFn: portalApi.hosts });
  const [restored] = useState(() => readPendingHostCreate(user?.id));
  const [name, setName] = useState(restored.pending?.input.name ?? '');
  const pendingCreate = useRef<PendingHostCreate | null>(restored.pending);
  const [recoveryPending, setRecoveryPending] = useState(!!restored.pending);
  const [recoveryError, setRecoveryError] = useState(restored.invalid);

  const create = useMutation({
    mutationFn: async (pending: PendingHostCreate) => {
      const input = pending.input;
      if (input.workspace_mode === 'automatic') {
        return portalApi.createHost({ ...(input.name ? { name: input.name } : {}) }, pending.hostIdempotencyKey);
      }
      let workspaceIDs = input.workspace_ids;
      if (input.workspace_mode === 'new') {
        let workspaceID = pending.workspaceID;
        if (!workspaceID) {
          const workspace = await portalApi.createWorkspace(
            { name: `${input.name || '项目'}工作区`.slice(0, 100) }, pending.workspaceIdempotencyKey,
          );
          workspaceID = workspace.id;
          pending.workspaceID = workspaceID;
          localStorage.setItem(pendingCreateKey(user!.id), JSON.stringify(pending));
        }
        workspaceIDs = [workspaceID];
      }
      return portalApi.createHost({ ...(input.name ? { name: input.name } : {}), workspace_ids: workspaceIDs }, pending.hostIdempotencyKey);
    },
    onSuccess: () => {
      try { localStorage.removeItem(pendingCreateKey(user!.id)); } catch { /* A stale record only replays this same idempotent request. */ }
      pendingCreate.current = null;
      setRecoveryPending(false);
      void client.invalidateQueries({ queryKey: ['hosts'] });
      void client.invalidateQueries({ queryKey: ['workspaces'] });
      notify('云端电脑创建已提交，正在等待启动。', 'success');
      navigate('/hosts', { replace: true });
    },
    onError: (error) => {
      const knownRejection = error instanceof ApiError && error.status >= 400 && error.status < 500 && error.status !== 401;
      setRecoveryPending(!knownRejection);
      void client.invalidateQueries({ queryKey: ['workspaces'] });
      notify(errorDetails(error).message, 'error');
    },
  });
  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (create.isPending || recoveryError || !user) return;
    if (recoveryPending && pendingCreate.current) {
      create.mutate(pendingCreate.current);
      return;
    }
    const input = { ...(name.trim() ? { name: name.trim() } : {}), workspace_mode: 'automatic' as const, workspace_ids: [] as string[] };
    const fingerprint = JSON.stringify(input);
    if (!pendingCreate.current || pendingCreate.current.fingerprint !== fingerprint) {
      pendingCreate.current = {
        version: 1,
        fingerprint,
        input,
        hostIdempotencyKey: newIdempotencyKey(),
      };
    }
    try {
      localStorage.setItem(pendingCreateKey(user.id), JSON.stringify(pendingCreate.current));
    } catch {
      notify('浏览器无法保存创建恢复信息，请检查站点存储后重试。', 'error');
      return;
    }
    setRecoveryPending(true);
    create.mutate(pendingCreate.current);
  };
  const abandonRecovery = () => {
    if (!user) return;
    try {
      localStorage.removeItem(pendingCreateKey(user.id));
      pendingCreate.current = null;
      setRecoveryPending(false);
      setRecoveryError(false);
    } catch {
      notify('无法清除本机的创建恢复记录。', 'error');
    }
  };

  if (hosts.isPending) return <div className="loading-card" role="status">正在检查云端电脑数量…</div>;
  if (hosts.isError) return <div className="error-card" role="alert">{errorDetails(hosts.error).message}<button type="button" className="button button-secondary" onClick={() => void hosts.refetch()}>重试</button></div>;
  if (hosts.data.items.length >= (user?.quota.hosts ?? 0)) return <div className="notice notice-warning" role="alert">已达到云端电脑数量上限</div>;
  return <section>
    {recoveryError && <div className="notice notice-warning" role="alert">无法读取本机上次创建记录。为避免重复创建，请先到主机列表核实结果，再选择清除记录。<button type="button" className="button button-secondary" onClick={abandonRecovery}>我已核实，清除记录</button></div>}
    <form className="form-card form-stack" onSubmit={submit}>
      <div className="design-panel-header"><div><h2>基本配置</h2><p>使用管理员为你设置的规格。</p></div><Link className="button button-secondary" to="/hosts">← 返回云端电脑</Link></div>
      {recoveryPending && !create.isPending && <div className="notice notice-warning" role="status">上次创建结果尚未确认。继续会用相同请求编号核对结果；若已在主机列表确认，可以放弃恢复并重新填写。<button type="button" className="button button-secondary" onClick={abandonRecovery}>我已核实，放弃恢复并重新填写</button></div>}
      <p className="notice notice-info">默认工作和下载目录位于 <code>/home/gem/workspace</code>。建议将需要保留的文件保存到此目录。</p>
      <label htmlFor="host-name">云端电脑名称 <span className="muted">（可选）</span></label>
      <input id="host-name" value={name} maxLength={100} disabled={recoveryPending || recoveryError} onChange={(event) => setName(event.target.value)} placeholder="例如：数据分析" />
      <p className="muted">电脑会自动准备文件空间；你不需要单独选择工作区。</p>
      <div className="form-actions"><Link to="/hosts" className="button button-secondary">取消</Link><button className="button button-primary" type="submit" disabled={create.isPending || recoveryError}>{create.isPending ? '提交中…' : recoveryPending ? '继续上次创建' : '创建云端电脑'}</button></div>
    </form>
  </section>;
}
