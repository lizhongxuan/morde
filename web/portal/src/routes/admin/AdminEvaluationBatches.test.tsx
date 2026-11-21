import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { MemoryRouter } from 'react-router';
import { afterEach, expect, it, vi } from 'vitest';
import frozenSuiteJSON from '../../../../../server/internal/evaluation/testdata/suite-v1.json';
import type { components } from '../../api/schema';
import { portalApi } from '../../app/api';
import { AdminEvaluationBatches } from './AdminEvaluationBatches';

const pack: components['schemas']['BlindPack'] = {
  id: '1234567890abcdef1234567890abcdef', suite_version: 'suite-v1', rubric: [], cases: [],
};
const users = ['u1', 'u2', 'u3', 'u4'].map((id) => ({ id, name: id, status: 'approved' as const })) as Awaited<ReturnType<typeof portalApi.adminUsers>>['items'];

function renderPage() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={client}><MemoryRouter><AdminEvaluationBatches /></MemoryRouter></QueryClientProvider>);
}

afterEach(() => { cleanup(); vi.restoreAllMocks(); });

it('shows account loading state before the first page arrives', async () => {
  type UserPage = Awaited<ReturnType<typeof portalApi.adminUsers>>;
  let resolveUsers: (page: UserPage) => void = () => {};
  const pendingUsers = new Promise<UserPage>((resolve) => { resolveUsers = resolve; });
  vi.spyOn(portalApi, 'adminUsers').mockReturnValue(pendingUsers);

  renderPage();
  expect(await screen.findAllByText('正在加载已审核账号…')).toHaveLength(3);
  resolveUsers({ items: [], total: 0, limit: 50, offset: 0 });
  await waitFor(() => expect(screen.getAllByText('暂无已通过审核的账号。')).toHaveLength(3));
});

it('creates a synthetic blind batch with disjoint participant roles', async () => {
  vi.spyOn(portalApi, 'adminUsers').mockResolvedValue({ items: users, total: users.length, limit: 50, offset: 0 });
  const create = vi.spyOn(portalApi, 'createBlindEvaluationBatch').mockResolvedValue(pack);
  vi.spyOn(portalApi, 'blindEvaluationPanel').mockResolvedValue({ pack_id: pack.id, ready: false, pending_reason: '等待评审', review_count: 0, arbitration_count: 0 });
  vi.spyOn(portalApi, 'blindEvaluationMapping').mockResolvedValue({ pack_id: pack.id, assignments: [] });
  vi.spyOn(portalApi, 'blindEvaluationManifests').mockResolvedValue({ items: [] });

  renderPage();
  await screen.findAllByRole('checkbox');
  const implementers = await screen.findByRole('group', { name: /实现者/ });
  const reviewers = screen.getByRole('group', { name: /独立审阅者/ });
  const arbitrators = screen.getByRole('group', { name: /仲裁者/ });
  fireEvent.click(within(implementers).getByRole('checkbox', { name: /u1/ }));
  fireEvent.click(within(reviewers).getByRole('checkbox', { name: /u1/ }));
  expect(screen.getByRole('button', { name: '创建匿名批次' })).toBeDisabled();
  fireEvent.click(within(reviewers).getByRole('checkbox', { name: /u1/ }));
  fireEvent.click(within(reviewers).getByRole('checkbox', { name: /u2/ }));
  fireEvent.click(within(reviewers).getByRole('checkbox', { name: /u3/ }));
  fireEvent.click(within(arbitrators).getByRole('checkbox', { name: /u4/ }));
  fireEvent.change(screen.getByLabelText('候选运行结果 JSON'), { target: { value: '[{},{}]' } });
  fireEvent.click(screen.getByRole('button', { name: '创建匿名批次' }));

  await waitFor(() => expect(create).toHaveBeenCalledTimes(1));
  expect(create.mock.calls[0]?.[0]).toMatchObject({
    suite: frozenSuiteJSON, runs: [{}, {}],
    implementers: ['u1'], reviewers: ['u2', 'u3'], arbitrators: ['u4'],
  });
  expect(await screen.findByText(new RegExp(pack.id))).toBeInTheDocument();
});
