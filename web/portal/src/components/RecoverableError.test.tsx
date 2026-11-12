import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { MemoryRouter, Route, Routes, useLocation } from 'react-router';
import { RecoverableError } from './RecoverableError';
import { RecoverablePageBoundary } from '../app/PageErrorBoundary';

function CurrentPath() {
  return <output>{useLocation().pathname}</output>;
}

function renderError(initialEntries: string[], initialIndex: number) {
  window.history.replaceState(null, '', '/');
  window.history.pushState({ external: true }, '', '/browser-history-only');
  window.history.replaceState({ idx: initialIndex }, '', '/');
  render(<MemoryRouter initialEntries={initialEntries} initialIndex={initialIndex}>
    <Routes><Route path="*" element={<><RecoverableError title="页面暂时无法显示" message="稍后重试。" homePath="/assistant" /><CurrentPath /></>} /></Routes>
  </MemoryRouter>);
}

describe('RecoverableError', () => {
  afterEach(() => {
    cleanup();
    window.history.replaceState(null, '', '/');
    vi.restoreAllMocks();
  });

  it('returns to an in-app history entry', () => {
    renderError(['/assistant', '/settings'], 1);

    fireEvent.click(screen.getByRole('button', { name: '返回上一页' }));

    expect(screen.getByText('/assistant')).toBeInTheDocument();
  });

  it('uses the safe home route when there is no in-app history entry', () => {
    renderError(['/broken'], 0);

    fireEvent.click(screen.getByRole('button', { name: '返回上一页' }));

    expect(screen.getByText('/assistant')).toBeInTheDocument();
  });

  it('retries a render failure without exposing the exception', () => {
    let shouldThrow = true;
    function UnstablePage() {
      if (shouldThrow) throw new Error('{"error":{"code":"not_found"}}');
      return <p>页面已恢复</p>;
    }
    vi.spyOn(console, 'error').mockImplementation(() => undefined);
    render(<MemoryRouter initialEntries={['/assistant']}>
      <RecoverablePageBoundary homePath="/assistant" homeLabel="返回助手">
        <UnstablePage />
      </RecoverablePageBoundary>
    </MemoryRouter>);

    expect(screen.getByRole('heading', { name: '页面暂时无法显示' })).toBeInTheDocument();
    expect(screen.queryByText(/not_found/)).not.toBeInTheDocument();
    shouldThrow = false;
    fireEvent.click(screen.getByRole('button', { name: '重试' }));
    expect(screen.getByText('页面已恢复')).toBeInTheDocument();
  });
});
