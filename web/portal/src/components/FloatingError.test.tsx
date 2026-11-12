import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import { ToastProvider } from '../app/toast';
import { FloatingError } from './FloatingError';

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
