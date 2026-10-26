import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { components } from '../api/schema';
import { portalApi } from '../app/api';
import { Notifications } from './Notifications';

const timestamp = '2026-10-22T08:00:00.000Z';
const unread: components['schemas']['Notification'] = { id: 'n1234567', kind: 'waiting_user', task_id: 'q1234567', event_sequence: 4, summary: '需要补充任务信息。', redacted: false, actionable: true, created_at: timestamp, read_at: null };
const read: components['schemas']['Notification'] = { ...unread, read_at: timestamp };
const secondPageRead: components['schemas']['Notification'] = { ...read, id: 'n7654321', task_id: 'q7654321', summary: '第二页通知。' };

function renderPage() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(<QueryClientProvider client={client}><MemoryRouter><Notifications /></MemoryRouter></QueryClientProvider>);
}

describe('Notifications', () => {
  afterEach(() => { cleanup(); vi.restoreAllMocks(); });

  it('marks a notification as read without acting on the associated task', async () => {
    vi.spyOn(portalApi, 'notifications').mockResolvedValueOnce({ items: [unread], next_cursor: null }).mockResolvedValue({ items: [read], next_cursor: null });
    const markRead = vi.spyOn(portalApi, 'markNotificationRead').mockResolvedValue(undefined);
    const taskAction = vi.spyOn(portalApi, 'taskAction');
    const user = userEvent.setup();
    renderPage();

    expect(await screen.findByText('需要补充任务信息。')).toBeInTheDocument();
    expect(screen.getByRole('link', { name: '打开任务详情' })).toHaveAttribute('href', '/tasks/q1234567');
    await user.click(screen.getByRole('button', { name: '标记已读' }));

    await waitFor(() => expect(markRead).toHaveBeenCalledWith(unread.id));
    expect(taskAction).not.toHaveBeenCalled();
    expect(await screen.findByText('已读')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: '标记已读' })).not.toBeInTheDocument();
  });

  it('hides content when the server redacts it', async () => {
    vi.spyOn(portalApi, 'notifications').mockResolvedValue({ items: [{ ...unread, redacted: true, summary: null, actionable: false }], next_cursor: null });
    renderPage();
    expect(await screen.findByText('相关内容因当前权限而隐藏。')).toBeInTheDocument();
    expect(screen.getByText('关联请求已处理、已失效或当前不可访问。')).toBeInTheDocument();
    expect(screen.queryByText(unread.summary!)).not.toBeInTheDocument();
  });

  it('filters unread notifications through the API', async () => {
    const list = vi.spyOn(portalApi, 'notifications').mockResolvedValue({ items: [unread], next_cursor: null });
    const user = userEvent.setup();
    renderPage();

    expect(await screen.findByText('需要补充任务信息。')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: '未读' }));
    await waitFor(() => expect(list).toHaveBeenLastCalledWith(undefined, true));
    expect(screen.getByRole('button', { name: '未读' })).toHaveAttribute('aria-pressed', 'true');
  });

  it('keeps loaded notifications visible when loading another page fails', async () => {
    vi.spyOn(portalApi, 'notifications')
      .mockResolvedValueOnce({ items: [unread], next_cursor: 'next' })
      .mockRejectedValueOnce(new Error('网络暂不可用'))
      .mockResolvedValueOnce({ items: [secondPageRead], next_cursor: null });
    const user = userEvent.setup();
    renderPage();

    expect(await screen.findByText('需要补充任务信息。')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: '加载更多通知' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('加载更多通知失败');
    expect(screen.getByText('需要补充任务信息。')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: '重试' }));
    expect(await screen.findByText('已读')).toBeInTheDocument();
  });
});
