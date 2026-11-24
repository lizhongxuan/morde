import { cleanup, render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { ToastNoticeCenter, ToastProvider } from '../app/toast';
import { FloatingError, FloatingNotice } from './FloatingError';

afterEach(cleanup);

describe('FloatingError', () => {
  it('moves API errors to one global notice without retry controls', async () => {
    const retry = vi.fn();
    const user = userEvent.setup();
    render(<ToastProvider><ToastNoticeCenter /><main data-testid="page"><p>页面内容</p><FloatingError className="error-card">
      <span>接口暂不可用</span><button type="button" onClick={retry}>重试</button>
    </FloatingError></main></ToastProvider>);

    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent('接口暂不可用');
    expect(alert.closest('.error-notice-overlay')).toBeInTheDocument();
    expect(screen.getByTestId('page')).not.toContainElement(alert);
    expect(within(alert).queryByRole('button', { name: '重试' })).not.toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: '关闭错误提示' }));
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
    expect(retry).not.toHaveBeenCalled();
  });

  it('keeps an explicit page recovery action inline when requested', async () => {
    const retry = vi.fn();
    const user = userEvent.setup();
    render(<ToastProvider><ToastNoticeCenter /><main><FloatingError inline>同步未完成。<button type="button" onClick={retry}>重试保存</button></FloatingError></main></ToastProvider>);

    const alert = screen.getByRole('alert');
    expect(alert).toHaveTextContent('同步未完成。');
    expect(alert.closest('.error-notice-overlay')).not.toBeInTheDocument();
    await user.click(within(alert).getByRole('button', { name: '重试保存' }));
    expect(retry).toHaveBeenCalledOnce();
  });

  it('renders inline without an application provider', () => {
    const { container } = render(<main><p>页面内容</p><FloatingError>接口暂不可用</FloatingError></main>);
    expect(screen.getByRole('alert').parentElement).toBe(container.querySelector('main'));
  });

  it('keeps errors inside hidden page sections without announcing them', () => {
    render(<ToastProvider><ToastNoticeCenter /><section hidden><FloatingError>隐藏页面错误</FloatingError></section></ToastProvider>);
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
    expect(screen.queryByText('隐藏页面错误')).not.toBeInTheDocument();
  });
});

describe('FloatingNotice', () => {
  it('keeps status messages in their page flow', () => {
    render(<ToastProvider><ToastNoticeCenter /><main data-testid="page"><FloatingNotice className="notice-warning">服务当前不可用</FloatingNotice><p>页面内容</p></main></ToastProvider>);
    const status = screen.getByRole('status');
    expect(status).toHaveTextContent('服务当前不可用');
    expect(status.parentElement).toBe(screen.getByTestId('page'));
    expect(screen.getByTestId('page')).toHaveTextContent('页面内容');
    expect(document.querySelector('.notice-center')).toHaveAttribute('hidden');
  });

  it('keeps notices inside hidden sections', () => {
    render(<ToastProvider><ToastNoticeCenter /><section hidden><FloatingNotice>隐藏提示</FloatingNotice></section></ToastProvider>);
    expect(screen.queryByRole('status')).not.toBeInTheDocument();
    expect(document.querySelector('[hidden] [role="status"]')).toHaveTextContent('隐藏提示');
  });
});
