import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { components } from '../api/schema';
import { portalApi } from '../app/api';
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
  const originalCreateObjectURL = URL.createObjectURL;
  const originalRevokeObjectURL = URL.revokeObjectURL;
  afterEach(() => {
    cleanup();
    if (originalCreateObjectURL) Object.defineProperty(URL, 'createObjectURL', { configurable: true, value: originalCreateObjectURL });
    else Reflect.deleteProperty(URL, 'createObjectURL');
    if (originalRevokeObjectURL) Object.defineProperty(URL, 'revokeObjectURL', { configurable: true, value: originalRevokeObjectURL });
    else Reflect.deleteProperty(URL, 'revokeObjectURL');
    vi.restoreAllMocks();
  });

  it('renders an isolated MCP App and binds its single approval tool to the exact request', async () => {
    const onDecision = vi.fn().mockResolvedValue(undefined);
    render(<TaskApprovalCard taskID="q1234567" approval={approval} reviewInbox onDecision={onDecision} />);

    const iframe = screen.getByTitle('MCP App 操作审批');
    expect(iframe).toHaveAttribute('sandbox', 'allow-scripts');
    expect(iframe).toHaveAttribute('referrerpolicy', 'no-referrer');
    expect(iframe.getAttribute('srcdoc')).toContain("default-src 'none'");
    expect(iframe.getAttribute('srcdoc')).toContain("connect-src 'none'");
    expect(iframe.getAttribute('srcdoc')).toContain('data-layout="review"');
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
    await app.send({ jsonrpc: '2.0', method: 'ui/notifications/size-changed', params: { height: 150 } });
    expect(iframe).toHaveStyle({ height: '150px' });
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
    render(<TaskApprovalCard taskID="q1234567" approval={expired} onDecision={vi.fn()} />);

    const card = screen.getByRole('region', { name: '审批已到期' });
    expect(card).toHaveTextContent('审批已到期；该操作已被拒绝并未执行。');
    expect(card.querySelector('iframe')).toBeNull();
    expect(screen.queryByRole('button', { name: '允许本次操作' })).not.toBeInTheDocument();
  });

  it.each([
    ['image/png', 'img', 'load'],
    ['audio/ogg', 'audio', 'canplay'],
    ['video/mp4', 'video', 'canplay'],
  ])('shows a %s preview before exposing approval and releases its object URL', async (mediaType, selector, readyEvent) => {
    const createObjectURL = vi.fn(() => 'blob:morde-task-preview');
    const revokeObjectURL = vi.fn();
    Object.defineProperty(URL, 'createObjectURL', { configurable: true, value: createObjectURL });
    Object.defineProperty(URL, 'revokeObjectURL', { configurable: true, value: revokeObjectURL });
    const previewRequest = vi.spyOn(portalApi, 'taskMediaPreview').mockResolvedValue(new Blob(['preview'], { type: mediaType }));
    const onDecision = vi.fn().mockResolvedValue(undefined);
    const { container, unmount } = render(<TaskApprovalCard taskID="q1234567"
      approval={{ ...approval, media_preview_operation_id: 'o1234567' }} reviewInbox onDecision={onDecision} />);

    expect(await screen.findByRole('status')).toHaveTextContent('正在加载媒体预览');
    expect(previewRequest).toHaveBeenCalledExactlyOnceWith('q1234567', 'o1234567');
    expect(screen.queryByTitle('MCP App 操作审批')).not.toBeInTheDocument();
    const media = await waitFor(() => {
      const element = container.querySelector(selector);
      expect(element).not.toBeNull();
      return element as HTMLMediaElement | HTMLImageElement;
    });
    expect(media.getAttribute('src')).toBe('blob:morde-task-preview');
    expect(screen.queryByTitle('MCP App 操作审批')).not.toBeInTheDocument();
    fireEvent(media, new Event(readyEvent));
    const iframe = await screen.findByTitle('MCP App 操作审批');
    const app = taskApprovalMcpAppDriver(iframe as HTMLIFrameElement);
    await app.initialize();
    await app.call('approve-after-preview', { approval_id: approval.id, version: approval.version, decision: 'approve' });
    await waitFor(() => expect(onDecision).toHaveBeenCalledExactlyOnceWith('approve'));
    unmount();
    expect(revokeObjectURL).toHaveBeenCalledExactlyOnceWith('blob:morde-task-preview');
  });

  it('keeps rejection and close available when a media preview fails, but never allows approval', async () => {
    vi.spyOn(portalApi, 'taskMediaPreview').mockRejectedValue(new Error('preview unavailable'));
    const onDecision = vi.fn();
    render(<TaskApprovalCard taskID="q1234567" approval={{ ...approval, media_preview_operation_id: 'o1234567' }}
      reviewInbox onDecision={onDecision} />);

    expect(await screen.findByRole('alert')).toHaveTextContent('媒体预览暂不可用');
    expect(screen.queryByTitle('MCP App 操作审批')).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /允许/ })).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: `拒绝：${approval.title}` }));
    expect(onDecision).toHaveBeenCalledExactlyOnceWith('reject');

    fireEvent.click(screen.getByRole('button', { name: new RegExp(`查看操作详情：${approval.title}`) }));
    fireEvent.click(screen.getByRole('button', { name: '关闭' }));
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
  });

  it('does not render an unsupported MIME or a provider URL as a preview', async () => {
    const createObjectURL = vi.fn(() => 'blob:morde-unsafe-preview');
    Object.defineProperty(URL, 'createObjectURL', { configurable: true, value: createObjectURL });
    vi.spyOn(portalApi, 'taskMediaPreview').mockResolvedValue(new Blob([
      '<html><img src="https://provider.invalid/private-media"></html>',
    ], { type: 'image/svg+xml' }));
    const onDecision = vi.fn();
    const { container } = render(<TaskApprovalCard taskID="q1234567" approval={{ ...approval, media_preview_operation_id: 'o1234567' }}
      reviewInbox onDecision={onDecision} />);

    expect(await screen.findByRole('alert')).toHaveTextContent('媒体预览暂不可用');
    expect(container.querySelector('img, audio, video, iframe')).toBeNull();
    expect(createObjectURL).not.toHaveBeenCalled();
    expect(screen.queryByRole('button', { name: /允许/ })).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: `拒绝：${approval.title}` })).toBeInTheDocument();
  });

  it('revokes and replaces the preview when its Task and operation scope changes', async () => {
    const createObjectURL = vi.fn().mockReturnValueOnce('blob:old-task-preview').mockReturnValueOnce('blob:new-task-preview');
    const revokeObjectURL = vi.fn();
    Object.defineProperty(URL, 'createObjectURL', { configurable: true, value: createObjectURL });
    Object.defineProperty(URL, 'revokeObjectURL', { configurable: true, value: revokeObjectURL });
    const previewRequest = vi.spyOn(portalApi, 'taskMediaPreview').mockImplementation(async (_taskID, operationID) =>
      new Blob([operationID], { type: 'image/png' }));
    const { container, rerender } = render(<TaskApprovalCard taskID="q1234567"
      approval={{ ...approval, media_preview_operation_id: 'o1234567' }} reviewInbox onDecision={vi.fn()} />);

    const oldImage = await waitFor(() => {
      const element = container.querySelector('img');
      expect(element?.getAttribute('src')).toBe('blob:old-task-preview');
      return element as HTMLImageElement;
    });
    fireEvent(oldImage, new Event('load'));
    expect(await screen.findByTitle('MCP App 操作审批')).toBeInTheDocument();

    rerender(<TaskApprovalCard taskID="q2234567"
      approval={{ ...approval, version: approval.version + 1, media_preview_operation_id: 'o2234567' }}
      reviewInbox onDecision={vi.fn()} />);

    const newImage = await waitFor(() => {
      const element = container.querySelector('img');
      expect(element?.getAttribute('src')).toBe('blob:new-task-preview');
      return element as HTMLImageElement;
    });
    expect(previewRequest).toHaveBeenNthCalledWith(1, 'q1234567', 'o1234567');
    expect(previewRequest).toHaveBeenNthCalledWith(2, 'q2234567', 'o2234567');
    expect(revokeObjectURL).toHaveBeenCalledWith('blob:old-task-preview');
    expect(newImage.getAttribute('src')).not.toBe('blob:old-task-preview');
    expect(screen.queryByTitle('MCP App 操作审批')).not.toBeInTheDocument();
  });

  it('recreates the embedded view when the server changes the approval version', async () => {
    const onDecision = vi.fn().mockResolvedValue(undefined);
    const { rerender } = render(<TaskApprovalCard taskID="q1234567" approval={approval} onDecision={onDecision} />);
    const firstFrame = screen.getByTitle('MCP App 操作审批') as HTMLIFrameElement;
    const firstApp = taskApprovalMcpAppDriver(firstFrame);
    await firstApp.initialize();

    const updatedApproval = { ...approval, version: approval.version + 1 };
    rerender(<TaskApprovalCard taskID="q1234567" approval={updatedApproval} onDecision={onDecision} />);

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
    const { rerender } = render(<TaskApprovalCard taskID="q1234567" approval={approval} onDecision={onDecision} />);
    const firstFrame = screen.getByTitle('MCP App 操作审批');
    await taskApprovalMcpAppDriver(firstFrame as HTMLIFrameElement).initialize();

    rerender(<TaskApprovalCard taskID="q1234567" approval={{ ...approval, status: 'approved', decision: 'approve', responded_at: '2026-11-25T00:00:00Z' }}
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
    render(<TaskApprovalCard taskID="q1234567" approval={invalidBasis} onDecision={vi.fn()} />);

    expect(screen.getByText('费用数据不完整')).toBeInTheDocument();
    expect(screen.queryByText('已结算 USD 1')).not.toBeInTheDocument();
  });
});
