import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { createElement } from 'react';
import { cleanup, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { TaskApprovalMcpApp } from './TaskApprovalMcpApp';
import { taskApprovalMcpAppDriver } from './TaskApprovalMcpApp.test-utils';

const appScript = readFileSync(resolve(process.cwd(), 'public/mcp-app/task-approval.js'), 'utf8');

describe('task approval MCP App resource', () => {
  afterEach(() => {
    cleanup();
    document.getElementById('app')?.remove();
    delete document.body.dataset.nonce;
    delete document.body.dataset.layout;
    vi.restoreAllMocks();
  });

  it.each(['conversation', 'review'] as const)('renders the %s MCP App safely and calls only the approval tool', async (layout) => {
    document.body.dataset.nonce = 'fedcba0987654321';
    document.body.dataset.layout = layout;
    const app = document.createElement('main');
    app.id = 'app';
    document.body.append(app);
    const outgoing: Record<string, unknown>[] = [];
    vi.spyOn(window, 'postMessage').mockImplementation((message) => {
      outgoing.push(message as Record<string, unknown>);
    });
    window.eval(appScript);

    const send = (data: Record<string, unknown>) => window.dispatchEvent(new MessageEvent('message', {
      data,
      origin: window.location.origin,
      source: window,
    }));
    const initialize = outgoing[0];
    expect(initialize).toMatchObject({
      jsonrpc: '2.0', method: 'ui/initialize',
      params: { protocolVersion: '2026-01-26', appInfo: { name: 'morde-task-approval' }, appCapabilities: {} },
    });
    const initializeID = initialize?.id;
    send({ jsonrpc: '2.0', id: initializeID, result: { hostCapabilities: { serverTools: {} } } });
    const listRequest = outgoing.find((message) => message.method === 'tools/list');
    expect(listRequest).toBeDefined();
    send({ jsonrpc: '2.0', id: listRequest?.id, result: { tools: [{ name: 'respond_to_task_approval' }] } });
    send({ jsonrpc: '2.0', method: 'ui/notifications/tool-input', params: { arguments: {
      id: 'approval-1', version: 7, title: '<img src=x onerror=alert(1)>', object: 'workspace/report.md',
      content: '写入报告', impact: '新增文件', risk: '未知', thisCost: '未知', cumulativeCost: '未知',
      alternatives: '稍后处理', revocation: '执行前可停止', expiresAt: '2027-11-26T09:06:00Z',
    } } });

    const root = document.getElementById('app');
    expect(root?.textContent).toContain('<img src=x onerror=alert(1)>');
    expect(root?.querySelector('img')).toBeNull();
    if (layout === 'review') {
      expect(root?.querySelector('.review-card')).toHaveTextContent('写入报告');
      expect(root?.querySelector('.review-card')).toHaveTextContent('目标：workspace/report.md · 影响：新增文件');
      expect(root?.querySelector('.review-meta')).toHaveTextContent('风险未知');
      expect(root?.querySelector('.review-meta')).toHaveTextContent('费用未知');
      expect(root?.querySelector('.review-meta')).toHaveTextContent('仅本次操作');
      expect(root?.querySelector('time')).toHaveTextContent('11月26日 17:06');
      expect(root?.querySelector('details')).toHaveTextContent('稍后处理');
      expect(root?.querySelector('details')).not.toHaveAttribute('open');
      expect(root?.textContent).not.toContain('审批仅适用于本次显示的操作');
    } else {
      expect(root?.querySelector('.review-card')).toBeNull();
    }
    const approveLabel = layout === 'review' ? '允许本次' : '允许本次操作';
    const approve = Array.from(root?.querySelectorAll('button') ?? []).find((button) => button.textContent === approveLabel);
    expect(approve).toBeDefined();
    await waitFor(() => expect(Array.from(root?.querySelectorAll('button') ?? [])
      .find((button) => button.textContent === approveLabel)).toBeEnabled());
    Array.from(root?.querySelectorAll('button') ?? []).find((button) => button.textContent === approveLabel)?.click();
    const call = outgoing.find((message) => message.method === 'tools/call');
    expect(call).toMatchObject({
      params: { name: 'respond_to_task_approval', arguments: { approval_id: 'approval-1', version: 7, decision: 'approve' } },
    });
    send({ jsonrpc: '2.0', id: call?.id, result: { isError: true } });
    await waitFor(() => expect(root?.textContent).toContain('审批结果尚未确认'));
    const buttonsAfterFailure = Array.from(root?.querySelectorAll('button') ?? []);
    expect(buttonsAfterFailure.find((button) => button.textContent === '拒绝')).toBeDisabled();
    const retry = buttonsAfterFailure.find((button) => button.textContent === approveLabel);
    expect(retry).toBeEnabled();
    retry?.click();
    const retryCall = outgoing.filter((message) => message.method === 'tools/call').at(-1);
    expect(retryCall?.id).not.toBe(call?.id);
    expect(retryCall?.params).toEqual(call?.params);
    send({ jsonrpc: '2.0', id: retryCall?.id, result: { content: [{ type: 'text', text: '决定已提交' }], isError: false } });
    await new Promise((resolvePromise) => setTimeout(resolvePromise, 0));
    expect(root?.textContent).toContain('已提交允许决定；任务后续状态仍需确认。');
    expect(Array.from(root?.querySelectorAll('button') ?? []).every((button) => button.disabled)).toBe(true);
  });

  it('reports a synchronous approval adapter failure and leaves the decision retryable', async () => {
    const onDecision = vi.fn()
      .mockImplementationOnce(() => { throw new Error('synchronous failure'); })
      .mockResolvedValue(undefined);
    render(createElement(TaskApprovalMcpApp, { approval: {
      id: 'approval-1', version: 7, title: '删除文件', object: 'workspace/report.md',
      content: '删除草稿', impact: '文件不可恢复', risk: '高', thisCost: '不适用',
      cumulativeCost: '未知', alternatives: '保留', revocation: '无法撤销', expiresAt: null,
    }, onDecision }));
    const frame = screen.getByTitle('MCP App 操作审批') as HTMLIFrameElement;
    const app = taskApprovalMcpAppDriver(frame);
    await app.initialize();

    const arguments_ = { approval_id: 'approval-1', version: 7, decision: 'reject' };
    await app.call('sync-failure', arguments_);
    await waitFor(() => expect(onDecision).toHaveBeenCalledExactlyOnceWith('reject'));
    await waitFor(() => expect(app.sent).toContainEqual(expect.objectContaining({
      jsonrpc: '2.0', id: 'sync-failure', result: expect.objectContaining({ isError: true }),
    })));

    await app.call('opposite-decision', { ...arguments_, decision: 'approve' });
    expect(onDecision).toHaveBeenCalledTimes(1);
    expect(app.sent).toContainEqual(expect.objectContaining({
      jsonrpc: '2.0', id: 'opposite-decision', error: expect.any(Object),
    }));

    await app.call('retry', arguments_);
    await waitFor(() => expect(onDecision).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(app.sent).toContainEqual(expect.objectContaining({
      jsonrpc: '2.0', id: 'retry', result: expect.objectContaining({ isError: false }),
    })));
    await app.call('after-confirmation', arguments_);
    expect(onDecision).toHaveBeenCalledTimes(2);
    expect(app.sent).toContainEqual(expect.objectContaining({
      jsonrpc: '2.0', id: 'after-confirmation', error: expect.any(Object),
    }));
  });
});
