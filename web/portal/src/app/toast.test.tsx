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
    render(<main data-testid="page-layout"><ToastProvider><TriggerActionToast action={retry} /></ToastProvider></main>);

    await user.click(screen.getByRole('button', { name: '显示错误' }));
    const alert = screen.getByRole('alert');
    const region = alert.closest('.toast-region');
    expect(region).toBeInTheDocument();
    expect(region?.parentElement).toBe(document.body);
    expect(screen.getByTestId('page-layout')).not.toContainElement(alert);
    await user.click(screen.getByRole('button', { name: '重试' }));

    expect(retry).toHaveBeenCalledOnce();
    expect(screen.getByRole('alert')).toHaveTextContent('网络暂不可用');
  });
});
