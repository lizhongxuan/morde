import { cleanup, render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it } from 'vitest';
import FloatingError from './FloatingError';

afterEach(() => {
  cleanup();
});

describe('Workbench FloatingError', () => {
  it('renders errors in their content flow and keeps actions available', async () => {
    const user = userEvent.setup();
    render(<main data-testid="workbench"><p>桌面内容</p><FloatingError>连接失败<button type="button">重试</button></FloatingError></main>);

    const alert = await screen.findByRole('alert');
    expect(alert.closest('[data-testid="workbench"]')).toBeInTheDocument();
    expect(alert.parentElement).toHaveAttribute('data-testid', 'workbench');
    expect(document.getElementById('workbench-notice-region')).toBeNull();
    expect(screen.getByTestId('workbench').textContent).toContain('桌面内容');
    await user.click(screen.getByRole('button', { name: '重试' }));
    await user.click(screen.getByRole('button', { name: '关闭错误提示' }));
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });

  it('keeps errors inside hidden workspace panels', () => {
    render(<section hidden><FloatingError>隐藏工作区错误</FloatingError></section>);

    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
    expect(document.querySelector('[hidden] .workbench-floating-error')).toBeInTheDocument();
  });

  it('does not surface errors from an inactive tab', () => {
    render(<section aria-hidden="true" style={{ visibility: 'hidden' }}><FloatingError>隐藏标签错误</FloatingError></section>);

    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });
});
