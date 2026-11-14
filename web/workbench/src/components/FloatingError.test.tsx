import { cleanup, render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it } from 'vitest';
import FloatingError from './FloatingError';

afterEach(() => {
  cleanup();
});

describe('Workbench FloatingError', () => {
  it('renders errors in a non-blocking overlay and keeps actions available', async () => {
    const user = userEvent.setup();
    render(<main data-testid="workbench"><p>桌面内容</p><FloatingError>连接失败<button type="button">重试</button></FloatingError></main>);

    const alert = await screen.findByRole('alert');
    expect(alert.closest('[data-testid="workbench"]')).toBeNull();
    expect(alert.closest('#workbench-notice-region')).toBeInTheDocument();
    expect(screen.getByTestId('workbench').textContent).toContain('桌面内容');
    await user.click(screen.getByRole('button', { name: '重试' }));
    await user.click(screen.getByRole('button', { name: '关闭错误提示' }));
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });

  it('does not surface errors from a hidden workspace panel', () => {
    render(<section hidden><FloatingError>隐藏工作区错误</FloatingError></section>);

    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });

  it('does not surface errors from an inactive tab', () => {
    render(<section aria-hidden="true" style={{ visibility: 'hidden' }}><FloatingError>隐藏标签错误</FloatingError></section>);

    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });
});
