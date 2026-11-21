import { useState } from 'react';
import { cleanup, render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { ToastNoticeCenter, ToastProvider, useToast } from './toast';
import { FloatingNotice } from '../components/FloatingError';
import { Modal } from '../components/Modal';

function TriggerActionToast({ action }: { action: () => void }) {
  const { notify } = useToast();
  return <button type="button" onClick={() => notify('网络暂不可用', 'error', { label: '重试', onClick: action })}>显示错误</button>;
}

function ModalWithNotice() {
  const [open, setOpen] = useState(false);
  return <><TriggerActionToast action={() => {}} /><button type="button" onClick={() => setOpen(true)}>打开弹窗</button>
    {open && <Modal title="测试弹窗" onClose={() => setOpen(false)}><p>弹窗内容</p></Modal>}
  </>;
}

describe('ToastProvider', () => {
  afterEach(cleanup);

  it('shows a single error in the top-left overlay outside page content', async () => {
    const user = userEvent.setup();
    render(<ToastProvider><ToastNoticeCenter autoOpenErrors /><main data-testid="page-layout"><TriggerActionToast action={vi.fn()} /></main></ToastProvider>);

    await user.click(screen.getByRole('button', { name: '显示错误' }));

    const alert = await screen.findByRole('alert');
    expect(alert.closest('.error-notice-overlay')).toBeInTheDocument();
    expect(screen.getByTestId('page-layout')).not.toContainElement(alert);
    expect(within(alert).queryByRole('button', { name: '重试' })).not.toBeInTheDocument();
  });

  it('keeps the latest error in the overlay and page notices inline', async () => {
    const user = userEvent.setup();
    const retry = vi.fn();
    render(<ToastProvider><ToastNoticeCenter /><main data-testid="page-layout"><TriggerActionToast action={retry} /><FloatingNotice>云端状态待确认</FloatingNotice></main></ToastProvider>);

    await user.click(screen.getByRole('button', { name: '显示错误' }));
    const alert = screen.getByRole('alert');
    expect(alert.closest('.error-notice-overlay')).toBeInTheDocument();
    expect(screen.getByRole('status')).toHaveTextContent('云端状态待确认');
    expect(screen.getByTestId('page-layout')).toContainElement(screen.getByRole('status'));
    expect(document.querySelector('.toast-region')).toBeNull();
    expect(screen.getByTestId('page-layout')).not.toContainElement(alert);
    expect(screen.queryByRole('button', { name: '重试' })).not.toBeInTheDocument();
    expect(retry).not.toHaveBeenCalled();
    expect(screen.getByRole('alert')).toHaveTextContent('网络暂不可用');
  });

  it('keeps the global error overlay outside a modal', async () => {
    const user = userEvent.setup();
    render(<ToastProvider><ToastNoticeCenter autoOpenErrors /><ModalWithNotice /></ToastProvider>);
    await user.click(screen.getByRole('button', { name: '显示错误' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('网络暂不可用');

    await user.click(screen.getByRole('button', { name: '打开弹窗' }));
    expect(screen.getByRole('alert').closest('.error-notice-overlay')).toBeInTheDocument();
    await user.click(within(screen.getByRole('dialog', { name: '测试弹窗' })).getByRole('button', { name: '关闭' }));
    await user.click(screen.getByRole('button', { name: '关闭错误提示' }));
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });
});
