import { cleanup, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { ToastProvider } from '../app/toast';
import { FloatingError, FloatingNotice } from './FloatingError';

afterEach(cleanup);

describe('FloatingError', () => {
  it('shows recoverable API errors outside the page layout', async () => {
    const retry = vi.fn();
    const user = userEvent.setup();
    render(<ToastProvider><main data-testid="page"><p>页面内容</p><FloatingError className="error-card">
      <span>接口暂不可用</span><button type="button" onClick={retry}>重试</button>
    </FloatingError></main></ToastProvider>);

    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent('接口暂不可用');
    expect(alert.closest('.toast-region')).toBeInTheDocument();
    expect(screen.getByTestId('page').querySelector('.error-card')).toBeNull();
    await user.click(screen.getByRole('button', { name: '重试' }));
    expect(retry).toHaveBeenCalledOnce();
    await user.click(screen.getByRole('button', { name: '关闭错误提示' }));
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });

  it('keeps errors floating when a route is rendered without the application provider', async () => {
    const { container } = render(<main><p>页面内容</p><FloatingError>接口暂不可用</FloatingError></main>);

    const alert = await screen.findByRole('alert');
    expect(alert.parentElement).toBe(document.body);
    expect(container.querySelector('[role="alert"]')).toBeNull();
  });

  it('does not surface errors from hidden tabs', async () => {
    const { rerender } = render(<ToastProvider><section hidden><FloatingError>隐藏页面错误</FloatingError></section></ToastProvider>);

    expect(screen.queryByText('隐藏页面错误')).not.toBeInTheDocument();
    rerender(<ToastProvider><section><FloatingError>隐藏页面错误</FloatingError></section></ToastProvider>);
    expect((await screen.findByText('隐藏页面错误')).closest('[role="alert"]')).toBeInTheDocument();
  });
});

describe('FloatingNotice', () => {
  it('keeps page status messages out of the layout and makes them available on demand', async () => {
    const user = userEvent.setup();
    const { rerender } = render(<ToastProvider><main data-testid="page"><FloatingNotice className="notice-warning">服务当前不可用</FloatingNotice><p>页面内容</p></main></ToastProvider>);
    expect(screen.getByTestId('page')).not.toHaveTextContent('服务当前不可用');
    const summary = await screen.findByLabelText('状态提示，1 条');
    expect(summary.closest('details')).not.toHaveAttribute('open');
    await user.click(summary);
    expect(screen.getByRole('status')).toHaveTextContent('服务当前不可用');
    rerender(<ToastProvider><main data-testid="page"><p>页面内容</p></main></ToastProvider>);
    await waitFor(() => expect(document.querySelector('.notice-center')).toHaveAttribute('hidden'));
  });

  it('does not show notices from hidden sections', async () => {
    render(<ToastProvider><section hidden><FloatingNotice>隐藏提示</FloatingNotice></section></ToastProvider>);
    expect(screen.queryByText('隐藏提示')).not.toBeInTheDocument();
    expect(document.querySelector('.notice-center')).toHaveAttribute('hidden');
  });

  it('groups simultaneous notices without adding them to the conversation layout', async () => {
    const user = userEvent.setup();
    render(<ToastProvider><main data-testid="page"><FloatingNotice data-notice-label="演示模式">演示服务</FloatingNotice>
      <FloatingNotice>对话处理待确认</FloatingNotice><FloatingNotice>云端工具不可用</FloatingNotice><p>对话内容</p></main></ToastProvider>);
    expect(screen.getByTestId('page')).toHaveTextContent('对话内容');
    expect(screen.getByTestId('page')).not.toHaveTextContent('对话处理待确认');
    await user.click(await screen.findByLabelText('演示模式，3 条'));
    expect(screen.getByText('演示服务')).toBeVisible();
    expect(screen.getByText('对话处理待确认')).toBeVisible();
    expect(screen.getByText('云端工具不可用')).toBeVisible();
  });
});
