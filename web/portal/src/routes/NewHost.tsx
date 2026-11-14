import { useRef, useState } from 'react';
import type { FormEvent } from 'react';
import { Link, useNavigate } from 'react-router';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { ApiError } from '../api/client';
import { errorDetails, newIdempotencyKey, portalApi } from '../app/api';
import { useAuth } from '../app/auth';
import { useToast } from '../app/toast';
import { FloatingError } from '../components/FloatingError';

type PendingHostCreate = {
  name?: string;
  hostIdempotencyKey: string;
};

type PendingHostCreateRead = {
  pending: PendingHostCreate | null;
  issue: 'legacy' | 'invalid' | null;
};

function pendingCreateKey(userID: string) {
  return `morde.pending-host-create.${userID}`;
}

function isPendingHostCreate(value: unknown): value is PendingHostCreate {
  if (!value || typeof value !== 'object') return false;
  const pending = value as Partial<PendingHostCreate>;
  const validKey = (key: unknown) => typeof key === 'string' && /^[A-Za-z0-9._~-]{8,128}$/.test(key);
  return Object.keys(value).every((key) => key === 'name' || key === 'hostIdempotencyKey') &&
    (pending.name === undefined || typeof pending.name === 'string') &&
    validKey(pending.hostIdempotencyKey);
}

function readPendingHostCreate(userID?: string): PendingHostCreateRead {
  if (!userID) return { pending: null, issue: null };
  try {
    const raw = localStorage.getItem(pendingCreateKey(userID));
    if (!raw) return { pending: null, issue: null };
    const value: unknown = JSON.parse(raw);
    if (isPendingHostCreate(value)) return { pending: value, issue: null };
    if (value && typeof value === 'object' && (value as { version?: unknown }).version === 1) {
      return { pending: null, issue: 'legacy' };
    }
    return { pending: null, issue: 'invalid' };
  } catch {
    return { pending: null, issue: 'invalid' };
  }
}

export function NewHost() {
  const navigate = useNavigate();
  const client = useQueryClient();
  const { user } = useAuth();
  const { notify } = useToast();
  const hosts = useQuery({ queryKey: ['hosts'], queryFn: portalApi.hosts });
  const ownedHostCount = hosts.data?.items.filter((host) => host.owner_id === user?.id).length ?? 0;
  const [restored] = useState(() => readPendingHostCreate(user?.id));
  const [name, setName] = useState(restored.pending?.name ?? '');
  const pendingCreate = useRef<PendingHostCreate | null>(restored.pending);
  const [recoveryPending, setRecoveryPending] = useState(!!restored.pending);
  const [recoveryIssue, setRecoveryIssue] = useState(restored.issue);

  const create = useMutation({
    mutationFn: (pending: PendingHostCreate) =>
      portalApi.createHost({ ...(pending.name ? { name: pending.name } : {}) }, pending.hostIdempotencyKey),
    onSuccess: () => {
      try { localStorage.removeItem(pendingCreateKey(user!.id)); } catch { /* A stale record only replays this same idempotent request. */ }
      pendingCreate.current = null;
      setRecoveryPending(false);
      void client.invalidateQueries({ queryKey: ['hosts'] });
      notify('云端电脑创建已提交，正在等待启动。', 'success');
      navigate('/hosts', { replace: true });
    },
    onError: (error) => {
      const knownRejection = error instanceof ApiError && error.status >= 400 && error.status < 500 && error.status !== 401;
      setRecoveryPending(!knownRejection);
      notify(errorDetails(error).message, 'error');
    },
  });
  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (create.isPending || recoveryIssue || !user) return;
    if (recoveryPending && pendingCreate.current) {
      create.mutate(pendingCreate.current);
      return;
    }
    const normalizedName = name.trim();
    if (!pendingCreate.current || pendingCreate.current.name !== (normalizedName || undefined)) {
      pendingCreate.current = {
        ...(normalizedName ? { name: normalizedName } : {}),
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
      setRecoveryIssue(null);
    } catch {
      notify('无法清除本机的创建恢复记录。', 'error');
    }
  };

  if (hosts.isPending) return <div className="loading-card" role="status">正在检查云端电脑数量…</div>;
  if (hosts.isError) return <FloatingError className="error-card" role="alert">{errorDetails(hosts.error).message}<button type="button" className="button button-secondary" onClick={() => void hosts.refetch()}>重试</button></FloatingError>;
  if (ownedHostCount >= (user?.quota.hosts ?? 0) && !recoveryIssue) return <FloatingError>已达到云端电脑数量上限</FloatingError>;
  return <section>
    {recoveryIssue && <FloatingError>
      {recoveryIssue === 'legacy'
        ? '发现旧版电脑创建恢复记录，不能安全重放。请先检查云端电脑列表，确认上次创建结果，再清除本机记录。'
        : '无法读取本机上次创建记录。为避免重复创建，请先检查云端电脑列表并核实结果，再清除本机记录。'}
      {' '}<Link to="/hosts">检查云端电脑列表</Link>{' '}
      <button type="button" className="button button-secondary" onClick={abandonRecovery}>我已核实，清除记录</button>
    </FloatingError>}
    <form className="form-card form-stack" onSubmit={submit}>
      <div className="design-panel-header"><div><h2>基本配置</h2><p>使用管理员为你设置的规格。</p></div><Link className="button button-secondary" to="/hosts">← 返回云端电脑</Link></div>
      {recoveryPending && !create.isPending && <div className="notice notice-warning" role="status">上次创建结果尚未确认。继续会用相同请求编号核对结果；若已在主机列表确认，可以放弃恢复并重新填写。<button type="button" className="button button-secondary" onClick={abandonRecovery}>我已核实，放弃恢复并重新填写</button></div>}
      <p className="notice notice-info">电脑文件保存在专属的 <code>~/workspace/</code> 目录中。这里的数据随电脑持久保存；项目同步会写入 <code>~/workspace/project/项目名/</code>。</p>
      <label htmlFor="host-name">云端电脑名称 <span className="muted">（可选）</span></label>
      <input id="host-name" value={name} maxLength={100} disabled={recoveryPending || recoveryIssue !== null} onChange={(event) => setName(event.target.value)} placeholder="例如：数据分析" />
      <p className="muted">创建电脑时会自动准备其专属文件目录，无需另外配置。</p>
      <div className="form-actions"><Link to="/hosts" className="button button-secondary">取消</Link><button className="button button-primary" type="submit" disabled={create.isPending || recoveryIssue !== null}>{create.isPending ? '提交中…' : recoveryPending ? '继续上次创建' : '创建云端电脑'}</button></div>
    </form>
  </section>;
}
