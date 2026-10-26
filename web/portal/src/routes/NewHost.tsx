import { useMemo, useRef, useState } from 'react';
import type { FormEvent } from 'react';
import { Link, useNavigate } from 'react-router';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { ApiError } from '../api/client';
import { errorDetails, newIdempotencyKey, portalApi } from '../app/api';
import { useAuth } from '../app/auth';
import { formatBytes, formatCpu, formatDuration, formatMemory } from '../app/format';
import { useToast } from '../app/toast';

type HostCreateInput = { template_id: string; name?: string; workspace_mode: 'new' | 'existing'; workspace_ids: string[] };
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
    typeof input.template_id === 'string' &&
    (input.name === undefined || typeof input.name === 'string') &&
    (input.workspace_mode === 'new' || input.workspace_mode === 'existing') &&
    Array.isArray(input.workspace_ids) && input.workspace_ids.every((id) => typeof id === 'string') &&
    pending.fingerprint === JSON.stringify(input) && validKey(pending.hostIdempotencyKey) &&
    (input.workspace_mode === 'existing' || validKey(pending.workspaceIdempotencyKey)) &&
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
  const templates = useQuery({ queryKey: ['templates'], queryFn: portalApi.templates, staleTime: 300000 });
  const workspaces = useQuery({ queryKey: ['workspaces'], queryFn: portalApi.workspaces });
  const [restored] = useState(() => readPendingHostCreate(user?.id));
  const [templateId, setTemplateId] = useState(restored.pending?.input.template_id ?? '');
  const [name, setName] = useState(restored.pending?.input.name ?? '');
  const [workspaceMode, setWorkspaceMode] = useState<'new' | 'existing'>(restored.pending?.input.workspace_mode ?? 'new');
  const template = useMemo(() => templates.data?.items.find((item) => item.id === templateId) ?? templates.data?.items[0], [templates.data, templateId]);
  const [workspaceIds, setWorkspaceIds] = useState<string[]>(restored.pending?.input.workspace_ids ?? []);
  const pendingCreate = useRef<PendingHostCreate | null>(restored.pending);
  const [recoveryPending, setRecoveryPending] = useState(!!restored.pending);
  const [recoveryError, setRecoveryError] = useState(restored.invalid);
  const readyWorkspaces = workspaces.data?.items.filter((workspace) => workspace.state === 'ready') ?? [];

  const create = useMutation({
    mutationFn: async (pending: PendingHostCreate) => {
      const input = pending.input;
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
      return portalApi.createHost({ template_id: input.template_id, ...(input.name ? { name: input.name } : {}), workspace_ids: workspaceIDs }, pending.hostIdempotencyKey);
    },
    onSuccess: () => {
      try { localStorage.removeItem(pendingCreateKey(user!.id)); } catch { /* A stale record only replays this same idempotent request. */ }
      pendingCreate.current = null;
      setRecoveryPending(false);
      void client.invalidateQueries({ queryKey: ['hosts'] });
      void client.invalidateQueries({ queryKey: ['workspaces'] });
      notify('主机创建已提交，正在等待启动。', 'success');
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
    if (!template || create.isPending || recoveryError || !user) return;
    const input = { template_id: template.id, ...(name.trim() ? { name: name.trim() } : {}), workspace_mode: workspaceMode, workspace_ids: workspaceIds };
    const fingerprint = JSON.stringify(input);
    if (!pendingCreate.current || pendingCreate.current.fingerprint !== fingerprint) {
      pendingCreate.current = {
        version: 1,
        fingerprint,
        input,
        hostIdempotencyKey: newIdempotencyKey(),
        ...(input.workspace_mode === 'new' ? { workspaceIdempotencyKey: newIdempotencyKey() } : {}),
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

  if (templates.isPending) return <div className="loading-card">正在加载主机规格…</div>;
  if (templates.isError) return <div className="error-card" role="alert">{errorDetails(templates.error).message}<button type="button" className="button button-secondary" onClick={() => void templates.refetch()}>重试</button></div>;
  return <section>
    <div className="page-heading"><div><p className="eyebrow">我的主机</p><h1>创建主机</h1><p className="muted">选择运行规格和工作目录。</p></div><Link className="button button-quiet" to="/hosts">返回主机列表</Link></div>
    {recoveryError && <div className="notice notice-warning" role="alert">无法读取本机上次创建记录。为避免重复创建，请先到主机列表核实结果，再选择清除记录。<button type="button" className="button button-secondary" onClick={abandonRecovery}>我已核实，清除记录</button></div>}
    {!templates.data?.items.length ? <div className="empty-card"><h2>暂无可用规格</h2><p>请联系管理员配置主机模板。</p></div> : <form className="form-card form-stack" onSubmit={submit}>
      {recoveryPending && !create.isPending && <div className="notice notice-warning" role="status">上次创建结果尚未确认。继续会用相同请求编号核对结果；若已在主机列表确认，可以放弃恢复并重新填写。<button type="button" className="button button-secondary" onClick={abandonRecovery}>我已核实，放弃恢复并重新填写</button></div>}
      <label htmlFor="host-name">主机名称 <span className="muted">（可选）</span></label>
      <input id="host-name" value={name} maxLength={100} disabled={recoveryPending || recoveryError} onChange={(event) => setName(event.target.value)} placeholder="例如：数据分析" />
      <label htmlFor="host-template">主机规格</label>
      <select id="host-template" required value={templateId || template?.id || ''} disabled={recoveryPending || recoveryError} onChange={(event) => setTemplateId(event.target.value)}>
        {templates.data?.items.map((item) => <option value={item.id} key={item.id}>{item.name}</option>)}
      </select>
      {template && <div className="template-summary"><strong>{template.name}</strong><p>{template.description}</p><p>{formatCpu(template.resources.cpu_limit_milli)} · {formatMemory(template.resources.mem_limit_mib)} · 最长运行 {formatDuration(template.ttl_seconds)}</p></div>}
      <fieldset className="fieldset"><legend>工作目录</legend>
        <label className="radio-row"><input type="radio" name="workspace-mode" value="new" checked={workspaceMode === 'new'} disabled={recoveryPending || recoveryError} onChange={() => setWorkspaceMode('new')} />创建新的独立工作区</label>
        <label className="radio-row"><input type="radio" name="workspace-mode" value="existing" checked={workspaceMode === 'existing'} onChange={() => setWorkspaceMode('existing')} disabled={recoveryPending || recoveryError || workspaces.isPending || workspaces.isError} />绑定已有工作区</label>
        {workspaceMode === 'existing' && <div className="form-stack"><span>选择要绑定的工作区</span>{readyWorkspaces.map((workspace) => <label className="checkbox-row" key={workspace.id}><input type="checkbox" checked={workspaceIds.includes(workspace.id)} disabled={recoveryPending || recoveryError} onChange={(event) => setWorkspaceIds((current) => event.target.checked ? [...current, workspace.id] : current.filter((id) => id !== workspace.id))} />{workspace.name || workspace.id} · {formatBytes(workspace.used_bytes)} · 版本 {workspace.revision}</label>)}</div>}
        {workspaces.isPending && <p className="muted" role="status">正在加载工作目录…</p>}
        {workspaces.isError && <div className="error-card" role="alert">{errorDetails(workspaces.error).message}<button type="button" className="button button-secondary" onClick={() => void workspaces.refetch()}>重试</button></div>}
        {workspaceMode === 'existing' && !workspaces.isPending && !workspaces.isError && !readyWorkspaces.length && <p className="muted">目前没有可用的工作区。</p>}
      </fieldset>
      <div className="notice notice-info">新工作区会先独立保存，再绑定到主机；删除主机时工作区内容仍会保留。</div>
      <div className="form-actions"><Link to="/hosts" className="button button-secondary">取消</Link><button className="button button-primary" type="submit" disabled={create.isPending || recoveryError || (workspaceMode === 'existing' && !workspaceIds.length)}>{create.isPending ? '提交中…' : recoveryPending ? '继续上次创建' : '创建主机'}</button></div>
    </form>}
  </section>;
}
