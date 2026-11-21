import { useMemo, useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useLocation, useParams } from 'react-router';
import type { components, operations } from '../api/schema';
import { errorDetails, portalApi } from '../app/api';
import './BlindEvaluationWorkspace.css';

type BlindPack = components['schemas']['BlindPack'];
type EvaluationScore = components['schemas']['EvaluationScore'];
type Judgment = components['schemas']['EvaluationJudgment'];
type JudgmentDraft = { score: EvaluationScore | ''; rationale: string; evidenceID: string; quote: string };
type ArbitrationBundle = Awaited<ReturnType<typeof portalApi.blindArbitrationBundle>>;

function createDraft(pack: BlindPack): JudgmentDraft[][] {
  return pack.cases.map((testCase) => pack.rubric.map((criterion) => ({
    score: testCase.applicability.find((item) => item.criterion_id === criterion.id)?.applicable === false ? 'N/A' : '',
    rationale: '', evidenceID: '', quote: '',
  })));
}

function scoreReviewsDisagree(reviews: components['schemas']['BlindArbitrationReview'][]) {
  if (reviews.length < 2) return false;
  const first = reviews[0];
  if (!first) return false;
  return reviews.slice(1).some((review) => review.cases.some((item, caseIndex) =>
    item.judgments.some((judgment, judgmentIndex) => judgment.score !== first.cases[caseIndex]?.judgments[judgmentIndex]?.score)));
}

function toCaseReviews(pack: BlindPack, candidateLabel: string, draft: JudgmentDraft[][]): components['schemas']['BlindCaseReview'][] {
  return pack.cases.map((testCase, caseIndex) => ({
    case_id: testCase.case_id,
    judgments: pack.rubric.map((criterion, rubricIndex): Judgment => {
      const item = draft[caseIndex]?.[rubricIndex];
      if (!item?.rationale.trim()) throw new Error(`请补充“${criterion.name}”的评分理由。`);
      if (!item.score) throw new Error(`请为“${criterion.name}”选择 0 或 1。`);
      if (item.score === 'N/A') return {
        criterion_id: criterion.id, score: item.score, rationale: item.rationale.trim(),
        evidence: [{ kind: 'suite_na_reason', id: `${testCase.case_id}/${criterion.id}`, start_byte: 0, end_byte: 0 }],
      };
      if (item.evidenceID) return {
        criterion_id: criterion.id, score: item.score, rationale: item.rationale.trim(),
        evidence: [{ kind: 'evidence_id', id: item.evidenceID, start_byte: 0, end_byte: 0 }],
      };
      const output = testCase.candidates.find((candidate) => candidate.label === candidateLabel)?.output;
      const quote = item.quote;
      const start = output?.indexOf(quote) ?? -1;
      if (!quote.trim() || start < 0 || output?.indexOf(quote, start + quote.length) !== -1) {
        throw new Error(`请为“${criterion.name}”填写一段在回答中唯一出现的原文，或选择已登记证据。`);
      }
      const startByte = new TextEncoder().encode(output.slice(0, start)).length;
      const endByte = startByte + new TextEncoder().encode(quote).length;
      return { criterion_id: criterion.id, score: item.score, rationale: item.rationale.trim(),
        evidence: [{ kind: 'output_span', start_byte: startByte, end_byte: endByte }] };
    }),
  }));
}

export function BlindEvaluationWorkspace() {
  const { pack_id: packID = '' } = useParams();
  const location = useLocation();
  const arbitrationMode = location.pathname.endsWith('/arbitration');
  const queryClient = useQueryClient();
  const [candidateLabel, setCandidateLabel] = useState('');
  const [drafts, setDrafts] = useState<Record<string, JudgmentDraft[][]>>({});
  const reviewer = useQuery({
    queryKey: ['blind-review', packID], queryFn: () => portalApi.blindReviewBundle(packID),
    enabled: Boolean(packID) && !arbitrationMode,
  });
  const arbitrator = useQuery({
    queryKey: ['blind-arbitration', packID], queryFn: () => portalApi.blindArbitrationBundle(packID),
    enabled: Boolean(packID) && arbitrationMode,
  });
  const bundle = arbitrationMode ? arbitrator.data : reviewer.data;
  const pack = bundle?.pack;
  const labels = useMemo(() => pack?.cases[0]?.candidates.map((candidate) => candidate.label) ?? [], [pack]);
  const selected = labels.includes(candidateLabel) ? candidateLabel : labels[0] ?? '';
  const draft = selected && pack ? drafts[selected] ?? createDraft(pack) : [];
  const savedReview = reviewer.data?.reviews.find((item) => item.candidate === selected);
  const arbitrationReviews = arbitrationMode
    ? (arbitrator.data as ArbitrationBundle | undefined)?.reviews.filter((item) => item.candidate === selected) ?? [] : [];
  const savedArbitration = arbitrationMode
    ? (arbitrator.data as ArbitrationBundle | undefined)?.arbitrations.find((item) => item.candidate === selected) : undefined;
  const reviewsComplete = arbitrationReviews.length === arbitrator.data?.expected_reviews_per_candidate;
  const disagreement = scoreReviewsDisagree(arbitrationReviews);
  const submitReview = useMutation({
    mutationFn: (body: operations['submitBlindReview']['requestBody']['content']['application/json']) => portalApi.submitBlindReview(packID, body),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ['blind-review', packID] }),
  });
  const submitArbitration = useMutation({
    mutationFn: (body: operations['submitBlindArbitration']['requestBody']['content']['application/json']) => portalApi.submitBlindArbitration(packID, body),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ['blind-arbitration', packID] }),
  });
  const query = arbitrationMode ? arbitrator : reviewer;
  const error = query.isError ? errorDetails(query.error).message : '';
  const updateJudgment = (caseIndex: number, rubricIndex: number, update: Partial<JudgmentDraft>) => {
    if (!pack || !selected) return;
    setDrafts((current) => {
      const next = current[selected] ? current[selected].map((items) => [...items]) : createDraft(pack);
      const row = next[caseIndex] ?? [];
      row[rubricIndex] = { score: '', rationale: '', evidenceID: '', quote: '', ...row[rubricIndex], ...update };
      next[caseIndex] = row;
      return { ...current, [selected]: next };
    });
  };
  const submit = () => {
    if (!pack || !selected) return;
    try {
      const cases = toCaseReviews(pack, selected, draft);
      if (arbitrationMode) {
        const body: operations['submitBlindArbitration']['requestBody']['content']['application/json'] = {
          candidate: selected,
          reason: arbitrationReason.trim(),
          cases,
        };
        if (!body.reason) throw new Error('请说明仲裁结论依据。');
        submitArbitration.mutate(body);
      } else {
        submitReview.mutate({ candidate: selected, cases });
      }
    } catch (caught) {
      setFormError(caught instanceof Error ? caught.message : '评分内容无效。');
    }
  };
  const [formError, setFormError] = useState('');
  const [arbitrationReason, setArbitrationReason] = useState('');
  const mutationError = submitReview.isError ? errorDetails(submitReview.error).message
    : submitArbitration.isError ? errorDetails(submitArbitration.error).message : '';

  return <main className="blind-workspace page-content">
    <header className="page-heading blind-heading">
      <div><p className="eyebrow">独立评测</p><h1>{arbitrationMode ? '盲测分歧仲裁' : '盲测评审'}</h1>
        <p>{arbitrationMode ? '只依据匿名评审、用例要求和可核查证据作出裁决。' : '独立核对每个候选回答；每项评分都要附理由和证据。'}</p></div>
    </header>
    {query.isPending && <p className="loading-card" role="status">正在读取盲测内容…</p>}
    {error && <div className="error-card" role="alert"><span>{error}</span><button className="button button-secondary" onClick={() => void query.refetch()}>重试</button></div>}
    {pack && <>
      <section className="blind-candidate-picker" aria-label="选择匿名候选">
        <label htmlFor="blind-candidate">匿名候选</label>
        <select id="blind-candidate" value={selected} onChange={(event) => setCandidateLabel(event.target.value)}>
          {labels.map((label) => <option key={label} value={label}>{label}</option>)}
        </select>
        <span>{pack.suite_version} · {pack.cases.length} 个用例</span>
      </section>
      {savedReview && !arbitrationMode && <p className="blind-status" role="status">已提交该候选的评分。提交记录已保存，不能重复修改。</p>}
      {savedArbitration && arbitrationMode && <p className="blind-status" role="status">该候选已完成仲裁：{savedArbitration.reason}</p>}
      {arbitrationMode && !savedArbitration && !reviewsComplete && <p className="blind-status" role="status">等待评审完成：{arbitrationReviews.length}/{arbitrator.data?.expected_reviews_per_candidate ?? 0} 份。</p>}
      {arbitrationMode && reviewsComplete && !disagreement && <p className="blind-status" role="status">评审结论一致，无需仲裁。</p>}
      {pack.cases.map((testCase, caseIndex) => {
        const candidate = testCase.candidates.find((item) => item.label === selected);
        if (!candidate) return null;
        const savedCase = savedReview?.cases.find((item) => item.case_id === testCase.case_id);
        return <article className="blind-case" key={testCase.case_id}>
          <header className="blind-case-heading"><div><span className="blind-case-priority">{testCase.priority}</span><strong>{testCase.case_id}</strong></div><div>{testCase.tags.map((tag) => <span className="blind-tag" key={tag}>{tag}</span>)}</div></header>
          <section className="blind-case-context"><h2>用户请求与验收要求</h2><p><strong>请求</strong><br />{testCase.input}</p><p><strong>预期行为</strong><br />{testCase.expected_behavior}</p></section>
          <section className="blind-answer"><h2>候选回答 <span>{candidate.label}</span></h2><pre>{candidate.output || '（空回答）'}</pre>
            {candidate.evidence.length > 0 && <details><summary>已登记证据（{candidate.evidence.length}）</summary><ul>{candidate.evidence.map((item) => <li key={item.id}><strong>{item.kind}</strong> · {item.id}{item.excerpt && <blockquote>{item.excerpt}</blockquote>}</li>)}</ul></details>}
          </section>
          {arbitrationMode && <section className="blind-peer-reviews" aria-label={`${testCase.case_id} 匿名评审意见`}>
            <h2>独立评审意见</h2>{arbitrationReviews.map((review) => <article key={review.reviewer}><h3>{review.reviewer}</h3>
              {review.cases.find((item) => item.case_id === testCase.case_id)?.judgments.map((judgment) => <p key={judgment.criterion_id}><strong>{pack.rubric.find((item) => item.id === judgment.criterion_id)?.name}: {judgment.score}</strong> · {judgment.rationale}</p>)}
            </article>)}
          </section>}
          <section className="blind-rubric"><h2>逐项评分</h2>{pack.rubric.map((criterion, rubricIndex) => {
            const applicability = testCase.applicability.find((item) => item.criterion_id === criterion.id);
            const item = draft[caseIndex]?.[rubricIndex] ?? { score: '' as const, rationale: '', evidenceID: '', quote: '' };
            const savedJudgment = savedCase?.judgments.find((judgment) => judgment.criterion_id === criterion.id);
            const resolvedJudgment = savedArbitration?.cases.find((reviewCase) => reviewCase.case_id === testCase.case_id)
              ?.judgments.find((judgment) => judgment.criterion_id === criterion.id);
            return <fieldset className="blind-criterion" key={criterion.id} disabled={Boolean(savedReview) || Boolean(savedArbitration) || (arbitrationMode && (!reviewsComplete || !disagreement))}>
              <legend>{criterion.name}{criterion.hard_gate && <span className="blind-hard-gate">硬门禁</span>}</legend>
              {!applicability?.applicable && <p className="blind-na-reason">不适用：{applicability?.not_applicable_reason}</p>}
              {savedJudgment || resolvedJudgment ? <p className="blind-saved-score">{resolvedJudgment && <strong>仲裁评分 </strong>}{(resolvedJudgment ?? savedJudgment)?.score} · {(resolvedJudgment ?? savedJudgment)?.rationale}</p> : <>
                <label>评分<select value={!applicability?.applicable ? 'N/A' : item.score} disabled={!applicability?.applicable} onChange={(event) => updateJudgment(caseIndex, rubricIndex, { score: event.target.value as EvaluationScore })}>
                  {applicability?.applicable ? <><option value="">请选择</option><option value="0">0 · 不符合</option><option value="1">1 · 符合</option></> : <option value="N/A">N/A · 不适用</option>}
                </select></label>
                <label>评分理由<textarea rows={2} value={item.rationale} onChange={(event) => updateJudgment(caseIndex, rubricIndex, { rationale: event.target.value })} /></label>
                {applicability?.applicable && <>
                  <label>证据来源<select value={item.evidenceID ? `evidence:${item.evidenceID}` : 'output'} onChange={(event) => updateJudgment(caseIndex, rubricIndex, { evidenceID: event.target.value.startsWith('evidence:') ? event.target.value.slice(9) : '' })}>
                    <option value="output">候选回答原文</option>{candidate.evidence.map((evidence) => <option key={evidence.id} value={`evidence:${evidence.id}`}>已登记证据：{evidence.id}</option>)}
                  </select></label>
                  {!item.evidenceID && <label>引用回答中的原文<textarea rows={2} value={item.quote} onChange={(event) => updateJudgment(caseIndex, rubricIndex, { quote: event.target.value })} placeholder="粘贴一段在候选回答中唯一出现的原文" /></label>}
                </>}
              </>}
            </fieldset>;
          })}</section>
        </article>;
      })}
      {arbitrationMode && reviewsComplete && disagreement && !savedArbitration && <label className="blind-arbitration-reason">仲裁结论与依据<textarea rows={3} value={arbitrationReason} onChange={(event) => setArbitrationReason(event.target.value)} placeholder="说明如何依据用例要求和引用证据解决评分分歧" /></label>}
      {(formError || mutationError) && <p className="blind-form-error" role="alert">{formError || mutationError}</p>}
      {(!arbitrationMode && !savedReview || arbitrationMode && reviewsComplete && disagreement && !savedArbitration) && <div className="blind-submit-row">
        <button type="button" className="button button-primary" disabled={submitReview.isPending || submitArbitration.isPending || (arbitrationMode && !arbitrationReason.trim())}
          onClick={() => { setFormError(''); submit(); }}>{submitReview.isPending || submitArbitration.isPending ? '正在提交…' : arbitrationMode ? '提交仲裁' : '提交独立评分'}</button>
      </div>}
    </>}
  </main>;
}
