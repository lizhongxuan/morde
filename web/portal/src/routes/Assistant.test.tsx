import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { portalApi } from '../app/api';
import { ToastProvider } from '../app/toast';
import { Assistant } from './Assistant';

const timestamp = '2026-10-22T08:00:00.000Z';
const model = {
  id: 'p1234567', name: '团队模型', base_url: 'https://models.example.test/v1', model_name: 'morde-test', shared: true, has_api_key: true,
  capabilities: { text: true, tools: false, vision: false }, checked_at: timestamp, created_at: timestamp, updated_at: timestamp,
} satisfies Awaited<ReturnType<typeof portalApi.modelConnections>>['items'][number];
const conversation = { id: 'c1234567', title: '你好', model_id: model.id, created_at: timestamp, updated_at: timestamp };
const message = { id: 'm1234567', conversation_id: conversation.id, role: 'user' as const, content: '你好', created_at: timestamp };
const reply = { id: 'm7654321', conversation_id: conversation.id, role: 'assistant' as const, content: '你好！', created_at: timestamp };

function renderPage() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(<QueryClientProvider client={client}><ToastProvider><MemoryRouter><Assistant /></MemoryRouter></ToastProvider></QueryClientProvider>);
}

function mockCommon(conversations = { items: [] as typeof conversation[] }) {
  vi.spyOn(portalApi, 'modelConnections').mockResolvedValue({ items: [model] });
  vi.spyOn(portalApi, 'preferences').mockResolvedValue({ assistant_name: 'Morde', selected_model_id: model.id, default_host_id: null, updated_at: timestamp });
  vi.spyOn(portalApi, 'instantChatConversations').mockResolvedValue(conversations);
  vi.spyOn(portalApi, 'instantChatMessages').mockResolvedValue({ items: [message, reply] });
}

describe('Assistant instant chat', () => {
  afterEach(() => { cleanup(); vi.restoreAllMocks(); });

  it('sends a message directly and displays the saved reply', async () => {
    mockCommon();
    const send = vi.spyOn(portalApi, 'sendInstantChatMessage').mockResolvedValue({ conversation, message, reply });
    const user = userEvent.setup();
    renderPage();
    await user.type(await screen.findByRole('textbox', { name: '消息内容' }), '你好');
    await user.click(screen.getByRole('button', { name: /发送/ }));
    await waitFor(() => expect(send).toHaveBeenCalledWith({ model_id: model.id, content: '你好' }, expect.any(String)));
    expect(await screen.findByText('你好！')).toBeInTheDocument();
  });

  it('continues a saved conversation using its model', async () => {
    mockCommon({ items: [conversation] });
    const send = vi.spyOn(portalApi, 'sendInstantChatMessage').mockResolvedValue({ conversation, message, reply });
    const user = userEvent.setup();
    renderPage();
    await user.click(await screen.findByRole('button', { name: /你好/ }));
    expect(await screen.findByText('你好！')).toBeInTheDocument();
    await user.type(screen.getByRole('textbox', { name: '消息内容' }), '继续');
    await user.click(screen.getByRole('button', { name: /发送/ }));
    await waitFor(() => expect(send).toHaveBeenCalledWith({ conversation_id: conversation.id, model_id: model.id, content: '继续' }, expect.any(String)));
    expect(screen.getByRole('combobox', { name: '选择 AI 模型' })).toBeDisabled();
  });

  it('reuses the request key when a reply is lost and the user retries', async () => {
    mockCommon();
    const send = vi.spyOn(portalApi, 'sendInstantChatMessage').mockRejectedValueOnce(new Error('连接中断')).mockResolvedValueOnce({ conversation, message, reply });
    const user = userEvent.setup();
    renderPage();
    await user.type(await screen.findByRole('textbox', { name: '消息内容' }), '你好');
    await user.click(screen.getByRole('button', { name: /发送/ }));
    await waitFor(() => expect(send).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(screen.getByRole('button', { name: /发送/ })).toBeEnabled());
    await user.click(screen.getByRole('button', { name: /发送/ }));
    await waitFor(() => expect(send).toHaveBeenCalledTimes(2));
    expect(send.mock.calls[1]?.[1]).toBe(send.mock.calls[0]?.[1]);
  });
});
