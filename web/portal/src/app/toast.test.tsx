import { cleanup, render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { ToastNoticeCenter, ToastProvider, useToast } from './toast';
import { FloatingNotice } from '../components/FloatingError';

function TriggerActionToast({ action }: { action: () => void }) {
  const { notify } = useToast();
  return <button type="button" onClick={() => notify('网络暂不可用', 'error', { label: '重试', onClick: action })}>显示错误</button>;
}

describe('ToastProvider', () => {
  afterEach(cleanup);

  it('opens a user error popup without placing it inside page content when enabled', async () => {
    const user = userEvent.setup();
    render(<ToastProvider><ToastNoticeCenter autoOpenErrors /><main data-testid="page-layout"><TriggerActionToast action={vi.fn()} /></main></ToastProvider>);

    await user.click(screen.getByRole('button', { name: '显示错误' }));

    const alert = await screen.findByRole('alert');
    expect(alert.closest('details')).toHaveAttribute('open');
    expect(alert.closest('.notice-center-panel')?.closest('main')).toBeNull();
    expect(screen.getByTestId('page-layout')).not.toContainElement(alert);
  });

  it('keeps action notifications in the notice center and page notices inline', async () => {
    const user = userEvent.setup();
    const retry = vi.fn();
    render(<ToastProvider><ToastNoticeCenter /><main data-testid="page-layout"><TriggerActionToast action={retry} /><FloatingNotice>云端状态待确认</FloatingNotice></main></ToastProvider>);

    await user.click(screen.getByRole('button', { name: '显示错误' }));
    const summary = await screen.findByLabelText('错误提示，1 条');
    expect(summary.closest('details')).not.toHaveAttribute('open');
    await user.click(summary);
    const alert = screen.getByRole('alert');
    expect(alert.closest('.notice-center-panel')).toBeInTheDocument();
    expect(screen.getByRole('status')).toHaveTextContent('云端状态待确认');
    expect(screen.getByTestId('page-layout')).toContainElement(screen.getByRole('status'));
    expect(document.querySelector('.toast-region')).toBeNull();
    expect(screen.getByTestId('page-layout')).not.toContainElement(alert);
    await user.click(screen.getByRole('button', { name: '重试' }));

    expect(retry).toHaveBeenCalledOnce();
    expect(screen.getByRole('alert')).toHaveTextContent('网络暂不可用');
  });
});
