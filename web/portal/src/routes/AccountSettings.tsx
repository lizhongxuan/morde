import { useInfiniteQuery, useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useEffect, useState } from 'react';
import { Link } from 'react-router';
import { errorDetails, portalApi } from '../app/api';
import { useAuth } from '../app/auth';
import { formatBytes, formatDate } from '../app/format';
import { useToast } from '../app/toast';
import type { components } from '../api/schema';
import {
  subscribePersonalDataSyncStatus,
  type PersonalDataSyncStatus,
} from './personal-data-store';
import { FloatingError } from '../components/FloatingError';

const consentLabels = { full_record: '完整会话记录', training_contribution: '训练样本贡献' } as const;
type RecordCaptureStatus = components['schemas']['RecordCaptureStatus'];

const capturePurposeLabels: Record<RecordCaptureStatus['purpose'], string> = {
  full_record: '完整会话记录',
  training_contribution: '训练样本贡献',
};
const captureKindLabels: Record<RecordCaptureStatus['kind'], string> = { turn: '对话轮次', sample: '训练样本' };
const captureStateLabels: Record<RecordCaptureStatus['state'], string> = {
  queued: '等待持久化', leased: '等待持久化', stored: '已保存', rejected: '未保存', failed: '未保存',
};
const captureStateClasses: Record<RecordCaptureStatus['state'], string> = {
  queued: 'status-queued', leased: 'status-queued', stored: 'status-ready', rejected: 'status-failed', failed: 'status-failed',
};

function safeFailureCode(code: string): string | undefined {
  return /^[a-z0-9_]{1,64}$/.test(code) ? code : undefined;
}

function RecordConsentSettings() {
  const client = useQueryClient();
  const { notify } = useToast();
  const query = useQuery({ queryKey: ['me', 'record-consents'], queryFn: portalApi.recordConsents });
  const update = useMutation({
    mutationFn: portalApi.setRecordConsent,
    onSuccess: async () => {
      notify('记录同意设置已保存。', 'success');
      await client.invalidateQueries({ queryKey: ['me', 'record-consents'] });
    },
    onError: (error) => notify(errorDetails(error).message, 'error'),
  });

  return <section className="connection-section" aria-labelledby="record-consent-settings-title">
    <div className="connection-section-heading"><div><h2 id="record-consent-settings-title">记录与样本同意</h2><p className="muted">两类记录分别控制，默认关闭。关闭后会清理待处理内容，已保存的记录仍可由你申请删除。</p></div></div>
    {query.isPending ? <div className="loading-card" role="status">正在读取记录同意设置…</div>
      : query.isError ? <FloatingError className="error-card" role="alert">{errorDetails(query.error).message}<button type="button" className="button button-secondary" onClick={() => void query.refetch()}>重试</button></FloatingError>
        : <fieldset className="form-card form-stack" disabled={update.isPending}>
          <legend className="sr-only">选择记录保存方式</legend>
          {query.data.items.map((consent) => <label className="checkbox-row" key={consent.purpose}>
            <input type="checkbox" checked={consent.enabled} onChange={(event) => update.mutate({ purpose: consent.purpose, enabled: event.target.checked })} />
            允许保存{consentLabels[consent.purpose]}
          </label>)}
          {update.isPending && <p role="status" className="muted">正在保存…</p>}
        </fieldset>}
  </section>;
}

function MyRecordCaptureStatuses() {
  const [pageIndex, setPageIndex] = useState(0);
  const query = useInfiniteQuery({
    queryKey: ['me', 'record-capture-statuses'],
    initialPageParam: undefined as string | undefined,
    queryFn: ({ pageParam }) => portalApi.myRecordCaptureStatuses({ limit: 4, ...(pageParam ? { before_id: pageParam } : {}) }),
    getNextPageParam: (page) => page.next_before || undefined,
    refetchInterval: (current) => current.state.data?.pages.some((page) => page.items.some((item) => item.state === 'queued' || item.state === 'leased')) ? 3000 : false,
  });
  const items = query.data?.pages[pageIndex]?.items ?? [];
  const nextPage = async () => {
    const targetPage = pageIndex + 1;
    if (targetPage >= (query.data?.pages.length ?? 0)) {
      if (!query.hasNextPage) return;
      const result = await query.fetchNextPage();
      if (result.isError || !result.data?.pages[targetPage]) return;
    }
    setPageIndex(targetPage);
  };

  return <section className="connection-section" aria-labelledby="record-capture-status-title">
    <div className="connection-section-heading"><div><h2 id="record-capture-status-title">任务记录采集状态</h2><p className="muted">这里只显示任务记录的状态和时间等元数据，不显示记录正文或附件信息。</p></div></div>
    {query.isPending ? <div className="loading-card" role="status">正在读取任务记录状态…</div>
      : query.isError && !query.data ? <FloatingError className="error-card" role="alert">{errorDetails(query.error).message}<button type="button" className="button button-secondary" onClick={() => void query.refetch()}>重试</button></FloatingError>
        : <>
          {query.isError && <FloatingError className="error-card" role="alert">{errorDetails(query.error).message}<button type="button" className="button button-secondary" onClick={() => void query.refetch()}>重试</button></FloatingError>}
          {items.length === 0 ? <div className="empty-card"><h3>没有任务记录状态</h3><p className="muted">任务记录只会在你单独同意后提交。</p></div>
            : <div className="form-stack">{items.map((item) => {
              const failed = item.state === 'failed' || item.state === 'rejected';
              const failureCode = failed ? safeFailureCode(item.failure_code) : undefined;
              return <article className="model-connection-card" key={item.id}>
                <div className="model-connection-main">
                  <div className="model-connection-title"><h3>{capturePurposeLabels[item.purpose]}</h3><span className={`status ${captureStateClasses[item.state]}`}>{captureStateLabels[item.state]}</span></div>
                  <p className="muted">{captureKindLabels[item.kind]} · 采集于 {formatDate(item.captured_at)} · 到期 {formatDate(item.expires_at)}</p>
                  {failed && <p className="muted">未保存原因代码：{failureCode ? <code>{failureCode}</code> : '暂不可用'}</p>}
                </div>
              </article>;
            })}
              <div className="inline-actions" role="group" aria-label="任务记录状态分页">
                <button type="button" className="button button-secondary" disabled={pageIndex === 0} onClick={() => setPageIndex((current) => Math.max(0, current - 1))}>上一页</button>
                <span className="muted">第 {pageIndex + 1} 页</span>
                <button type="button" className="button button-secondary" disabled={!query.hasNextPage && pageIndex + 1 >= (query.data?.pages.length ?? 0) || query.isFetchingNextPage} onClick={() => void nextPage()}>{query.isFetchingNextPage ? '正在读取…' : '下一页'}</button>
              </div>
            </div>}
        </>}
  </section>;
}

function MyRecordSettings() {
  const client = useQueryClient();
  const { notify } = useToast();
  const [confirmDelete, setConfirmDelete] = useState<string | null>(null);
  const [pageIndex, setPageIndex] = useState(0);
  const [deleting, setDeleting] = useState<Record<string, string>>({});
  const records = useInfiniteQuery({
    queryKey: ['me', 'records'],
    initialPageParam: undefined as string | undefined,
    queryFn: ({ pageParam }) => portalApi.myRecords({ limit: 4, ...(pageParam ? { before_id: pageParam } : {}) }),
    getNextPageParam: (page) => page.next_before || undefined,
  });
  const remove = useMutation({
    mutationFn: portalApi.deleteMyRecord,
    onSuccess: async (tombstone) => {
      setDeleting((current) => ({ ...current, [tombstone.record_id]: tombstone.state }));
      setConfirmDelete(null);
      notify(tombstone.state === 'completed' ? '记录已清理完成。' : '删除请求已受理，记录正文和专用附件正在清理。', 'success');
      await client.invalidateQueries({ queryKey: ['me', 'records'] });
    },
    onError: (error) => notify(errorDetails(error).message, 'error'),
  });
  const items = records.data?.pages[pageIndex]?.items ?? [];
  const nextPage = async () => {
    const targetPage = pageIndex + 1;
    if (targetPage >= (records.data?.pages.length ?? 0)) {
      if (!records.hasNextPage) return;
      const result = await records.fetchNextPage();
      if (result.isError || !result.data?.pages[targetPage]) return;
    }
    setPageIndex(targetPage);
  };

  return <section className="connection-section" aria-labelledby="my-records-title">
    <div className="connection-section-heading"><div><h2 id="my-records-title">已保存记录</h2><p className="muted">这里只列出记录元数据。撤回同意不会删除已保存内容；你可以逐条请求清理正文与记录专用附件。</p></div></div>
    {records.isPending ? <div className="loading-card" role="status">正在读取已保存记录…</div>
      : records.isError ? <FloatingError className="error-card" role="alert">{errorDetails(records.error).message}<button type="button" className="button button-secondary" onClick={() => void records.refetch()}>重试</button></FloatingError>
        : items.length === 0 ? <div className="empty-card"><h3>没有已保存记录</h3><p className="muted">新的记录仅在你单独同意后保存。</p></div>
          : <div className="form-stack">{items.map((item) => <article className="model-connection-card" key={item.id}>
            <div className="model-connection-main"><div className="model-connection-title"><h3>{item.purpose === 'full_record' ? '完整会话记录' : '训练样本'}</h3><span className={`status ${deleting[item.id] && deleting[item.id] !== 'completed' ? 'status-deleting' : 'status-ready'}`}>{deleting[item.id] === 'completed' ? '已清理' : deleting[item.id] ? '清理中' : '已保存'}</span></div>
              <p className="muted">{formatDate(item.created_at)} · {formatBytes(item.payload_bytes)} · {item.attachment_count} 个专用附件 · 到期 {formatDate(item.expires_at)}</p>
              {deleting[item.id] && <p className="muted" role="status">删除请求已受理；后台清理完成前，记录仍可能短暂显示在列表中。</p>}
            </div>
            {!deleting[item.id] && !confirmDelete && <button type="button" className="button button-danger-quiet" onClick={() => setConfirmDelete(item.id)}>删除记录</button>}
            {confirmDelete === item.id && <div className="notice notice-warning" role="group" aria-label={`确认删除记录 ${item.id}`}><p>将清理这条记录的正文和专用附件，此操作不能撤销，不会删除普通会话或资源库附件。</p><div className="inline-actions"><button type="button" className="button button-secondary" disabled={remove.isPending} onClick={() => setConfirmDelete(null)}>取消</button><button type="button" className="button button-danger-quiet" disabled={remove.isPending} onClick={() => remove.mutate(item.id)}>{remove.isPending ? '提交中…' : '确认删除记录'}</button></div></div>}
          </article>)}
          <div className="inline-actions" role="group" aria-label="已保存记录分页">
            <button type="button" className="button button-secondary" disabled={pageIndex === 0} onClick={() => setPageIndex((current) => Math.max(0, current - 1))}>上一页</button>
            <span className="muted">第 {pageIndex + 1} 页</span>
            <button type="button" className="button button-secondary" disabled={!records.hasNextPage && pageIndex + 1 >= (records.data?.pages.length ?? 0) || records.isFetchingNextPage} onClick={() => void nextPage()}>{records.isFetchingNextPage ? '正在读取…' : '下一页'}</button>
          </div>
        </div>}
  </section>;
}

function PersonalSyncSettings() {
  const { user } = useAuth();
  const userID = user?.id ?? null;
  const capabilities = useQuery({ queryKey: ['service-capabilities'], queryFn: portalApi.capabilities });
  const serviceInstanceID = capabilities.data?.service_instance_id;
  const [storedStatus, setStoredStatus] = useState<PersonalDataSyncStatus & { userID: string | null }>(() => ({
    userID, pendingCount: null, conflictCount: null,
  }));
  const status = storedStatus.userID === userID ? storedStatus : {
    userID, pendingCount: null, conflictCount: null,
  };
  const quota = useQuery({ queryKey: ['personal-data-quota', serviceInstanceID], queryFn: portalApi.personalDataQuota,
    enabled: Boolean(serviceInstanceID), refetchInterval: serviceInstanceID ? 30000 : false });

  useEffect(() => {
    if (!userID || !serviceInstanceID) return;
    const origin = window.location.origin;
    const unsubscribeStatus = subscribePersonalDataSyncStatus(userID, (next) => setStoredStatus({ ...next, userID }), origin, serviceInstanceID);
    return unsubscribeStatus;
  }, [userID, serviceInstanceID]);

  return <section className="connection-section" aria-labelledby="personal-sync-settings-title">
    <div className="connection-section-heading"><div><h2 id="personal-sync-settings-title">个人数据保存</h2><p className="muted">保存后自动更新云端；手机连接后会获取最新资料。</p></div></div>
    <div className="form-card form-stack">
      <p role="status" className="notice notice-info">{status.pendingCount === null || status.conflictCount === null ? '本机状态正在读取' : `待提交 ${status.pendingCount} 项 · 冲突 ${status.conflictCount} 项`}</p>
      {status.error && <FloatingError className="error-inline" role="alert">无法读取本机同步队列：{status.error}</FloatingError>}
      {status.conflictCount !== null && status.conflictCount > 0 && <p className="notice notice-warning">存在未解决冲突；本地草稿已保留，请到<Link to="/personal">个人数据</Link>查看并处理。</p>}
    </div>
    <div className="connection-section-heading"><div><h3>个人数据额度</h3><p className="muted">包括个人对象、计划和个人附件，不包括聊天、云端电脑文件和完整记录。</p></div></div>
    {quota.isPending ? <div className="loading-card" role="status">正在读取个人数据额度…</div>
      : quota.isError ? <FloatingError className="error-card" role="alert">{errorDetails(quota.error).message}<button type="button" className="button button-secondary" onClick={() => void quota.refetch()}>重试</button></FloatingError>
        : <p className="notice notice-info" role="status">已用 {formatBytes(quota.data.used_bytes)}，待上传预留 {formatBytes(quota.data.reserved_bytes)}，总额度 {formatBytes(quota.data.quota_bytes)}，可用 {formatBytes(quota.data.remaining_bytes)}；单附件上限 {formatBytes(quota.data.max_attachment_bytes)}。</p>}
  </section>;
}

const roleLabels: Record<string, string> = {
  user: '普通用户', admin: '管理员', super_admin: '超级管理员',
};

const accountSections = [
  { id: 'account', title: '账号' },
  { id: 'privacy', title: '隐私与同意' },
  { id: 'personal-data', title: '个人数据' },
  { id: 'record-status', title: '记录状态' },
  { id: 'saved-records', title: '已保存记录' },
] as const;
type AccountSectionID = typeof accountSections[number]['id'];

export function AccountSettings() {
  const { user } = useAuth();
  const [activeSection, setActiveSection] = useState<AccountSectionID>('account');

  return <section>
    <div className="page-heading">
      <div><h1>账户设置</h1></div>
    </div>
    <nav className="subnav account-settings-tabs" role="tablist" aria-label="账户设置分区">
      {accountSections.map((item) => <button key={item.id} type="button" role="tab" id={`account-settings-tab-${item.id}`}
        aria-selected={activeSection === item.id} aria-controls={`account-settings-panel-${item.id}`}
        className={activeSection === item.id ? 'button button-primary' : 'button button-secondary'} onClick={() => setActiveSection(item.id)}>{item.title}</button>)}
    </nav>
    <section role="tabpanel" id="account-settings-panel-account" aria-labelledby="account-settings-tab-account" hidden={activeSection !== 'account'}>
      <section className="connection-section" aria-labelledby="account-info-title">
        <div className="connection-section-heading"><div><h2 id="account-info-title">当前账号</h2><p className="muted">账号权限由服务端管理。</p></div></div>
        <article className="model-connection-card">
          <div className="model-connection-main">
            <div className="model-connection-title"><h3>{user?.name || '当前账号'}</h3><span className="status status-ready">{user ? roleLabels[user.role] ?? '已登录' : '加载中'}</span></div>
            <p className="muted">修改密码后，现有登录会话会被撤销，需要在设备上重新登录。</p>
          </div>
          <Link className="button button-secondary" to="/settings/password">修改密码</Link>
        </article>
        <div className="connection-section-heading account-clients-link"><div><h2>已登录设备</h2><p className="muted">查看或撤销已登录的电脑和手机。</p></div><Link className="button button-secondary" to="/clients">管理客户端</Link></div>
      </section>
    </section>
    <section role="tabpanel" id="account-settings-panel-privacy" aria-labelledby="account-settings-tab-privacy" hidden={activeSection !== 'privacy'}>
      <RecordConsentSettings />
    </section>
    <section role="tabpanel" id="account-settings-panel-personal-data" aria-labelledby="account-settings-tab-personal-data" hidden={activeSection !== 'personal-data'}>
      <PersonalSyncSettings />
    </section>
    <section role="tabpanel" id="account-settings-panel-record-status" aria-labelledby="account-settings-tab-record-status" hidden={activeSection !== 'record-status'}>
      <MyRecordCaptureStatuses />
    </section>
    <section role="tabpanel" id="account-settings-panel-saved-records" aria-labelledby="account-settings-tab-saved-records" hidden={activeSection !== 'saved-records'}>
      <MyRecordSettings />
    </section>
  </section>;
}
