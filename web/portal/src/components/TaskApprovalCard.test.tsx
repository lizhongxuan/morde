import { cleanup, fireEvent, render, screen, within } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { components } from '../api/schema';
import { TaskApprovalCard } from './TaskApprovalCard';

const approval: components['schemas']['TaskPendingRequest'] = {
  id: 'k1234567', version: 2, status: 'pending', kind: 'approval', title: '删除项目文件',
  object: 'project/demo/draft.txt', content: '删除该文件。', impact: '项目中的草稿文件将不可用。',
  disclosures: {
    risk: { state: 'unknown', summary: null },
    this_cost: { state: 'unknown', basis: null, amount_micros: null, currency: null },
    cumulative_cost: { state: 'unknown', basis: null, amount_micros: null, currency: null },
    alternatives: { state: 'unknown', options: [] },
    revocation: { state: 'unknown', summary: null },
  },
  expires_at: null,
};

describe('TaskApprovalCard', () => {
  afterEach(() => cleanup());

  it('states the one-action scope and honest outcome before allowing a decision', () => {
    const onDecision = vi.fn();
    render(<TaskApprovalCard approval={approval} reviewInbox onDecision={onDecision} />);

    expect(screen.getAllByText(/审批范围仅限本次显示的操作/u)).toHaveLength(2);
    expect(screen.getAllByText(/拒绝后本次操作不会执行/u)).toHaveLength(2);
    expect(onDecision).not.toHaveBeenCalled();

    fireEvent.click(screen.getByRole('button', { name: '查看操作详情：删除项目文件' }));
    const dialog = screen.getByRole('dialog');
    expect(dialog).toHaveTextContent('项目中的草稿文件将不可用。');
    expect(dialog).toHaveTextContent(/允许后任务会继续尝试执行，最终结果以任务状态为准/u);

    fireEvent.click(within(dialog).getByRole('button', { name: '允许：删除项目文件' }));
    expect(onDecision).toHaveBeenCalledExactlyOnceWith('approve');
  });

  it('submits an explicit rejection from the operation details dialog', () => {
    const onDecision = vi.fn();
    render(<TaskApprovalCard approval={approval} reviewInbox onDecision={onDecision} />);

    fireEvent.click(screen.getByRole('button', { name: '查看操作详情：删除项目文件' }));
    const dialog = screen.getByRole('dialog');
    fireEvent.click(within(dialog).getByRole('button', { name: '拒绝：删除项目文件' }));

    expect(onDecision).toHaveBeenCalledExactlyOnceWith('reject');
  });

  it('keeps unknown costs visibly unknown instead of rendering them as zero', () => {
    render(<TaskApprovalCard approval={approval} onDecision={vi.fn()} />);

    const disclosures = screen.getByLabelText('风险、费用和其他审批信息');
    expect(disclosures).toHaveTextContent(/本次费用未知/u);
    expect(disclosures).toHaveTextContent(/累计费用未知/u);
    expect(disclosures).not.toHaveTextContent('0 CNY');
  });
});
