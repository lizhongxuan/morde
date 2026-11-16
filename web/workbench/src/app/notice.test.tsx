import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { useWorkbenchNotice, WorkbenchNoticeProvider } from './notice';

function NoticeTrigger() {
  const notify = useWorkbenchNotice();
  return <button type="button" onClick={() => notify('网络连接失败', { label: '重试', onClick: () => undefined })}>通知</button>;
}

describe('Workbench notices', () => {
  afterEach(cleanup);

  it('shows a fixed-region notice without replacing the page and runs its retry action', () => {
    const retry = vi.fn();
    function Trigger() {
      const notify = useWorkbenchNotice();
      return <button type="button" onClick={() => notify('网络连接失败', { label: '重试', onClick: retry })}>通知</button>;
    }
    render(<WorkbenchNoticeProvider><div>原页面内容</div><Trigger /></WorkbenchNoticeProvider>);

    fireEvent.click(screen.getByRole('button', { name: '通知' }));
    expect(screen.getByText('原页面内容')).toBeVisible();
    expect(screen.getByLabelText('网络错误提示')).toHaveClass('workbench-notice-region', 'workbench-network-notice-region');
    fireEvent.click(screen.getByRole('button', { name: '重试' }));
    expect(retry).toHaveBeenCalledOnce();
  });

  it('has a no-op default outside the application provider', () => {
    render(<NoticeTrigger />);
    fireEvent.click(screen.getByRole('button', { name: '通知' }));
    expect(screen.queryByLabelText('网络错误提示')).not.toBeInTheDocument();
  });
});
