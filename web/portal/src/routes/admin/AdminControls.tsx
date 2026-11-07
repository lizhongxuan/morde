import { Fragment, useState } from 'react';
import type { FormEvent } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { errorDetails, portalApi } from '../../app/api';
import { formatDate } from '../../app/format';

type ControlsData = Awaited<ReturnType<typeof portalApi.adminControls>>;
type ControlRule = ControlsData['rules'][number];
type RuleDraft = { threshold: string; enabled: boolean; note: string; rearm: boolean };
type CreateBody = Parameters<typeof portalApi.createAdminControlRule>[0];
type UpdateBody = Parameters<typeof portalApi.updateAdminControlRule>[1];
type Evaluation = Awaited<ReturnType<typeof portalApi.evaluateAdminControls>>;
type NotifyTest = Awaited<ReturnType<typeof portalApi.adminControlNotifyTest>>;

const formatNumber = (value: number) => new Intl.NumberFormat('zh-CN').format(value);

function parseThreshold(value: string): number | null {
  const threshold = Number(value);
  return Number.isSafeInteger(threshold) && threshold > 0 ? threshold : null;
}

function errorMessage(error: unknown) {
  return errorDetails(error).message;
}

function isConflict(error: unknown) {
  const value = error instanceof Error ? error as Error & { code?: string; status?: number } : undefined;
  return value?.code === 'conflict' || value?.status === 409;
}

function ErrorCard({ error, retry }: { error: unknown; retry: () => void }) {
  return <div className="error-card" role="alert">
    {errorMessage(error)}
    <button type="button" className="button button-secondary" onClick={retry}>重试</button>
  </div>;
}

function draftFor(rule: ControlRule): RuleDraft {
  return { threshold: String(rule.threshold), enabled: rule.enabled, note: rule.note, rearm: false };
}

function RuleEditor({
  rule,
  draft,
  setDraft,
  confirming,
  pending,
  validationError,
  mutationError,
  onSubmit,
  onConfirm,
  onCancel,
}: {
  rule: ControlRule;
  draft: RuleDraft;
  setDraft: (value: RuleDraft) => void;
  confirming: boolean;
  pending: boolean;
  validationError: string;
  mutationError: unknown;
  onSubmit: (event: FormEvent<HTMLFormElement>) => void;
  onConfirm: () => void;
  onCancel: () => void;
}) {
  if (confirming) return <div className="notice notice-warning" role="group" aria-label={`确认修改通知规则 ${rule.id}`}>
    <p>确认将规则 #{rule.id} 更新为账号数 {draft.threshold}、{draft.enabled ? '启用' : '停用'}？{draft.rearm ? '并重新武装规则。' : ''}</p>
    {!!mutationError && <p role="alert">{errorMessage(mutationError)}</p>}
    <div className="inline-actions"><button type="button" className="button button-secondary button-small" disabled={pending} onClick={onCancel}>取消</button><button type="button" className="button button-primary button-small" disabled={pending} onClick={onConfirm}>{pending ? '保存中…' : '确认保存'}</button></div>
  </div>;

  return <form className="connection-form" onSubmit={onSubmit}>
    <div className="connection-form-grid">
      <div><label htmlFor={`control-edit-threshold-${rule.id}`}>账号阈值</label><input id={`control-edit-threshold-${rule.id}`} type="number" min={1} step={1} required value={draft.threshold} onChange={(event) => setDraft({ ...draft, threshold: event.target.value })} /></div>
      <div><label htmlFor={`control-edit-enabled-${rule.id}`}>规则状态</label><select id={`control-edit-enabled-${rule.id}`} value={draft.enabled ? 'enabled' : 'disabled'} onChange={(event) => setDraft({ ...draft, enabled: event.target.value === 'enabled' })}><option value="enabled">启用</option><option value="disabled">停用</option></select></div>
      <div className="connection-form-wide"><label htmlFor={`control-edit-note-${rule.id}`}>备注</label><input id={`control-edit-note-${rule.id}`} value={draft.note} onChange={(event) => setDraft({ ...draft, note: event.target.value })} /></div>
      {rule.last_fired_at && <label className="connection-form-wide"><input type="checkbox" checked={draft.rearm} onChange={(event) => setDraft({ ...draft, rearm: event.target.checked })} /> 重新武装此规则，允许它再次触发</label>}
    </div>
    {validationError && <p role="alert" className="form-error">{validationError}</p>}
    {!!mutationError && <p role="alert" className="form-error">{errorMessage(mutationError)}</p>}
    <div className="form-actions"><button type="button" className="button button-secondary" onClick={onCancel}>取消</button><button type="submit" className="button button-primary">继续保存</button></div>
  </form>;
}

export function AdminControls() {
  const client = useQueryClient();
  const queryKey = ['admin', 'controls'];
  const query = useQuery({ queryKey, queryFn: portalApi.adminControls });
  const [createDraft, setCreateDraft] = useState({ threshold: '', note: '' });
  const [createConfirmation, setCreateConfirmation] = useState<CreateBody | null>(null);
  const [createError, setCreateError] = useState('');
  const [createValidationError, setCreateValidationError] = useState('');
  const [edit, setEdit] = useState<{ rule: ControlRule; draft: RuleDraft; confirming: boolean } | null>(null);
  const [editValidationError, setEditValidationError] = useState('');
  const [deleteRule, setDeleteRule] = useState<ControlRule | null>(null);
  const [versionConflict, setVersionConflict] = useState(false);
  const [notice, setNotice] = useState('');
  const [evaluation, setEvaluation] = useState<Evaluation | null>(null);
  const [notifyResult, setNotifyResult] = useState<NotifyTest | null>(null);

  const refresh = async () => { await client.invalidateQueries({ queryKey }); };
  const handleMutationError = (error: unknown) => { if (isConflict(error)) setVersionConflict(true); };
  const create = useMutation({
    mutationFn: (body: CreateBody) => portalApi.createAdminControlRule(body),
    onSuccess: async () => {
      setCreateConfirmation(null);
      setCreateDraft({ threshold: '', note: '' });
      setCreateError('');
      setNotice('通知规则已创建。');
      await refresh();
    },
    onError: (error) => { setCreateError(errorMessage(error)); handleMutationError(error); },
  });
  const update = useMutation({
    mutationFn: ({ id, body }: { id: number; body: UpdateBody }) => portalApi.updateAdminControlRule(id, body),
    onSuccess: async () => {
      setEdit(null);
      setEditValidationError('');
      setNotice('通知规则已保存。');
      await refresh();
    },
    onError: handleMutationError,
  });
  const remove = useMutation({
    mutationFn: (rule: ControlRule) => portalApi.deleteAdminControlRule(rule.id, rule.version),
    onSuccess: async () => {
      setDeleteRule(null);
      setNotice('通知规则已删除。');
      await refresh();
    },
    onError: handleMutationError,
  });
  const evaluate = useMutation({
    mutationFn: () => portalApi.evaluateAdminControls(),
    onSuccess: async (result) => {
      setEvaluation(result);
      setNotice('阈值规则评估完成。');
      await refresh();
    },
    onError: handleMutationError,
  });
  const notifyTest = useMutation({
    mutationFn: () => portalApi.adminControlNotifyTest(),
    onSuccess: async (result) => {
      setNotifyResult(result);
      setNotice('本地接收箱测试完成。');
      await refresh();
    },
    onError: handleMutationError,
  });

  async function reloadAfterConflict() {
    const result = await query.refetch();
    if (!result.isError) {
      setVersionConflict(false);
      setEdit(null);
      setDeleteRule(null);
      setCreateConfirmation(null);
      setCreateError('');
    }
  }

  function submitCreate(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setCreateError('');
    const threshold = parseThreshold(createDraft.threshold);
    if (threshold === null) {
      setCreateValidationError('阈值必须是大于零的整数。');
      return;
    }
    setCreateValidationError('');
    setNotice('');
    setCreateConfirmation({ threshold, note: createDraft.note.trim() });
  }

  function submitEdit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (!edit) return;
    const threshold = parseThreshold(edit.draft.threshold);
    if (threshold === null) {
      setEditValidationError('阈值必须是大于零的整数。');
      return;
    }
    setEditValidationError('');
    setNotice('');
    setEdit({ ...edit, draft: { ...edit.draft, threshold: String(threshold), note: edit.draft.note.trim() }, confirming: true });
  }

  function confirmEdit() {
    if (!edit) return;
    const body: UpdateBody = {
      expected_version: edit.rule.version,
      threshold: Number(edit.draft.threshold),
      enabled: edit.draft.enabled,
      note: edit.draft.note,
      rearm: edit.draft.rearm,
    };
    update.mutate({ id: edit.rule.id, body });
  }

  const data = query.data;
  const changeDisabled = versionConflict || !data?.notify_configured || create.isPending || update.isPending || remove.isPending;

  return <section className="admin-content">
    {versionConflict && <div className="error-card" role="alert">规则版本已变化。重新加载最新状态后，才能继续修改或删除。<button type="button" className="button button-secondary" onClick={() => void reloadAfterConflict()}>重新加载</button></div>}
    {notice && <p className="notice notice-info" role="status">{notice}</p>}

    {query.isPending ? <div className="loading-card" role="status">正在加载通知规则…</div>
      : query.isError ? <ErrorCard error={query.error} retry={() => void query.refetch()} />
        : data && <>
          <section className="admin-detail-section" aria-labelledby="admin-controls-summary-heading">
            <div className="design-panel-header"><div><h2 id="admin-controls-summary-heading">阈值通知概览</h2><p>所有通知只写入 Morde 本地接收箱，不会发送到外部服务。</p></div><button type="button" className="button button-secondary" disabled={query.isFetching} onClick={() => void query.refetch()}>{query.isFetching ? '正在刷新…' : '刷新数据'}</button></div>
            <div className="admin-detail-facts">
              <article><span>账号总数</span><strong>{formatNumber(data.accounts_total)}</strong></article>
              <article><span>下一触发阈值</span><strong>{data.next_threshold === null ? '暂无' : formatNumber(data.next_threshold)}</strong></article>
              <article><span>本地接收箱</span><strong>{data.notify_configured ? '已配置' : '未配置'}</strong><p className="muted">不连接或发送至外部通知服务。</p></article>
            </div>
            {!data.notify_configured && <div className="empty-card compact" role="status"><p>本地接收箱未配置；新增、启用和手动测试暂不可用。</p></div>}
          </section>

          <section className="admin-detail-section" aria-labelledby="admin-control-create-heading">
            <h2 id="admin-control-create-heading">新增通知规则</h2>
            <p className="muted">规则只支持 notify 动作；达到账号阈值时写入本地接收箱，不会关闭注册、暂停服务或修改额度。</p>
            <form className="form-card" onSubmit={(event) => void submitCreate(event)}>
              <div className="connection-form-grid">
                <div><label htmlFor="admin-control-threshold">账号阈值</label><input id="admin-control-threshold" type="number" min={1} step={1} required value={createDraft.threshold} onChange={(event) => setCreateDraft({ ...createDraft, threshold: event.target.value })} /></div>
                <div className="connection-form-wide"><label htmlFor="admin-control-note">备注</label><input id="admin-control-note" value={createDraft.note} onChange={(event) => setCreateDraft({ ...createDraft, note: event.target.value })} /></div>
              </div>
              {createValidationError && <p className="form-error" role="alert">{createValidationError}</p>}
              {createError && <p className="form-error" role="alert">{createError}</p>}
              <button type="submit" className="button button-primary" disabled={changeDisabled}>继续创建</button>
            </form>
            {createConfirmation && <div className="notice notice-warning" role="group" aria-label="确认创建通知规则">
              <p>确认创建账号数达到 {formatNumber(createConfirmation.threshold)} 时通知本地接收箱的规则？</p>
              <div className="inline-actions"><button type="button" className="button button-secondary button-small" disabled={create.isPending} onClick={() => setCreateConfirmation(null)}>取消</button><button type="button" className="button button-primary button-small" disabled={changeDisabled} onClick={() => create.mutate(createConfirmation)}>{create.isPending ? '创建中…' : '确认创建'}</button></div>
            </div>}
          </section>

          <section className="admin-detail-section" aria-labelledby="admin-control-rules-heading">
            <h2 id="admin-control-rules-heading">通知规则</h2>
            {!data.rules.length ? <div className="empty-card compact" role="status"><p>尚未创建通知规则。</p></div>
              : <div className="table-wrap"><table><thead><tr><th>阈值</th><th>状态</th><th>备注</th><th>最近触发</th><th>版本</th><th>操作</th></tr></thead><tbody>
                {data.rules.map((rule) => <Fragment key={rule.id}>
                  <tr>
                    <td>{formatNumber(rule.threshold)} 个账号</td><td>{rule.enabled ? '已启用' : '已停用'}</td><td>{rule.note || '—'}</td>
                    <td>{rule.last_fired_at ? <>{formatDate(rule.last_fired_at)}<small className="table-sub">触发时账号 {formatNumber(rule.fired_accounts)}</small></> : '尚未触发'}</td><td>v{rule.version}</td>
                    <td><div className="inline-actions"><button type="button" className="button button-secondary button-small" disabled={changeDisabled || edit !== null || deleteRule !== null} onClick={() => { setEdit({ rule, draft: draftFor(rule), confirming: false }); update.reset(); }}>编辑</button><button type="button" className="button button-danger-quiet button-small" disabled={changeDisabled || edit !== null || deleteRule !== null} onClick={() => { setDeleteRule(rule); remove.reset(); }}>删除</button></div></td>
                  </tr>
                  {edit?.rule.id === rule.id && <tr><td colSpan={6}><RuleEditor rule={edit.rule} draft={edit.draft} setDraft={(draft) => setEdit({ ...edit, draft })} confirming={edit.confirming} pending={update.isPending} validationError={editValidationError} mutationError={update.error} onSubmit={submitEdit} onConfirm={confirmEdit} onCancel={() => { setEdit(null); update.reset(); }} /></td></tr>}
                  {deleteRule?.id === rule.id && <tr><td colSpan={6}><div className="notice notice-warning" role="group" aria-label={`确认删除通知规则 ${rule.id}`}>
                    <p>确认删除阈值为 {formatNumber(rule.threshold)} 的通知规则？此操作会阻止该规则后续触发。</p>
                    {remove.isError && <p role="alert">{errorMessage(remove.error)}</p>}
                    <div className="inline-actions"><button type="button" className="button button-secondary button-small" disabled={remove.isPending} onClick={() => { setDeleteRule(null); remove.reset(); }}>取消</button><button type="button" className="button button-danger-quiet button-small" disabled={changeDisabled} onClick={() => remove.mutate(rule)}>{remove.isPending ? '正在删除…' : '确认删除'}</button></div>
                  </div></td></tr>}
                </Fragment>)}
              </tbody></table></div>}
          </section>

          <section className="admin-detail-section" aria-labelledby="admin-control-evaluate-heading">
            <h2 id="admin-control-evaluate-heading">手动评估</h2>
            <p className="muted">仅评估通知阈值并写入本地接收箱，不执行服务开关操作。</p>
            <button type="button" className="button button-secondary" disabled={!data.notify_configured || evaluate.isPending} onClick={() => { setNotice(''); evaluate.mutate(); }}>{evaluate.isPending ? '正在评估…' : '立即评估'}</button>
            {evaluate.isError && <ErrorCard error={evaluate.error} retry={() => evaluate.mutate()} />}
            {evaluation && <p className="notice notice-info" role="status">评估了 {formatNumber(evaluation.accounts_total)} 个账号，触发 {formatNumber(evaluation.fired.length)} 条通知规则。</p>}
          </section>

          <section className="admin-detail-section" aria-labelledby="admin-control-notify-test-heading">
            <h2 id="admin-control-notify-test-heading">本地通知检查</h2>
            <p className="muted">测试结果只写入 Morde 本地接收箱，不会发送邮件、短信或调用外部通知服务。</p>
            <button type="button" className="button button-secondary" disabled={!data.notify_configured || notifyTest.isPending} onClick={() => { setNotice(''); setNotifyResult(null); notifyTest.mutate(); }}>{notifyTest.isPending ? '正在检查…' : '写入测试通知'}</button>
            {notifyTest.isError && <ErrorCard error={notifyTest.error} retry={() => notifyTest.mutate()} />}
            {notifyResult && <p className="notice notice-info" role="status">{notifyResult.sent && notifyResult.configured ? `测试通知已写入本地接收箱 · ${formatDate(notifyResult.created_at)}` : '本地通知测试未完成。'}</p>}
          </section>

          <section className="admin-detail-section" aria-labelledby="admin-control-inbox-heading">
            <h2 id="admin-control-inbox-heading">本地接收箱</h2>
            {!data.notifications.length ? <div className="empty-card compact" role="status"><p>暂无本地通知。</p></div>
              : <div className="table-wrap"><table><thead><tr><th>时间</th><th>类型</th><th>规则</th><th>账号数</th></tr></thead><tbody>
                {data.notifications.map((item, index) => <tr key={`${item.rule_id}:${item.created_at}:${index}`}><td>{formatDate(item.created_at)}</td><td><code>{item.kind}</code></td><td>{item.rule_id === null ? '—' : `#${item.rule_id}`}</td><td>{formatNumber(item.accounts_total)}</td></tr>)}
              </tbody></table></div>}
          </section>
        </>}
  </section>;
}
