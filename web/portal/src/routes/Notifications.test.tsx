import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, render, screen, waitFor, within } from '@testing-library/react';
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

  it('shows historical notifications without a dead task link', async () => {
    vi.spyOn(portalApi, 'notifications').mockResolvedValueOnce({ items: [unread], next_cursor: null }).mockResolvedValue({ items: [read], next_cursor: null });
    const markRead = vi.spyOn(portalApi, 'markNotificationRead').mockResolvedValue(undefined);
    const user = userEvent.setup();
    renderPage();

    expect(await screen.findByText('需要补充任务信息。')).toBeInTheDocument();
    expect(screen.queryByRole('link', { name: '打开任务详情' })).not.toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: '标记已读' }));

    await waitFor(() => expect(markRead).toHaveBeenCalledWith(unread.id));
    expect(await screen.findByText('已读')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: '标记已读' })).not.toBeInTheDocument();
  });

  it('hides content when the server redacts it', async () => {
    vi.spyOn(portalApi, 'notifications').mockResolvedValue({ items: [{ ...unread, redacted: true, summary: null, actionable: false }], next_cursor: null });
    renderPage();
    expect(await screen.findByText('相关内容因当前权限而隐藏。')).toBeInTheDocument();
    expect(screen.queryByText('关联请求已处理、已失效或当前不可访问。')).not.toBeInTheDocument();
    expect(screen.queryByText(unread.summary!)).not.toBeInTheDocument();
  });

  it('keeps notification content hidden after a permission error and retries loading', async () => {
    const list = vi.spyOn(portalApi, 'notifications')
      .mockRejectedValueOnce(Object.assign(new Error('当前账号无权读取通知'), { status: 403 }))
      .mockResolvedValue({ items: [unread], next_cursor: null });
    const user = userEvent.setup();
    renderPage();

    expect(await screen.findByRole('alert')).toHaveTextContent('当前账号无权读取通知');
    expect(screen.queryByText(unread.summary!)).not.toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: '重试' }));
    expect(await screen.findByText(unread.summary!)).toBeInTheDocument();
    expect(list).toHaveBeenCalledTimes(2);
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

  it('paginates loaded notifications and keeps mark-read actions on the selected page', async () => {
    const secondUnread = { ...unread, id: 'n7654321', task_id: 'q7654321', summary: '第二页未读通知。' };
    let rows = [unread, secondUnread];
    vi.spyOn(portalApi, 'notifications').mockImplementation(async () => ({ items: [...rows], next_cursor: null }));
    vi.spyOn(portalApi, 'markNotificationRead').mockImplementation(async (id) => {
      rows = rows.map((item) => item.id === id ? { ...item, read_at: timestamp } : item);
      return undefined;
    });
    const user = userEvent.setup();
    renderPage();

    expect(await screen.findByText('需要补充任务信息。')).toBeInTheDocument();
    const pagination = screen.getByRole('navigation', { name: '通知列表分页' });
    const firstRow = screen.getByText('需要补充任务信息。').closest<HTMLElement>('.screen-pagination__item')!;
    const secondRow = screen.getByText('第二页未读通知。').closest<HTMLElement>('.screen-pagination__item')!;
    expect(firstRow).not.toHaveAttribute('hidden');
    expect(secondRow).toHaveAttribute('hidden');

    await user.click(within(pagination).getByRole('button', { name: '下一页' }));
    expect(firstRow).toHaveAttribute('hidden');
    expect(secondRow).not.toHaveAttribute('hidden');
    await user.click(within(secondRow).getByRole('button', { name: '标记已读' }));

    await waitFor(() => expect(within(secondRow).getByText('已读')).toBeInTheDocument());
    expect(within(secondRow).queryByRole('button', { name: '标记已读' })).not.toBeInTheDocument();
    expect(screen.getByRole('navigation', { name: '通知列表分页' })).toBeInTheDocument();
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
