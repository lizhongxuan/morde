import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { ScreenPagination } from './ScreenPagination';

const items = Array.from({ length: 5 }, (_, index) => ({ id: `row-${index}`, label: `Row ${index + 1}` }));
const emptyItems: typeof items = [];

describe('ScreenPagination', () => {
  afterEach(() => { cleanup(); vi.restoreAllMocks(); vi.unstubAllGlobals(); });

  it('keeps page controls, hides other pages, and preserves their mounted content', () => {
    render(<ScreenPagination items={items} getKey={(item) => item.id} estimatedItemHeight={30} height={68} renderItem={(item) => <button>{item.label}</button>} />);

    expect(screen.getByText('Row 1')).toBeInTheDocument();
    expect(screen.getByText('Row 2')).toBeInTheDocument();
    expect(screen.getByText('Row 3')).not.toBeVisible();
    expect(screen.getByRole('button', { name: '下一页' })).toBeEnabled();

    fireEvent.click(screen.getByRole('button', { name: '下一页' }));
    expect(screen.getByText('Row 3')).toBeVisible();
    expect(screen.getByText('Row 1')).not.toBeVisible();
  });

  it('retains the current item when items before it are removed', async () => {
    const view = render(<ScreenPagination items={items} getKey={(item) => item.id} estimatedItemHeight={30} height={68} renderItem={(item) => <span>{item.label}</span>} />);
    fireEvent.click(screen.getByRole('button', { name: '第 2 页' }));
    expect(screen.getByText('Row 3')).toBeInTheDocument();

    view.rerender(<ScreenPagination items={items.slice(2)} getKey={(item) => item.id} estimatedItemHeight={30} height={68} renderItem={(item) => <span>{item.label}</span>} />);
    await waitFor(() => expect(screen.getByText('Row 3')).toBeInTheDocument());
    expect(screen.getByText('1 / 2')).toBeInTheDocument();
  });

  it('keeps the current item visible when the viewport or measured row height repaginates the list', async () => {
    const longItems = Array.from({ length: 10 }, (_, index) => ({ id: `row-${index}`, label: `Row ${index + 1}` }));
    let viewportHeight = 120;
    let itemHeight = 30;
    const resizeCallbacks: ResizeObserverCallback[] = [];
    vi.stubGlobal('ResizeObserver', class {
      constructor(callback: ResizeObserverCallback) { resizeCallbacks.push(callback); }
      observe() {}
      disconnect() {}
    });
    vi.spyOn(HTMLElement.prototype, 'clientHeight', 'get').mockImplementation(function (this: HTMLElement) {
      return this.classList.contains('screen-pagination__viewport') ? viewportHeight : 0;
    });
    vi.spyOn(HTMLElement.prototype, 'getBoundingClientRect').mockImplementation(function (this: HTMLElement) {
      const height = this.classList.contains('screen-pagination__viewport') ? viewportHeight
        : this.hasAttribute('data-item-key') && !this.hasAttribute('hidden') ? itemHeight : 0;
      return { x: 0, y: 0, top: 0, right: 100, bottom: height, left: 0, width: 100, height, toJSON: () => ({}) } as DOMRect;
    });
    vi.spyOn(HTMLElement.prototype, 'scrollHeight', 'get').mockImplementation(function (this: HTMLElement) {
      return this.hasAttribute('data-item-key') && !this.hasAttribute('hidden') ? itemHeight : this.clientHeight;
    });

    render(<ScreenPagination items={longItems} getKey={(item) => item.id} estimatedItemHeight={30} height={120} renderItem={(item) => <span>{item.label}</span>} />);
    await waitFor(() => expect(screen.getByText('1 / 4')).toBeInTheDocument());
    fireEvent.click(screen.getByRole('button', { name: '第 4 页' }));
    expect(screen.getByText('Row 10')).toBeVisible();

    viewportHeight = 68;
    await act(async () => {
      resizeCallbacks.at(-1)?.([] as unknown as ResizeObserverEntry[], {} as ResizeObserver);
    });
    await waitFor(() => expect(screen.getByText('Row 10')).toBeVisible());
    expect(screen.getByText('5 / 5')).toBeInTheDocument();

    itemHeight = 40;
    await act(async () => {
      resizeCallbacks.at(-1)?.([] as unknown as ResizeObserverEntry[], {} as ResizeObserver);
    });
    await waitFor(() => expect(screen.getByText('6 / 6')).toBeInTheDocument());
    expect(screen.getByText('Row 10')).toBeVisible();
    expect(screen.getByText('Row 9')).not.toBeVisible();
  });

  it('supports keyboard paging without stealing keys from inputs', () => {
    const onRender = vi.fn((item: typeof items[number]) => <span>{item.label}<input aria-label={`input-${item.label}`} /></span>);
    render(<ScreenPagination items={items} getKey={(item) => item.id} estimatedItemHeight={30} height={68} renderItem={onRender} />);
    const region = screen.getByLabelText('分页列表内容');
    region.focus();
    fireEvent.keyDown(region, { key: 'PageDown' });
    expect(screen.getByText('Row 3')).toBeInTheDocument();

    const input = screen.getByLabelText('input-Row 3');
    fireEvent.keyDown(input, { key: 'PageDown' });
    expect(screen.getByText('Row 3')).toBeInTheDocument();
  });

  it('keeps a hidden page draft mounted and out of focus order', () => {
    render(<ScreenPagination items={items} getKey={(item) => item.id} estimatedItemHeight={30} height={68} renderItem={(item) => <input aria-label={item.label} />} />);
    const draft = screen.getByLabelText('Row 1') as HTMLInputElement;
    fireEvent.change(draft, { target: { value: 'unsent draft' } });
    fireEvent.click(screen.getByRole('button', { name: '下一页' }));

    expect(draft.value).toBe('unsent draft');
    expect(draft.closest('.screen-pagination__item')).toHaveAttribute('hidden');
    draft.focus();
    expect(document.activeElement).not.toBe(draft);

    fireEvent.click(screen.getByRole('button', { name: '上一页' }));
    expect(draft).toBeVisible();
    expect(draft.value).toBe('unsent draft');
  });

  it('keeps an oversized row reachable without turning its keyboard paging keys into list navigation', async () => {
    vi.spyOn(HTMLElement.prototype, 'getBoundingClientRect').mockImplementation(function (this: HTMLElement) {
      const height = this.classList.contains('screen-pagination__viewport') ? 68
        : this.dataset.itemKey === 'string:row-0' ? 120
          : this.hasAttribute('data-item-key') ? 30 : 0;
      return { x: 0, y: 0, top: 0, right: 100, bottom: height, left: 0, width: 100, height, toJSON: () => ({}) } as DOMRect;
    });
    vi.spyOn(HTMLElement.prototype, 'clientHeight', 'get').mockImplementation(function (this: HTMLElement) {
      return this.classList.contains('screen-pagination__viewport') ? 68 : 0;
    });
    vi.spyOn(HTMLElement.prototype, 'scrollHeight', 'get').mockImplementation(function (this: HTMLElement) {
      return this.dataset.itemKey === 'string:row-0' ? 120 : this.clientHeight;
    });

    render(<ScreenPagination items={items} getKey={(item) => item.id} estimatedItemHeight={30} height={68} renderItem={(item) => <span>{item.label}</span>} />);

    const oversizedRow = screen.getByText('Row 1').closest<HTMLElement>('.screen-pagination__item')!;
    await waitFor(() => expect(oversizedRow).toHaveAttribute('data-screen-pagination-scroll'));
    expect(oversizedRow).toHaveAttribute('role', 'region');
    expect(oversizedRow).toHaveAttribute('tabindex', '0');
    expect(oversizedRow).toHaveAttribute('aria-label', '分页列表条目内容，可在此区域内滚动查看');
    expect(oversizedRow).toHaveStyle({ maxHeight: '68px' });
    expect(screen.getByText('Row 2')).not.toBeVisible();

    fireEvent.keyDown(oversizedRow, { key: 'PageDown' });
    expect(oversizedRow.scrollTop).toBeGreaterThan(0);
    expect(screen.getByText('Row 2')).not.toBeVisible();
    fireEvent.keyDown(oversizedRow, { key: 'End' });
    expect(oversizedRow.scrollTop).toBe(120);
    fireEvent.keyDown(oversizedRow, { key: 'Home' });
    expect(oversizedRow.scrollTop).toBe(0);
  });

  it('shows an empty state when there are no items', () => {
    render(<ScreenPagination items={emptyItems} getKey={(item) => item.id} renderItem={(item) => item.label} renderEmpty={() => <p>暂无记录</p>} />);
    expect(screen.getByText('暂无记录')).toBeInTheDocument();
  });

  it('starts at the first page after a populated list is emptied and loaded again', async () => {
    const view = render(<ScreenPagination items={items} getKey={(item) => item.id} estimatedItemHeight={30} height={68} renderItem={(item) => <span>{item.label}</span>} />);
    fireEvent.click(screen.getByRole('button', { name: '第 3 页' }));
    expect(screen.getByText('Row 5')).toBeVisible();

    view.rerender(<ScreenPagination items={emptyItems} getKey={(item) => item.id} estimatedItemHeight={30} height={68} renderItem={(item) => <span>{item.label}</span>} renderEmpty={() => <p>暂无记录</p>} />);
    expect(screen.getByText('暂无记录')).toBeInTheDocument();
    view.rerender(<ScreenPagination items={items} getKey={(item) => item.id} estimatedItemHeight={30} height={68} renderItem={(item) => <span>{item.label}</span>} />);

    await waitFor(() => expect(screen.getByText('1 / 3')).toBeInTheDocument());
    expect(screen.getByText('Row 1')).toBeVisible();
    expect(screen.getByText('Row 5')).not.toBeVisible();
  });
});
