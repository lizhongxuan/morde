import { useState } from 'react';
import type { FormEvent } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useParams } from 'react-router';
import { useAuth } from '../../app/auth';
import { errorDetails, portalApi } from '../../app/api';
import { formatBytes, formatDate } from '../../app/format';
import { useToast } from '../../app/toast';

type Purpose = 'full_record' | 'training_contribution';
type Capability = 'view' | 'attachment' | 'export' | 'delete';
type Settings = Awaited<ReturnType<typeof portalApi.adminRecordSettings>>;
type Access = Awaited<ReturnType<typeof portalApi.adminRecordAccess>>;
type RecordPage = Awaited<ReturnType<typeof portalApi.adminRecords>>;
type RecordItem = RecordPage['items'][number];

const capabilities: Capability[] = ['view', 'attachment', 'export', 'delete'];
const capabilityLabels: Record<Capability, string> = {
  view: '查看正文', attachment: '查看附件', export: '导出', delete: '删除',
};
const purposeLabels: Record<Purpose, string> = {
  full_record: '完整记录', training_contribution: '训练样本',
};

function showDownload(blob: Blob, fileName: string) {
  const url = URL.createObjectURL(blob);
  const link = document.createElement('a');
  link.href = url;
  link.download = fileName;
  document.body.append(link);
  link.click();
  link.remove();
  window.setTimeout(() => URL.revokeObjectURL(url), 0);
}

function RecordsError({ error, retry }: { error: unknown; retry: () => void }) {
  return <div className="error-card" role="alert">{errorDetails(error).message}<button type="button" className="button button-secondary" onClick={retry}>重试</button></div>;
}

function ConsentStatus({ consent }: { consent: Access['consents'][number] }) {
  return <article><span>{purposeLabels[consent.purpose as Purpose] ?? consent.purpose}</span><strong>{consent.enabled ? '已启用' : '默认关闭'}</strong><small className="table-sub">版本 v{consent.version}{consent.updated_at ? ` · ${formatDate(consent.updated_at)}` : ''}</small></article>;
}

function RecordList({ items, selectedID, select }: { items: RecordItem[]; selectedID: string | null; select: (item: RecordItem) => void }) {
  if (!items.length) return <div className="empty-card compact" role="status"><p>当前同意范围内没有可访问的记录或样本。</p></div>;
  return <div className="table-wrap"><table><thead><tr><th>类别</th><th>创建时间</th><th>大小</th><th>附件</th><th>同意版本</th><th>到期时间</th><th>操作</th></tr></thead><tbody>
    {items.map((item) => <tr key={item.id}>
      <td><strong>{purposeLabels[item.purpose as Purpose] ?? item.purpose}</strong><small className="table-sub">{item.kind === 'sample' ? '样本' : '对话记录'} · {item.id}</small></td>
      <td>{formatDate(item.created_at)}</td><td>{formatBytes(item.payload_bytes)}</td><td>{item.attachment_count}</td>
      <td>v{item.consent_version}</td><td>{formatDate(item.expires_at)}</td>
      <td><button type="button" className="button button-secondary button-small" aria-label={`查看 ${item.id}`} aria-pressed={selectedID === item.id} onClick={() => select(item)}>查看</button></td>
    </tr>)}
  </tbody></table></div>;
}

function SuperAdminSettings({ ownerID, settings, refresh }: { ownerID: string; settings: Settings; refresh: () => void }) {
  const { notify } = useToast();
  const [retentionDays, setRetentionDays] = useState(String(settings.quota.retention_days));
  const [limitBytes, setLimitBytes] = useState(String(settings.quota.limit_bytes));
  const [adminID, setAdminID] = useState('');
  const [capability, setCapability] = useState<Capability>('view');
  const [granted, setGranted] = useState(true);
  const quota = useMutation({
    mutationFn: () => portalApi.updateRecordQuota(ownerID, {
      retention_days: Number(retentionDays), limit_bytes: Number(limitBytes), expected_version: settings.quota.version,
    }),
    onSuccess: () => { notify('记录额度已保存。', 'success'); refresh(); },
    onError: (error) => notify(errorDetails(error).message, 'error'),
  });
  const grant = useMutation({
    mutationFn: (body: { admin_id: string; capability: Capability; granted: boolean }) => portalApi.updateRecordGrant(ownerID, body),
    onSuccess: () => { notify('记录访问权限已更新。', 'success'); setAdminID(''); refresh(); },
    onError: (error) => notify(errorDetails(error).message, 'error'),
  });
  const submitQuota = (event: FormEvent<HTMLFormElement>) => { event.preventDefault(); quota.mutate(); };
  const submitGrant = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (adminID.trim()) grant.mutate({ admin_id: adminID.trim(), capability, granted });
  };
  return <>
    <section className="admin-detail-section" aria-labelledby="admin-record-quota-heading">
      <h2 id="admin-record-quota-heading">记录额度</h2>
      <div className="admin-detail-facts">
        <article><span>已使用</span><strong>{formatBytes(settings.quota.used_bytes)}</strong></article>
        <article><span>已预留</span><strong>{formatBytes(settings.quota.reserved_bytes)}</strong></article>
        <article><span>当前上限</span><strong>{formatBytes(settings.quota.limit_bytes)}</strong></article>
      </div>
      <form className="form-card form-stack" onSubmit={submitQuota}>
        <label htmlFor="record-retention-days">保留天数</label><input id="record-retention-days" type="number" min="1" max="3650" step="1" required value={retentionDays} onChange={(event) => setRetentionDays(event.target.value)} />
        <label htmlFor="record-limit-bytes">总额度（字节）</label><input id="record-limit-bytes" type="number" min="1" step="1" required value={limitBytes} onChange={(event) => setLimitBytes(event.target.value)} />
        <div className="form-actions"><button className="button button-primary" type="submit" disabled={quota.isPending}>保存记录额度</button></div>
      </form>
    </section>
    <section className="admin-detail-section" aria-labelledby="admin-record-grants-heading">
      <h2 id="admin-record-grants-heading">管理员访问授权</h2>
      {!settings.grants.length ? <div className="empty-card compact" role="status"><p>尚无管理员授权。</p></div> : <div className="table-wrap"><table><thead><tr><th>管理员 ID</th><th>权限</th><th>状态</th><th>更新时间</th><th>操作</th></tr></thead><tbody>
        {settings.grants.map((item) => <tr key={`${item.admin_id}:${item.capability}`}><td className="mono">{item.admin_id}</td><td>{capabilityLabels[item.capability as Capability] ?? item.capability}</td><td>{item.granted ? '已授权' : '已撤销'}</td><td>{formatDate(item.updated_at)}</td><td><button type="button" className="button button-secondary button-small" disabled={grant.isPending} onClick={() => grant.mutate({ admin_id: item.admin_id, capability: item.capability as Capability, granted: !item.granted })}>{item.granted ? '撤销' : '恢复'}</button></td></tr>)}
      </tbody></table></div>}
      <form className="form-card form-stack" onSubmit={submitGrant}>
        <label htmlFor="record-grant-admin-id">管理员账号 ID</label><input id="record-grant-admin-id" required value={adminID} onChange={(event) => setAdminID(event.target.value)} />
        <label htmlFor="record-grant-capability">权限</label><select id="record-grant-capability" value={capability} onChange={(event) => setCapability(event.target.value as Capability)}>{capabilities.map((item) => <option key={item} value={item}>{capabilityLabels[item]}</option>)}</select>
        <label htmlFor="record-grant-status">状态</label><select id="record-grant-status" value={granted ? 'granted' : 'revoked'} onChange={(event) => setGranted(event.target.value === 'granted')}><option value="granted">授权</option><option value="revoked">撤销</option></select>
        <p className="muted">服务端会校验目标账号必须是已通过的管理员。授权不会替用户开启记录同意。</p>
        <div className="form-actions"><button className="button button-primary" type="submit" disabled={grant.isPending}>更新授权</button></div>
      </form>
    </section>
  </>;
}

export function AdminRecords() {
  const { id = '' } = useParams();
  const { user: current } = useAuth();
  const { notify } = useToast();
  const client = useQueryClient();
  const [purpose, setPurpose] = useState<'all' | Purpose>('all');
  const [beforeIDs, setBeforeIDs] = useState<string[]>([]);
  const [selected, setSelected] = useState<RecordItem | null>(null);
  const [confirmDelete, setConfirmDelete] = useState(false);
  const isSuperAdmin = current?.role === 'super_admin';
  const accessQuery = useQuery({ queryKey: ['admin', 'record-access', id], queryFn: () => portalApi.adminRecordAccess(id), enabled: !!id });
  const settingsQuery = useQuery({ queryKey: ['admin', 'record-settings', id], queryFn: () => portalApi.adminRecordSettings(id), enabled: !!id && isSuperAdmin });
  const hasCapability = (value: Capability) => accessQuery.data?.capabilities.includes(value) ?? false;
  const consentFor = (value: Purpose) => accessQuery.data?.consents.find((item) => item.purpose === value);
  const enabledConsent = purpose === 'all'
    ? accessQuery.data?.consents.some((item) => item.enabled) ?? false
    : consentFor(purpose)?.enabled ?? false;
  const recordsQuery = useQuery({
    queryKey: ['admin', 'records', id, purpose, beforeIDs.at(-1)],
    queryFn: () => portalApi.adminRecords(id, { limit: 50, ...(purpose === 'all' ? {} : { purpose }), ...(beforeIDs.length ? { before_id: beforeIDs.at(-1)! } : {}) }),
    enabled: !!id && accessQuery.isSuccess && hasCapability('view') && enabledConsent,
  });
  const detailQuery = useQuery({
    queryKey: ['admin', 'record', id, selected?.id],
    queryFn: () => portalApi.adminRecord(id, selected!.id),
    enabled: !!id && !!selected && hasCapability('view'),
  });
  const attachmentsQuery = useQuery({
    queryKey: ['admin', 'record-attachments', id, selected?.id],
    queryFn: () => portalApi.adminRecordAttachments(id, selected!.id),
    enabled: !!selected && hasCapability('attachment'),
  });
  const refresh = () => {
    void client.invalidateQueries({ queryKey: ['admin', 'record-settings', id] });
    void client.invalidateQueries({ queryKey: ['admin', 'record-access', id] });
    void client.invalidateQueries({ queryKey: ['admin', 'records', id] });
  };
  const remove = useMutation({
    mutationFn: () => portalApi.deleteAdminRecord(id, selected!.id),
    onSuccess: () => {
      notify('删除请求已提交，记录内容将在后台清理。', 'success');
      setSelected(null);
      setConfirmDelete(false);
      refresh();
    },
    onError: (error) => notify(errorDetails(error).message, 'error'),
  });
  const exportRecord = useMutation({
    mutationFn: (recordID: string) => portalApi.adminRecordExport(id, recordID),
    onSuccess: (blob, recordID) => {
      showDownload(blob, `record-${recordID}.json`);
      notify('记录导出已保存到本机。', 'success');
    },
    onError: (error) => notify(errorDetails(error).message, 'error'),
  });
  const downloadAttachment = useMutation({
    mutationFn: async ({ recordID, attachmentID, fileName }: { recordID: string; attachmentID: string; fileName: string }) => ({
      blob: await portalApi.adminRecordAttachment(id, recordID, attachmentID), fileName,
    }),
    onSuccess: ({ blob, fileName }) => { showDownload(blob, fileName); notify('附件已保存到本机。', 'success'); },
    onError: (error) => notify(errorDetails(error).message, 'error'),
  });

  return <section className="admin-content">
    <div className="page-heading"><div><p className="eyebrow">账号记录</p><h1>记录与样本</h1><p className="muted">完整记录和训练样本使用独立同意；当前页面只显示获授权的元数据和操作。</p></div></div>
    {accessQuery.isPending ? <div className="loading-card" role="status">正在读取记录权限…</div>
      : accessQuery.isError ? <RecordsError error={accessQuery.error} retry={() => void accessQuery.refetch()} />
        : <>
          <section className="admin-detail-section" aria-labelledby="record-consent-heading">
            <h2 id="record-consent-heading">用户同意状态</h2>
            <div className="admin-detail-facts">{accessQuery.data.consents.map((item) => <ConsentStatus key={item.purpose} consent={item} />)}</div>
            <p className="muted">同意由用户本人管理。撤回或关闭同意后，页面不会显示相应正文。</p>
            <div className="admin-detail-facts" aria-label="当前管理员权限">{capabilities.map((item) => <article key={item}><span>{capabilityLabels[item]}</span><strong>{hasCapability(item) ? '已授权' : '无权限'}</strong></article>)}</div>
          </section>
          {isSuperAdmin && settingsQuery.isPending ? <div className="loading-card" role="status">正在读取记录设置…</div>
            : isSuperAdmin && settingsQuery.isError ? <RecordsError error={settingsQuery.error} retry={() => void settingsQuery.refetch()} />
              : isSuperAdmin && settingsQuery.data ? <SuperAdminSettings ownerID={id} settings={settingsQuery.data} refresh={refresh} /> : null}
          <section className="admin-detail-section" aria-labelledby="admin-record-list-heading">
            <div className="page-heading"><div><h2 id="admin-record-list-heading">记录列表</h2><p className="muted">仅元数据列表；正文须单独点击并经过 view 授权。</p></div>
              <div className="device-manager-actions"><label htmlFor="record-purpose-filter">记录类别</label><select id="record-purpose-filter" value={purpose} onChange={(event) => { setPurpose(event.target.value as 'all' | Purpose); setBeforeIDs([]); setSelected(null); setConfirmDelete(false); }}><option value="all">全部</option><option value="full_record">完整记录</option><option value="training_contribution">训练样本</option></select></div>
            </div>
            {!hasCapability('view') ? <div className="empty-card compact" role="status"><p>当前管理员没有查看记录的授权。</p></div>
              : !enabledConsent ? <div className="empty-card compact" role="status"><p>此类别的记录同意默认关闭，当前没有可访问的记录。</p></div>
                : recordsQuery.isPending ? <div className="loading-card" role="status">正在加载记录元数据…</div>
                  : recordsQuery.isError ? <RecordsError error={recordsQuery.error} retry={() => void recordsQuery.refetch()} />
                    : <>
                      <RecordList items={recordsQuery.data.items} selectedID={selected?.id ?? null} select={setSelected} />
                      {(beforeIDs.length > 0 || recordsQuery.data.next_before) && <nav className="admin-pagination" aria-label="记录分页">
                        <span>{recordsQuery.data.items.length} 条记录</span>
                        <div className="inline-actions"><button type="button" className="button button-secondary button-small" disabled={!beforeIDs.length || recordsQuery.isFetching} onClick={() => { setBeforeIDs(beforeIDs.slice(0, -1)); setSelected(null); }}>上一页</button>
                          <button type="button" className="button button-secondary button-small" disabled={!recordsQuery.data.next_before || recordsQuery.isFetching} onClick={() => { const cursor = recordsQuery.data.next_before; if (cursor) setBeforeIDs([...beforeIDs, cursor]); setSelected(null); }}>下一页</button></div>
                      </nav>}
                    </>}
          </section>
          {selected && <section className="admin-detail-section" aria-labelledby="admin-record-detail-heading">
            <div className="page-heading"><div><h2 id="admin-record-detail-heading">记录详情</h2><p className="muted">{selected.id} · {purposeLabels[selected.purpose as Purpose] ?? selected.purpose}</p></div></div>
            {detailQuery.isPending ? <div className="loading-card" role="status">正在读取正文…</div>
              : detailQuery.isError ? <RecordsError error={detailQuery.error} retry={() => void detailQuery.refetch()} />
                : <pre className="audit-detail">{typeof detailQuery.data.payload === 'string' ? detailQuery.data.payload : JSON.stringify(detailQuery.data.payload, null, 2)}</pre>}
            <div className="inline-actions">
              {hasCapability('attachment') && selected.attachment_count > 0 && <span className="muted">附件列表在授权后显示。</span>}
              {!hasCapability('attachment') && selected.attachment_count > 0 && <span className="muted">当前管理员没有附件权限。</span>}
              {hasCapability('export') && <button type="button" className="button button-secondary" disabled={exportRecord.isPending} onClick={() => exportRecord.mutate(selected.id)}>导出 JSON</button>}
              {!hasCapability('export') && <span className="muted">当前管理员没有导出权限。</span>}
              {hasCapability('delete') && !confirmDelete && <button type="button" className="button button-danger-quiet" onClick={() => setConfirmDelete(true)}>删除记录</button>}
              {!hasCapability('delete') && <span className="muted">当前管理员没有删除权限。</span>}
            </div>
            {hasCapability('attachment') && selected.attachment_count > 0 && <section className="admin-detail-section" aria-labelledby="record-attachments-heading"><h3 id="record-attachments-heading">附件</h3>
              {attachmentsQuery.isPending ? <div className="loading-card" role="status">正在读取附件…</div>
                : attachmentsQuery.isError ? <RecordsError error={attachmentsQuery.error} retry={() => void attachmentsQuery.refetch()} />
                  : attachmentsQuery.data.items.length ? <ul>{attachmentsQuery.data.items.map((item) => <li key={item.id}>{item.file_name} · {formatBytes(item.size_bytes)} <button type="button" className="button button-secondary button-small" disabled={downloadAttachment.isPending} onClick={() => downloadAttachment.mutate({ recordID: selected.id, attachmentID: item.id, fileName: item.file_name })}>下载到本机</button></li>)}</ul>
                    : <div className="empty-card compact" role="status"><p>此记录没有可访问的附件。</p></div>}
            </section>}
            {confirmDelete && <div className="notice notice-warning" role="group" aria-label="确认删除记录"><p>删除后正文和专用附件会进入后台清理队列，不能撤销。</p><div className="inline-actions"><button type="button" className="button button-secondary" disabled={remove.isPending} onClick={() => setConfirmDelete(false)}>取消</button><button type="button" className="button button-danger-quiet" disabled={remove.isPending} onClick={() => remove.mutate()}>{remove.isPending ? '提交中…' : '确认删除记录'}</button></div></div>}
          </section>}
        </>}
  </section>;
}
