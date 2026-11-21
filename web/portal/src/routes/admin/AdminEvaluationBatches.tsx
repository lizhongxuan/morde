import { useMemo, useState, type FormEvent } from 'react';
import { useInfiniteQuery, useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useSearchParams } from 'react-router';
import frozenSuiteJSON from '../../../../../server/internal/evaluation/testdata/suite-v1.json';
import type { components, operations } from '../../api/schema';
import { errorDetails, portalApi } from '../../app/api';
import './AdminEvaluationBatches.css';

type EvaluationRun = components['schemas']['EvaluationRun'];
type ParticipantRole = 'implementers' | 'reviewers' | 'arbitrators';
type Roster = Record<ParticipantRole, string[]>;
type AdminUser = Awaited<ReturnType<typeof portalApi.adminUsers>>['items'][number];

const frozenSuite = frozenSuiteJSON as components['schemas']['EvaluationSuite'];
const roles: { key: ParticipantRole; title: string; minimum: number }[] = [
  { key: 'implementers', title: '实现者', minimum: 1 },
  { key: 'reviewers', title: '独立审阅者', minimum: 2 },
  { key: 'arbitrators', title: '仲裁者', minimum: 1 },
];

function adminEvaluationError(error: unknown) {
  return errorDetails(error).message || '盲测批次暂时无法读取。';
}

export function AdminEvaluationBatches() {
  const queryClient = useQueryClient();
  const [searchParams, setSearchParams] = useSearchParams();
  const packID = searchParams.get('pack') ?? '';
  const [roster, setRoster] = useState<Roster>({ implementers: [], reviewers: [], arbitrators: [] });
  const [runsJSON, setRunsJSON] = useState('');
  const [packLookup, setPackLookup] = useState(packID);
  const [formError, setFormError] = useState('');
  const users = useInfiniteQuery({
    queryKey: ['admin', 'blind-evaluation-users'],
    initialPageParam: 0,
    queryFn: ({ pageParam }) => portalApi.adminUsers('approved', pageParam),
    getNextPageParam: (page) => page.offset + page.items.length < page.total ? page.offset + page.items.length : undefined,
  });
  const candidates = useMemo(() => users.data?.pages.flatMap((page) => page.items).filter((item) => item.status === 'approved') ?? [], [users.data]);
  const assignedIDs = roles.flatMap((role) => roster[role.key]);
  const hasOverlap = new Set(assignedIDs).size !== assignedIDs.length;
  const selectedCount = (role: ParticipantRole) => roster[role].length;
  const create = useMutation({
    mutationFn: (body: operations['createBlindEvaluationBatch']['requestBody']['content']['application/json']) => portalApi.createBlindEvaluationBatch(body),
    onSuccess: async (pack) => {
      setFormError('');
      setPackLookup(pack.id);
      setSearchParams({ pack: pack.id }, { replace: true });
      await queryClient.invalidateQueries({ queryKey: ['admin', 'blind-evaluation', pack.id] });
    },
  });
  const panel = useQuery({
    queryKey: ['admin', 'blind-evaluation', packID], queryFn: () => portalApi.blindEvaluationPanel(packID), enabled: Boolean(packID),
  });
  const mapping = useQuery({
    queryKey: ['admin', 'blind-evaluation-mapping', packID], queryFn: () => portalApi.blindEvaluationMapping(packID), enabled: Boolean(packID),
  });
  const manifests = useQuery({
    queryKey: ['admin', 'blind-evaluation-manifests', packID], queryFn: () => portalApi.blindEvaluationManifests(packID), enabled: Boolean(packID),
  });

  const setParticipant = (role: ParticipantRole, userID: string, checked: boolean) => setRoster((current) => ({
    ...current, [role]: checked ? [...current[role], userID] : current[role].filter((id) => id !== userID),
  }));
  const setLookup = () => {
    const id = packLookup.trim();
    if (!/^[0-9a-f]{32}$/.test(id)) {
      setFormError('请输入 32 位盲测批次 ID。');
      return;
    }
    setFormError('');
    setSearchParams({ pack: id });
  };
  const createBatch = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    setFormError('');
    if (hasOverlap) return setFormError('实现者、审阅者和仲裁者必须是不同账号。');
    if (roles.some((role) => roster[role.key].length < role.minimum)) return setFormError('请至少选择 1 名实现者、2 名独立审阅者和 1 名仲裁者。');
    let runs: EvaluationRun[];
    try {
      const parsed: unknown = JSON.parse(runsJSON);
      if (!Array.isArray(parsed) || parsed.length < 2) throw new Error('请提供至少 2 份候选运行结果。');
      runs = parsed as EvaluationRun[];
    } catch (error) {
      return setFormError(error instanceof Error && error.message.startsWith('请提供') ? error.message : '运行结果 JSON 格式无效，请检查后重试。');
    }
    create.mutate({ suite: frozenSuite, runs, ...roster });
  };

  const statusLabel = panel.data?.ready ? '已通过盲测门禁' : '尚未就绪';
  const reviewersPath = packID ? `/evaluation-batches/${packID}/review` : '';
  const arbitrationPath = packID ? `/evaluation-batches/${packID}/arbitration` : '';

  return <section className="admin-content admin-blind-evaluations">
    <header className="admin-blind-heading"><div><h2>匿名盲测批次</h2><p>固定使用仓库的合成套件；候选配置映射仅管理员可见。</p></div></header>
    <section className="admin-detail-section" aria-labelledby="blind-batch-create-heading">
      <h2 id="blind-batch-create-heading">创建批次</h2>
      <div className="blind-suite-summary"><strong>{frozenSuite.version}</strong><span>{frozenSuite.cases.length} 个冻结用例 · {frozenSuite.rubric.length} 项 Rubric · 合成数据</span></div>
      <p className="admin-blind-privacy">只提交本套件生成的合成运行结果；不要粘贴真实用户对话、文件内容、凭据或敏感日志。服务器会重新校验套件、版本、参与者和证据锚点。</p>
      <form onSubmit={createBatch}>
        <div className="blind-roster-grid">{roles.map((role) => <fieldset className="blind-roster-card" key={role.key}>
          <legend>{role.title} <span>已选 {selectedCount(role.key)} · 至少 {role.minimum} 名</span></legend>
          {candidates.length ? <div className="blind-user-list">{candidates.map((user: AdminUser) => <label key={`${role.key}-${user.id}`}>
            <input type="checkbox" checked={roster[role.key].includes(user.id)} onChange={(event) => setParticipant(role.key, user.id, event.target.checked)} />
            <span>{user.name}</span><small>{user.id}</small>
          </label>)}</div> : users.isPending ? <p className="muted" role="status">正在加载已审核账号…</p>
            : users.isError ? <p className="error-inline" role="alert">读取账号失败：{adminEvaluationError(users.error)}</p>
              : <p className="muted">暂无已通过审核的账号。</p>}
        </fieldset>)}</div>
        {users.hasNextPage && <button type="button" className="button button-secondary" disabled={users.isFetchingNextPage} onClick={() => void users.fetchNextPage()}>
          {users.isFetchingNextPage ? '正在加载…' : '加载更多账号'}
        </button>}
        {users.isError && candidates.length > 0 && <p className="error-inline" role="alert">读取后续账号失败：{adminEvaluationError(users.error)}</p>}
        <label className="blind-runs-field">候选运行结果 JSON<textarea rows={12} value={runsJSON} onChange={(event) => setRunsJSON(event.target.value)}
          placeholder={`粘贴至少 2 份 ${frozenSuite.version} 运行结果 JSON 数组；每份需包含 manifest 和覆盖全部用例的 cases。`} spellCheck={false} /></label>
        <div className="blind-batch-actions"><button className="button button-primary" type="submit" disabled={create.isPending || users.isPending || hasOverlap}>
          {create.isPending ? '正在创建…' : '创建匿名批次'}
        </button></div>
      </form>
      {(formError || create.isError) && <p className="error-inline" role="alert">{formError || `创建失败：${adminEvaluationError(create.error)}`}</p>}
      {create.data && <p className="blind-created-notice" role="status">批次已创建：{create.data.id}。请保存批次 ID，并将对应入口交给已分配参与者。</p>}
    </section>

    <section className="admin-detail-section" aria-labelledby="blind-batch-inspect-heading">
      <h2 id="blind-batch-inspect-heading">查看批次</h2>
      <div className="blind-pack-lookup"><label htmlFor="blind-pack-lookup">批次 ID</label><input id="blind-pack-lookup" value={packLookup} onChange={(event) => setPackLookup(event.target.value)} placeholder="32 位十六进制 ID" />
        <button type="button" className="button button-secondary" onClick={setLookup}>读取批次</button></div>
      {packID && <>
        {panel.isPending && <p className="muted" role="status">正在读取批次状态…</p>}
        {panel.isError && <p className="error-inline" role="alert">读取批次失败：{adminEvaluationError(panel.error)}</p>}
        {panel.data && <div className="blind-batch-result">
          <article><span>状态</span><strong>{statusLabel}</strong><p>{panel.data.pending_reason || '已完成全部盲测和分歧处理。'}</p></article>
          <article><span>独立评审记录</span><strong>{panel.data.review_count}</strong></article>
          <article><span>仲裁记录</span><strong>{panel.data.arbitration_count}</strong></article>
        </div>}
        <div className="blind-review-links">
          <a className="button button-secondary" href={reviewersPath} target="_blank" rel="noreferrer">打开审阅入口</a>
          <a className="button button-secondary" href={arbitrationPath} target="_blank" rel="noreferrer">打开仲裁入口</a>
        </div>
        {mapping.data && <details className="blind-admin-data"><summary>查看匿名映射（管理员专属）</summary><div className="table-wrap"><table><thead><tr><th>匿名标签</th><th>运行 ID</th><th>用例位置</th></tr></thead><tbody>{mapping.data.assignments.map((item) => <tr key={item.label}><td>{item.label}</td><td className="mono">{item.run_id}</td><td>{item.positions.join(', ')}</td></tr>)}</tbody></table></div></details>}
        {mapping.isError && <p className="error-inline" role="alert">映射表读取失败：{adminEvaluationError(mapping.error)}</p>}
        {manifests.data && <details className="blind-admin-data"><summary>查看运行版本清单（管理员专属）</summary><div className="table-wrap"><table><thead><tr><th>运行 ID</th><th>模型</th><th>Provider</th><th>构建</th></tr></thead><tbody>{manifests.data.items.map((item) => <tr key={item.run_id}><td className="mono">{item.run_id}</td><td>{item.model}</td><td>{item.provider}</td><td className="mono">{item.build_sha}</td></tr>)}</tbody></table></div></details>}
        {manifests.isError && <p className="error-inline" role="alert">版本清单读取失败：{adminEvaluationError(manifests.error)}</p>}
      </>}
    </section>
    <p className="muted admin-blind-limit">候选 Run 的模型执行和版本信息自动采集尚未接入；当前必须使用受控、合成的 {frozenSuite.version} 运行结果。</p>
  </section>;
}
