import { cleanup, render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it } from 'vitest';
import { WorkspaceTransferPreview } from './WorkspaceTransferPreview';
import type { WorkspaceTransferPreviewSource } from './WorkspaceTransferPreview';

const source: WorkspaceTransferPreviewSource = {
  id: 'w1234567', name: '产品工作区', revision: 7, usedBytes: 4 * 1024 * 1024,
  runtimeState: 'stopped', checkpointReady: true, snapshotType: 'workspace',
};

function renderPreview(overrides: Partial<WorkspaceTransferPreviewSource> = {}) {
  return render(<WorkspaceTransferPreview source={{ ...source, ...overrides }} ownerLabel="原所有者" />);
}

function expectPartyFreezeCount(status: string, count: number) {
  const rows = screen.getByLabelText('移交双方冻结状态').querySelectorAll('small');
  expect(Array.from(rows).filter((row) => row.textContent?.includes(status))).toHaveLength(count);
}

describe('WorkspaceTransferPreview', () => {
  afterEach(cleanup);

  it('labels the copy as local, warns about hidden files and saved credentials, and requires acknowledgement', () => {
    renderPreview();

    expect(screen.getByText(/不连接真实服务、不创建服务端工作区、不上传文件/)).toBeInTheDocument();
    expect(screen.getByText(/隐藏文件.*用户保存的凭据/)).toBeInTheDocument();
    expect(screen.getByText('目标 Workspace 配额（MiB）')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: '模拟快照并创建副本' })).toBeDisabled();
  });

  it('does not permit snapshots while the source runtime is running', () => {
    renderPreview({ runtimeState: 'running' });

    expect(screen.getByText('运行状态：运行中，禁止快照。')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: '模拟快照并创建副本' })).toBeDisabled();
  });

  it('blocks unsupported content and checkpoints that are not ready', () => {
    const { rerender } = renderPreview({ snapshotType: 'host-runtime', checkpointReady: false });

    expect(screen.getByText('内容类型：Host Runtime 数据不支持复制。')).toBeInTheDocument();
    expect(screen.getByText('检查点：未就绪，暂不能快照。')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: '模拟快照并创建副本' })).toBeDisabled();

    rerender(<WorkspaceTransferPreview source={{ ...source, checkpointReady: false }} ownerLabel="原所有者" />);
    expect(screen.getByText('检查点：未就绪，暂不能快照。')).toBeInTheDocument();
  });

  it('blocks a copy when the new independent workspace quota is too small', async () => {
    const user = userEvent.setup();
    renderPreview();
    const quota = screen.getByLabelText('目标 Workspace 配额（MiB）');
    await user.clear(quota);
    await user.type(quota, '3');

    expect(screen.getByText(/目标配额：不足；复制需要/)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: '模拟快照并创建副本' })).toBeDisabled();
  });

  it('creates only an in-memory preview of an independent workspace after all checks pass', async () => {
    const user = userEvent.setup();
    renderPreview();

    await user.type(screen.getByLabelText('新建独立 Workspace 的名称'), '备份副本');
    await user.click(screen.getByRole('checkbox', { name: /我已检查快照可能包含/ }));
    await user.click(screen.getByRole('button', { name: '模拟快照并创建副本' }));

    expect(screen.getByText(/已在页面内存中模拟创建独立 Workspace/)).toHaveTextContent('产品工作区 的副本备份副本');
    expect(screen.getByText(/来源 v7 保持不变/)).toBeInTheDocument();
    expect(screen.getByText(/这不是服务端副本/)).toBeInTheDocument();
  });

  it('freezes both parties and lets the first accept or cancel action win', async () => {
    const user = userEvent.setup();
    renderPreview();
    await user.click(screen.getByRole('button', { name: '开始移交演示并冻结双方' }));

    expectPartyFreezeCount('已冻结（本地模拟）', 2);
    await user.click(screen.getByRole('button', { name: '模拟接收方接受' }));
    expect(screen.getByText(/接受先赢得竞争/)).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: '模拟所有者取消' })).not.toBeInTheDocument();
    expectPartyFreezeCount('已冻结（本地模拟）', 2);

    await user.click(screen.getByRole('button', { name: '模拟服务端确认移交' }));
    expect(screen.getByText(/服务端已确认移交给 演示接收方/)).toBeInTheDocument();
    expectPartyFreezeCount('未冻结', 2);
    expect(screen.getByText('当前所有者：演示接收方 · 未冻结')).toBeInTheDocument();
  });

  it('keeps both parties frozen after acceptance fails until the server confirms a safe unlock', async () => {
    const user = userEvent.setup();
    renderPreview();
    await user.click(screen.getByRole('button', { name: '开始移交演示并冻结双方' }));
    await user.click(screen.getByRole('button', { name: '模拟接收方接受' }));
    await user.click(screen.getByRole('button', { name: '模拟接受失败' }));

    expect(screen.getByText(/模拟接受失败。双方仍冻结/)).toBeInTheDocument();
    expectPartyFreezeCount('已冻结（本地模拟）', 2);
    await user.click(screen.getByRole('button', { name: '模拟服务端确认未变更并安全解锁' }));
    expect(screen.getByText(/服务端已确认所有权未变化并安全解锁/)).toBeInTheDocument();
    expectPartyFreezeCount('未冻结', 2);
    expect(screen.getByText('当前所有者：原所有者 · 未冻结')).toBeInTheDocument();
  });

  it('keeps the cancellation winner frozen until server confirmation', async () => {
    const user = userEvent.setup();
    renderPreview();
    await user.click(screen.getByRole('button', { name: '开始移交演示并冻结双方' }));
    await user.click(screen.getByRole('button', { name: '模拟所有者取消' }));

    expect(screen.getByText(/取消先赢得竞争/)).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: '模拟接收方接受' })).not.toBeInTheDocument();
    expectPartyFreezeCount('已冻结（本地模拟）', 2);
    await user.click(screen.getByRole('button', { name: '模拟服务端确认取消并解锁' }));
    expect(screen.getByText(/服务端已确认取消/)).toBeInTheDocument();
    expectPartyFreezeCount('未冻结', 2);
  });
});
