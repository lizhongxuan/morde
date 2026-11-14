import { cleanup, render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, expect, it, vi } from 'vitest';
import { LocalWorkspaceRecovery } from './LocalWorkspaceRecovery';

afterEach(cleanup);

it('keeps legacy safety copies in the PC page and restores only after a user action', async () => {
  const id = 'w1234567';
  const copyId = 'a'.repeat(32);
  const bridge = {
    legacySyncState: vi.fn().mockResolvedValue({ mappings: [{ workspaceId: id, directoryName: 'demo', syncEnabled: false, liveRunning: false }],
      syncState: 'idle', nodeState: 'stopped', safetyCopiesAvailable: true, checkpoint: null }),
    preserved: vi.fn().mockResolvedValue([{ id: copyId, workspace_id: id, path: 'notes/a.txt', size: 12, created_at: '2026-11-14T00:00:00Z' }]),
    restorePreserved: vi.fn().mockResolvedValue({ restored: true }),
  };
  const user = userEvent.setup();
  render(<LocalWorkspaceRecovery bridge={bridge} workspaceNames={{ [id]: '项目' }} />);
  await user.click(await screen.findByText('已有 Workspace 本机同步与安全副本'));
  expect(screen.getByText('项目')).toBeInTheDocument();
  expect(bridge.preserved).not.toHaveBeenCalled();
  await user.click(screen.getByRole('button', { name: '查看本机安全副本' }));
  expect(await screen.findByText(/notes\/a\.txt/)).toBeInTheDocument();
  expect(bridge.restorePreserved).not.toHaveBeenCalled();
  await user.click(screen.getByRole('button', { name: '恢复到本机' }));
  expect(bridge.restorePreserved).toHaveBeenCalledWith(id, copyId);
  expect(await screen.findByText('已恢复到本机，安全副本仍保留。')).toBeInTheDocument();
});

it('keeps safety-copy actions disabled while the old node may still own the directory', async () => {
  const bridge = { legacySyncState: vi.fn().mockResolvedValue({ mappings: [{ workspaceId: 'w1234567', directoryName: 'demo', syncEnabled: true, liveRunning: false }],
    syncState: 'idle', nodeState: 'unknown', safetyCopiesAvailable: false, checkpoint: null }), preserved: vi.fn(), legacyPauseSync: vi.fn() };
  const user = userEvent.setup();
  render(<LocalWorkspaceRecovery bridge={bridge} workspaceNames={{}} />);
  await user.click(await screen.findByText('已有 Workspace 本机同步与安全副本'));
  expect(screen.getByRole('button', { name: '查看本机安全副本' })).toBeDisabled();
  expect(bridge.preserved).not.toHaveBeenCalled();
});

it('can stop an explicitly resumed legacy checkpoint without claiming it completed', async () => {
  const id = 'w1234567';
  let listener: ((value: unknown) => void) | undefined;
  let syncState = 'idle';
  let finish: ((value: unknown) => void) | undefined;
  const bridge = {
    legacySyncState: vi.fn().mockImplementation(async () => ({ mappings: [{ workspaceId: id, directoryName: 'demo', syncEnabled: false, liveRunning: false }],
      syncState, nodeState: 'stopped', safetyCopiesAvailable: false, checkpoint: { workspaceId: id, mode: 'upload', scope: ['notes'] } })),
    onSync: (callback: (value: unknown) => void) => { listener = callback; return () => { listener = undefined; }; },
    legacyResumeSync: vi.fn().mockImplementation(() => {
      syncState = 'running'; listener?.({});
      return new Promise((resolve) => { finish = resolve; });
    }),
    legacyStopSync: vi.fn().mockImplementation(async () => { syncState = 'idle'; return true; }),
  };
  const user = userEvent.setup();
  render(<LocalWorkspaceRecovery bridge={bridge} workspaceNames={{}} />);
  await user.click(await screen.findByText('已有 Workspace 本机同步与安全副本'));
  expect(screen.getByText('同步范围：notes')).toBeInTheDocument();
  await user.click(screen.getByRole('button', { name: '按原模式继续同步' }));
  const stop = await screen.findByRole('button', { name: '停止旧版同步' });
  expect(stop).toBeEnabled();
  await user.click(stop);
  expect(bridge.legacyStopSync).toHaveBeenCalledWith(id);
  finish?.({ session: { state: 'cancelled' }, conflicts: [] });
  expect(await screen.findByText('旧版同步结果尚未确认，恢复点已保留。')).toBeInTheDocument();
  expect(screen.queryByText('旧版同步已完成。')).not.toBeInTheDocument();
});
