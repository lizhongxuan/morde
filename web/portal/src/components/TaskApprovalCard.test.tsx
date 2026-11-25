import { cleanup, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { components } from '../api/schema';
import { TaskApprovalCard } from './TaskApprovalCard';
import { taskApprovalMcpAppDriver } from './TaskApprovalMcpApp.test-utils';

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

  it('renders an isolated MCP App and binds its single approval tool to the exact request', async () => {
    const onDecision = vi.fn().mockResolvedValue(undefined);
    render(<TaskApprovalCard approval={approval} reviewInbox onDecision={onDecision} />);

    const iframe = screen.getByTitle('MCP App 操作审批');
    expect(iframe).toHaveAttribute('sandbox', 'allow-scripts');
    expect(iframe).toHaveAttribute('referrerpolicy', 'no-referrer');
    expect(iframe.getAttribute('srcdoc')).toContain("default-src 'none'");
    expect(iframe.getAttribute('srcdoc')).toContain("connect-src 'none'");
    expect(iframe.getAttribute('srcdoc')).not.toContain('allow-same-origin');
    const app = taskApprovalMcpAppDriver(iframe as HTMLIFrameElement);

    await app.send({ jsonrpc: '2.0', id: `initialize-${app.nonce}`, method: 'ui/initialize', params: {
      protocolVersion: '2026-01-26', appInfo: { name: 'forged', version: '1' }, appCapabilities: {},
    } }, 'null', window as unknown as MessageEventSource);
    expect(onDecision).not.toHaveBeenCalled();
    await app.send({ jsonrpc: '2.0', id: `initialize-${app.nonce}`, method: 'ui/initialize', params: {
      protocolVersion: '2026-01-26', appInfo: { name: 'forged', version: '1' }, appCapabilities: {},
    } }, 'https://attacker.invalid');
    expect(onDecision).not.toHaveBeenCalled();

    await app.initialize();
    await app.send({ jsonrpc: '2.0', method: 'ui/notifications/size-changed', params: { height: 430 } });
    expect(iframe).toHaveStyle({ height: '430px' });
    await app.send({ jsonrpc: '2.0', method: 'ui/notifications/size-changed', params: { height: 10_000 } });
    expect(iframe).toHaveStyle({ height: '640px' });
    await app.send({ jsonrpc: '2.0', method: 'ui/notifications/size-changed', params: { height: 400 } }, 'https://attacker.invalid');
    expect(iframe).toHaveStyle({ height: '640px' });
    await app.call('stale', { approval_id: approval.id, version: approval.version + 1, decision: 'approve' });
    expect(onDecision).not.toHaveBeenCalled();
    await app.call('bad-tool', { approval_id: approval.id, version: approval.version, decision: 'approve' }, 'task_action');
    expect(onDecision).not.toHaveBeenCalled();
    await app.call('valid', { approval_id: approval.id, version: approval.version, decision: 'approve' });
    await waitFor(() => expect(onDecision).toHaveBeenCalledExactlyOnceWith('approve'));
    expect(app.sent).toContainEqual(expect.objectContaining({ jsonrpc: '2.0', id: 'stale', error: expect.any(Object) }));
    expect(app.sent).toContainEqual(expect.objectContaining({ jsonrpc: '2.0', id: 'valid', result: expect.objectContaining({ isError: false }) }));
  });

  it('does not expose an approval decision when the request is already expired', () => {
    const expired = { ...approval, status: 'expired' as const, expires_at: '2026-10-21T08:00:00Z' };
    render(<TaskApprovalCard approval={expired} onDecision={vi.fn()} />);

    const card = screen.getByRole('region', { name: '审批已到期' });
    expect(card).toHaveTextContent('审批已到期；该操作已被拒绝并未执行。');
    expect(card.querySelector('iframe')).toBeNull();
    expect(screen.queryByRole('button', { name: '允许本次操作' })).not.toBeInTheDocument();
  });

  it.each([false, true])('explains an unavailable media preview without exposing a decision (review=%s)', (reviewInbox) => {
    render(<TaskApprovalCard approval={{ ...approval, media_preview_operation_id: 'o1234567' }}
      reviewInbox={reviewInbox} onDecision={vi.fn()} />);

    expect(screen.getByRole('status')).toHaveTextContent('媒体预览暂不可用，无法处理此审批。');
    expect(screen.queryByTitle('MCP App 操作审批')).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /允许|拒绝/ })).not.toBeInTheDocument();
  });

  it('recreates the embedded view when the server changes the approval version', async () => {
    const onDecision = vi.fn().mockResolvedValue(undefined);
    const { rerender } = render(<TaskApprovalCard approval={approval} onDecision={onDecision} />);
    const firstFrame = screen.getByTitle('MCP App 操作审批') as HTMLIFrameElement;
    const firstApp = taskApprovalMcpAppDriver(firstFrame);
    await firstApp.initialize();

    const updatedApproval = { ...approval, version: approval.version + 1 };
    rerender(<TaskApprovalCard approval={updatedApproval} onDecision={onDecision} />);

    const updatedFrame = screen.getByTitle('MCP App 操作审批') as HTMLIFrameElement;
    expect(updatedFrame).not.toBe(firstFrame);
    const updatedApp = taskApprovalMcpAppDriver(updatedFrame);
    await updatedApp.initialize();
    expect(updatedApp.currentApproval()).toMatchObject({ id: approval.id, version: updatedApproval.version });
    await updatedApp.call('stale-version', {
      approval_id: approval.id, version: approval.version, decision: 'approve',
    });
    expect(onDecision).not.toHaveBeenCalled();
    await updatedApp.call('current-version', {
      approval_id: approval.id, version: updatedApproval.version, decision: 'reject',
    });
    await waitFor(() => expect(onDecision).toHaveBeenCalledExactlyOnceWith('reject'));
  });

  it('replaces the actionable embedded view with a read-only state after the server records a decision', async () => {
    const onDecision = vi.fn().mockResolvedValue(undefined);
    const { rerender } = render(<TaskApprovalCard approval={approval} onDecision={onDecision} />);
    const firstFrame = screen.getByTitle('MCP App 操作审批');
    await taskApprovalMcpAppDriver(firstFrame as HTMLIFrameElement).initialize();

    rerender(<TaskApprovalCard approval={{ ...approval, status: 'approved', decision: 'approve', responded_at: '2026-11-25T00:00:00Z' }}
      onDecision={onDecision} />);

    expect(screen.queryByTitle('MCP App 操作审批')).not.toBeInTheDocument();
    expect(screen.getByRole('region', { name: '已处理的审批' })).toHaveTextContent('审批已处理；任务后续状态等待更新。');
    expect(screen.queryByRole('button', { name: '允许本次操作' })).not.toBeInTheDocument();
    expect(onDecision).not.toHaveBeenCalled();
  });

  it('does not label an unknown cost basis as settled', () => {
    const invalidBasis = {
      ...approval,
      status: 'approved' as const,
      disclosures: {
        ...approval.disclosures,
        this_cost: { state: 'known', basis: 'unknown', amount_micros: 1_000_000, currency: 'USD' },
      },
    } as unknown as components['schemas']['TaskPendingRequest'];
    render(<TaskApprovalCard approval={invalidBasis} onDecision={vi.fn()} />);

    expect(screen.getByText('费用数据不完整')).toBeInTheDocument();
    expect(screen.queryByText('已结算 USD 1')).not.toBeInTheDocument();
  });
});
