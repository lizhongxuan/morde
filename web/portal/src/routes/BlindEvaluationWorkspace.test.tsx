import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter, Route, Routes } from 'react-router';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { components } from '../api/schema';
import { portalApi } from '../app/api';
import { BlindEvaluationWorkspace } from './BlindEvaluationWorkspace';

const candidateA = 'blind-aaaaaaaaaaaaaaaa';
const candidateB = 'blind-bbbbbbbbbbbbbbbb';
const pack: components['schemas']['BlindPack'] = {
  id: '1234567890abcdef1234567890abcdef', suite_version: 'suite-v1',
  rubric: [{ id: 'H01', name: '结论清楚', hard_gate: true }],
  cases: [{ case_id: 'AIC-H-01', priority: 'P0', tags: ['synthetic'], input: '回答问题', expected_behavior: '给出清楚答案',
    applicability: [{ criterion_id: 'H01', applicable: true }],
    candidates: [
      { label: candidateA, output: '结论：你好。', evidence: [] },
      { label: candidateB, output: '结论：您好。', evidence: [] },
    ],
  }],
};

function renderWorkspace(path: string) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={client}><MemoryRouter initialEntries={[path]}>
    <Routes><Route path="/evaluation-batches/:pack_id/:mode" element={<BlindEvaluationWorkspace />} /></Routes>
  </MemoryRouter></QueryClientProvider>);
}

afterEach(() => { cleanup(); vi.restoreAllMocks(); });

describe('BlindEvaluationWorkspace', () => {
  it('submits an exact UTF-8 byte span and restores its submitted state', async () => {
    let bundle: Awaited<ReturnType<typeof portalApi.blindReviewBundle>> = { pack, reviews: [] };
    vi.spyOn(portalApi, 'blindReviewBundle').mockImplementation(async () => bundle);
    const submit = vi.spyOn(portalApi, 'submitBlindReview').mockImplementation(async (_packID, body) => {
      bundle = { pack, reviews: [{ candidate: body.candidate, submitted_at: '2026-11-21T00:00:00Z', cases: body.cases }] };
      return { candidate: body.candidate, submitted_at: '2026-11-21T00:00:00Z' };
    });

    renderWorkspace(`/evaluation-batches/${pack.id}/review`);
    fireEvent.change(await screen.findByLabelText('评分'), { target: { value: '0' } });
    fireEvent.change(screen.getByLabelText('评分理由'), { target: { value: '回答内容没有满足要求。' } });
    fireEvent.change(screen.getByLabelText('引用回答中的原文'), { target: { value: '你好' } });
    fireEvent.click(screen.getByRole('button', { name: '提交独立评分' }));

    await waitFor(() => expect(submit).toHaveBeenCalledTimes(1));
    expect(submit.mock.calls[0]?.[1].cases[0]?.judgments[0]?.evidence).toEqual([
      { kind: 'output_span', start_byte: 9, end_byte: 15 },
    ]);
    expect(await screen.findByText(/已提交该候选的评分/)).toBeInTheDocument();
  });

  it('anchors a not-applicable score to the frozen suite reason', async () => {
    const naPack: components['schemas']['BlindPack'] = {
      ...pack,
      rubric: [{ id: 'H02', name: '来源证据', hard_gate: false }],
      cases: pack.cases.map((testCase) => ({ ...testCase, applicability: [
        { criterion_id: 'H02', applicable: false, not_applicable_reason: '本用例不涉及外部来源。' },
      ] })),
    };
    vi.spyOn(portalApi, 'blindReviewBundle').mockResolvedValue({ pack: naPack, reviews: [] });
    const submit = vi.spyOn(portalApi, 'submitBlindReview').mockResolvedValue({ candidate: candidateA, submitted_at: '2026-11-21T00:00:00Z' });

    renderWorkspace(`/evaluation-batches/${pack.id}/review`);
    fireEvent.change(await screen.findByLabelText('评分理由'), { target: { value: '该用例范围不包含来源核验。' } });
    fireEvent.click(screen.getByRole('button', { name: '提交独立评分' }));

    await waitFor(() => expect(submit).toHaveBeenCalledTimes(1));
    expect(submit.mock.calls[0]?.[1].cases[0]?.judgments[0]).toMatchObject({
      criterion_id: 'H02', score: 'N/A',
      evidence: [{ kind: 'suite_na_reason', id: 'AIC-H-01/H02', start_byte: 0, end_byte: 0 }],
    });
  });

  it('shows peer identities as anonymous aliases and restores a completed arbitration', async () => {
    type Bundle = Awaited<ReturnType<typeof portalApi.blindArbitrationBundle>>;
    let bundle: Bundle = {
      pack, expected_reviews_per_candidate: 2,
      reviews: [
        { reviewer: '审阅者1', candidate: candidateA, cases: [{ case_id: 'AIC-H-01', judgments: [{ criterion_id: 'H01', score: '0', rationale: '没有达标', evidence: [{ kind: 'output_span', start_byte: 0, end_byte: 15 }] }] }] },
        { reviewer: '审阅者2', candidate: candidateA, cases: [{ case_id: 'AIC-H-01', judgments: [{ criterion_id: 'H01', score: '1', rationale: '已给出答案', evidence: [{ kind: 'output_span', start_byte: 0, end_byte: 15 }] }] }] },
      ],
      arbitrations: [],
    };
    vi.spyOn(portalApi, 'blindArbitrationBundle').mockImplementation(async () => bundle);
    const submit = vi.spyOn(portalApi, 'submitBlindArbitration').mockImplementation(async (_packID, body) => {
      bundle = { ...bundle, arbitrations: [{ candidate: body.candidate, reason: body.reason, cases: body.cases }] };
      return { candidate: body.candidate, resolved_at: '2026-11-21T00:00:00Z' };
    });

    renderWorkspace(`/evaluation-batches/${pack.id}/arbitration`);
    expect(await screen.findByText('审阅者1')).toBeInTheDocument();
    expect(screen.getByText('审阅者2')).toBeInTheDocument();
    fireEvent.change(screen.getByLabelText('评分'), { target: { value: '1' } });
    fireEvent.change(screen.getByLabelText('评分理由'), { target: { value: '引用的证据支持通过。' } });
    fireEvent.change(screen.getByLabelText('引用回答中的原文'), { target: { value: '你好' } });
    fireEvent.change(screen.getByLabelText('仲裁结论与依据'), { target: { value: '结合预期要求和回答片段，判定通过。' } });
    fireEvent.click(screen.getByRole('button', { name: '提交仲裁' }));

    await waitFor(() => expect(submit).toHaveBeenCalledTimes(1));
    expect(submit.mock.calls[0]?.[1].reason).toBe('结合预期要求和回答片段，判定通过。');
    expect(await screen.findByText(/该候选已完成仲裁/)).toBeInTheDocument();
    expect(screen.getByText('仲裁评分').parentElement).toHaveTextContent('1 · 引用的证据支持通过。');
    expect(screen.queryByText(/arbitrator_id|审阅者 u/)).not.toBeInTheDocument();
  });
});
