import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import { ToastProvider, useToast } from './toast';

function TriggerActionToast({ action }: { action: () => void }) {
  const { notify } = useToast();
  return <button type="button" onClick={() => notify('网络暂不可用', 'error', { label: '重试', onClick: action })}>显示错误</button>;
}

describe('ToastProvider', () => {
  it('shows persistent actions in its fixed notification region', async () => {
    const user = userEvent.setup();
    const retry = vi.fn();
    render(<ToastProvider><TriggerActionToast action={retry} /></ToastProvider>);

    await user.click(screen.getByRole('button', { name: '显示错误' }));
    const alert = screen.getByRole('alert');
    expect(alert.closest('.toast-region')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: '重试' }));

    expect(retry).toHaveBeenCalledOnce();
    expect(screen.getByRole('alert')).toHaveTextContent('网络暂不可用');
  });
});
