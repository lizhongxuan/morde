import { cleanup, render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it } from 'vitest';
import FloatingError from './FloatingError';

afterEach(() => {
  cleanup();
  document.getElementById('workbench-error-region')?.remove();
});

describe('Workbench FloatingError', () => {
  it('portals the error outside the workbench layout and keeps actions available', async () => {
    const region = document.createElement('div');
    region.id = 'workbench-error-region';
    document.body.append(region);
    const user = userEvent.setup();
    render(<main data-testid="workbench"><p>桌面内容</p><FloatingError>连接失败<button type="button">重试</button></FloatingError></main>);

    const alert = await screen.findByRole('alert');
    expect(alert.closest('#workbench-error-region')).toBe(region);
    expect(screen.getByTestId('workbench').querySelector('[role="alert"]')).toBeNull();
    await user.click(screen.getByRole('button', { name: '重试' }));
    await user.click(screen.getByRole('button', { name: '关闭错误提示' }));
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });

  it('does not surface errors from a hidden workspace panel', () => {
    const region = document.createElement('div');
    region.id = 'workbench-error-region';
    document.body.append(region);
    render(<section hidden><FloatingError>隐藏工作区错误</FloatingError></section>);

    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });
});
