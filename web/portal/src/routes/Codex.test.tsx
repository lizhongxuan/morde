import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { components } from '../api/schema';
import { portalApi } from '../app/api';
import { ToastNoticeCenter, ToastProvider } from '../app/toast';
import { Codex } from './Codex';

const state = {
  authenticated: true,
  codex: {
    selectedProfileKey: 'default', running: true, profiles: [{ profileKey: 'default', label: '日常配置', executableLabel: 'codex',
      state: 'running', connectionAuthorized: true, scopes: ['read', 'live', 'send'], syncState: 'synced', syncTotal: 1, syncDone: 1 }],
    sessions: [{ id: 'mirror-1', profileKey: 'default', threadId: 'thread-1', title: '本机对话', cwdLabel: '~/repo',
      updatedAt: '2026-11-14T10:00:00Z', status: 'completed', scopes: ['read', 'live', 'send'], historyComplete: true }],
  },
};

function renderPage() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(<QueryClientProvider client={client}><ToastProvider><ToastNoticeCenter /><MemoryRouter><Codex /></MemoryRouter></ToastProvider></QueryClientProvider>);
}

describe('Codex', () => {
  afterEach(() => { cleanup(); delete (window as Window & { morde?: unknown }).morde; vi.restoreAllMocks(); });

  it('lists local conversations and displays synced reasoning and tool events from the server', async () => {
    Object.defineProperty(window, 'morde', { configurable: true, value: {
      codexState: vi.fn().mockResolvedValue(state),
      codexListThreads: vi.fn().mockResolvedValue({ items: [{ id: 'thread-1', title: '本机对话', cwdLabel: '~/repo',
        updatedAt: '2026-11-14T10:00:00Z', status: 'completed', authorized: true, archived: false }], nextCursor: null }),
    } });
    vi.spyOn(portalApi, 'codexSessionEvents').mockResolvedValue({
      items: [
        { session_id: 'mirror-1', sequence: 1, provider_event_id: 'e1', kind: 'assistant_delta', turn_id: 'turn-1', item_id: 'item-1', occurred_at: '2026-11-14T10:00:00Z', payload: { text: '尚未完成的回复片段' }, stored_at: '2026-11-14T10:00:00Z' },
        { session_id: 'mirror-1', sequence: 2, provider_event_id: 'e2', kind: 'assistant_message', turn_id: 'turn-1', item_id: 'item-1', occurred_at: '2026-11-14T10:00:00Z', payload: { text: '完整回复正文' }, stored_at: '2026-11-14T10:00:00Z' },
        { session_id: 'mirror-1', sequence: 3, provider_event_id: 'e3', kind: 'assistant_delta', turn_id: 'turn-1', item_id: 'item-2', occurred_at: '2026-11-14T10:00:00Z', payload: { text: '另一个条目的实时片段' }, stored_at: '2026-11-14T10:00:00Z' },
        { session_id: 'mirror-1', sequence: 4, provider_event_id: 'e4', kind: 'reasoning_summary', occurred_at: '2026-11-14T10:00:00Z', payload: { explanation: '先检查依赖配置' }, stored_at: '2026-11-14T10:00:00Z' },
        { session_id: 'mirror-1', sequence: 5, provider_event_id: 'e5', kind: 'tool_call', occurred_at: '2026-11-14T10:00:01Z', payload: {
          tool: 'exec_command', summary: 'Run command', call_id: 'call-42', status: 'inProgress',
          input: '{"command":"git status --short"}', input_part: 0, input_parts: 1,
        } as unknown as components['schemas']['CodexEvent']['payload'], stored_at: '2026-11-14T10:00:01Z' },
        { session_id: 'mirror-1', sequence: 6, provider_event_id: 'e6', kind: 'tool_result', occurred_at: '2026-11-14T10:00:02Z', payload: {
          tool: 'exec_command', summary: 'Command output', call_id: 'call-42', status: 'completed',
          output: '{"stdout":"line 1\\nline 2","stderr":""}', output_part: 0, output_parts: 1, exit_code: 0,
        } as unknown as components['schemas']['CodexEvent']['payload'], stored_at: '2026-11-14T10:00:02Z' },
        { session_id: 'mirror-1', sequence: 7, provider_event_id: 'e7', kind: 'unsupported', occurred_at: '2026-11-14T10:00:03Z', payload: {
          provider_type: 'future_item', diagnostic: 'Morde 暂不支持此 Codex item 类型。',
        }, stored_at: '2026-11-14T10:00:03Z' },
      ], current_sequence: 7, next_cursor: null,
    });
    const user = userEvent.setup();
    renderPage();

    expect(await screen.findByRole('heading', { name: 'Codex 会话' })).toBeInTheDocument();
    expect(await screen.findByText('本机连接：运行中')).toBeInTheDocument();
    expect(screen.getByText(/会话发现完成，历史按会话状态继续补齐/)).toBeInTheDocument();
    await user.click(await screen.findByRole('button', { name: /本机对话/ }));

    expect(await screen.findByText('推理摘要')).toBeInTheDocument();
    expect(screen.getByText('先检查依赖配置')).toBeInTheDocument();
    expect(screen.getByText('完整回复正文')).toBeInTheDocument();
    expect(screen.queryByText('尚未完成的回复片段')).not.toBeInTheDocument();
    expect(screen.getByText('另一个条目的实时片段')).toBeInTheDocument();
    expect(screen.getByText('Morde 暂不支持此 Codex item 类型。')).toBeInTheDocument();

    const callSummary = screen.getByText('工具调用 · exec_command').closest('summary');
    const callCard = callSummary?.closest('details');
    expect(callCard).not.toHaveAttribute('open');
    await user.click(callSummary!);
    expect(callCard).toHaveAttribute('open');
    expect(within(callCard!).getByText('工具调用')).toBeInTheDocument();
    expect(within(callCard!).getByText('状态：').closest('p')).toHaveTextContent('执行中');
    expect(within(callCard!).getByText('call-42')).toBeInTheDocument();
    expect(within(callCard!).getAllByText(/片段 0 \/ 1（序号从 0 开始）/)).toHaveLength(2);
    expect(within(callCard!).getByText('{"command":"git status --short"}')).toBeInTheDocument();

    const resultSummary = screen.getByText('工具结果 · exec_command').closest('summary');
    const resultCard = resultSummary?.closest('details');
    await user.click(resultSummary!);
    expect(resultCard).toHaveAttribute('open');
    expect(within(resultCard!).getByText('工具结果')).toBeInTheDocument();
    expect(within(resultCard!).getByText('状态：').closest('p')).toHaveTextContent('已完成');
    expect(within(resultCard!).getAllByText(/片段 0 \/ 1（序号从 0 开始）/)).toHaveLength(2);
    expect(within(resultCard!).getByText('{"stdout":"line 1\\nline 2","stderr":""}')).toBeInTheDocument();
    expect(within(resultCard!).getByText('退出码：').closest('p')).toHaveTextContent('0');
    expect(portalApi.codexSessionEvents).toHaveBeenCalledWith('mirror-1', 0, 100);
  });

  it('uses read, live-output and send as the default profile scope', async () => {
    const codexAuthorizeProfile = vi.fn().mockResolvedValue({ authorized: true });
    Object.defineProperty(window, 'morde', { configurable: true, value: {
      codexState: vi.fn().mockResolvedValue({ ...state, codex: { ...state.codex,
        profiles: [{ ...state.codex.profiles[0]!, connectionAuthorized: false, scopes: [] }], sessions: [] } }),
      codexListThreads: vi.fn().mockResolvedValue({ items: [], nextCursor: null }), codexAuthorizeProfile,
    } });
    const user = userEvent.setup();
    renderPage();

    expect(await screen.findByRole('checkbox', { name: /读取历史/ })).toBeChecked();
    expect(screen.getByRole('checkbox', { name: /同步实时输出/ })).toBeChecked();
    expect(screen.getByRole('checkbox', { name: /发送消息/ })).toBeChecked();
    expect(screen.getByRole('checkbox', { name: /中断任务/ })).not.toBeChecked();
    const connectButton = screen.getByRole('button', { name: '连接并同步全部会话' });
    await waitFor(() => expect(connectButton).toBeEnabled());
    await user.click(connectButton);
    expect(codexAuthorizeProfile).toHaveBeenCalledWith('default', ['read', 'live', 'send']);
  });
});
