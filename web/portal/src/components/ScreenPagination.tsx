import { useLayoutEffect, useMemo, useRef, useState, type CSSProperties, type KeyboardEvent, type ReactNode } from 'react';
import './ScreenPagination.css';

type ItemKey = string | number;

export interface ScreenPaginationProps<T> {
  items: readonly T[];
  /** Return a unique, stable key for each row. */
  getKey: (item: T) => ItemKey;
  renderItem: (item: T, index: number) => ReactNode;
  estimatedItemHeight?: number;
  height?: number | string;
  gap?: number;
  ariaLabel?: string;
  className?: string;
  renderEmpty?: () => ReactNode;
}

interface PageRange {
  start: number;
  end: number;
}

function token(key: ItemKey): string {
  return `${typeof key}:${String(key)}`;
}

function findPage(ranges: readonly PageRange[], itemIndex: number): number {
  return Math.max(0, ranges.findIndex(({ start, end }) => itemIndex >= start && itemIndex < end));
}

function pageNumbers(page: number, count: number): Array<number | 'ellipsis-start' | 'ellipsis-end'> {
  if (count <= 7) return Array.from({ length: count }, (_, index) => index);
  const visible = new Set([0, count - 1, page - 1, page, page + 1].filter((item) => item >= 0 && item < count));
  const result: Array<number | 'ellipsis-start' | 'ellipsis-end'> = [];
  let previous = -1;
  for (const current of [...visible].sort((a, b) => a - b)) {
    if (current - previous > 1) result.push(previous === 0 ? 'ellipsis-end' : 'ellipsis-start');
    result.push(current);
    previous = current;
  }
  return result;
}

export function ScreenPagination<T>({
  items,
  getKey,
  renderItem,
  estimatedItemHeight = 72,
  height = 'min(72dvh, 720px)',
  gap = 8,
  ariaLabel = '分页列表',
  className = '',
  renderEmpty,
}: ScreenPaginationProps<T>) {
  const viewportRef = useRef<HTMLDivElement>(null);
  const anchorToken = useRef<string | null>(null);
  const previousKeys = useRef<string[]>([]);
  const measuredHeights = useRef(new Map<string, number>());
  const [viewportHeight, setViewportHeight] = useState(0);
  const [heightRevision, setHeightRevision] = useState(0);
  const [page, setPage] = useState(0);

  const keys = useMemo(() => items.map((item) => token(getKey(item))), [items, getKey]);
  const keySignature = JSON.stringify(keys);
  const ranges = useMemo<PageRange[]>(() => {
    if (!items.length) return [];
    const available = Math.max(1, viewportHeight || (typeof height === 'number' ? height : 0));
    const result: PageRange[] = [];
    let start = 0;
    let used = 0;
    for (let index = 0; index < items.length; index += 1) {
      const itemHeight = measuredHeights.current.get(keys[index] ?? '') ?? Math.max(1, estimatedItemHeight);
      const nextHeight = used + (index > start ? gap : 0) + itemHeight;
      if (index > start && nextHeight > available) {
        result.push({ start, end: index });
        start = index;
        used = itemHeight;
      } else {
        used = nextHeight;
      }
    }
    result.push({ start, end: items.length });
    return result;
  }, [items, keys, estimatedItemHeight, gap, height, viewportHeight, heightRevision]);
  const rangeSignature = ranges.map(({ start, end }) => `${start}:${end}`).join('|');
  const pageByIndex = useMemo(() => {
    const result = Array<number>(items.length).fill(0);
    ranges.forEach(({ start, end }, pageIndex) => {
      for (let index = start; index < end; index += 1) result[index] = pageIndex;
    });
    return result;
  }, [items.length, rangeSignature]);
  const pageCount = ranges.length;
  const currentPage = Math.min(page, Math.max(0, pageCount - 1));
  const currentRange = ranges[currentPage];

  useLayoutEffect(() => {
    if (previousKeys.current.length === keys.length && previousKeys.current.every((key, index) => key === keys[index])) return;
    previousKeys.current = keys;
    if (!anchorToken.current) return;
    const index = keys.indexOf(anchorToken.current);
    if (index >= 0) {
      const nextPage = findPage(ranges, index);
      setPage(nextPage);
      anchorToken.current = keys[ranges[nextPage]?.start ?? index] ?? anchorToken.current;
    } else {
      const nextPage = Math.min(currentPage, Math.max(0, pageCount - 1));
      setPage(nextPage);
      anchorToken.current = keys[ranges[nextPage]?.start ?? 0] ?? null;
    }
  }, [keySignature, ranges, rangeSignature, currentPage, pageCount]);

  useLayoutEffect(() => {
    const viewport = viewportRef.current;
    if (!viewport) return;
    const updateViewport = () => {
      const nextHeight = viewport.clientHeight;
      setViewportHeight((current) => Math.abs(current - nextHeight) < 1 ? current : nextHeight);
      for (const element of viewport.querySelectorAll<HTMLElement>('[data-item-key]')) {
        const itemToken = element.dataset.itemKey;
        if (!itemToken) continue;
        const nextItemHeight = element.getBoundingClientRect().height;
        if (nextItemHeight > 0 && Math.abs((measuredHeights.current.get(itemToken) ?? 0) - nextItemHeight) > 1) {
          measuredHeights.current.set(itemToken, nextItemHeight);
          setHeightRevision((revision) => revision + 1);
        }
      }
    };
    updateViewport();
    if (typeof ResizeObserver === 'undefined') return;
    const observer = new ResizeObserver(() => updateViewport());
    observer.observe(viewport);
    viewport.querySelectorAll('[data-item-key]').forEach((element) => observer.observe(element));
    return () => observer.disconnect();
  }, [currentPage, rangeSignature, keySignature]);

  useLayoutEffect(() => {
    if (pageCount && page >= pageCount) setPage(pageCount - 1);
  }, [page, pageCount]);

  function goToPage(nextPage: number) {
    const boundedPage = Math.max(0, Math.min(pageCount - 1, nextPage));
    setPage(boundedPage);
    anchorToken.current = keys[ranges[boundedPage]?.start ?? 0] ?? null;
  }

  function onKeyDown(event: KeyboardEvent<HTMLElement>) {
    const target = event.target;
    if (target instanceof HTMLElement && target.closest('input, textarea, select, button, a, [contenteditable="true"]')) return;
    if (event.key === 'ArrowLeft' || event.key === 'PageUp') {
      event.preventDefault();
      goToPage(currentPage - 1);
    } else if (event.key === 'ArrowRight' || event.key === 'PageDown') {
      event.preventDefault();
      goToPage(currentPage + 1);
    }
  }

  return <section
    className={`screen-pagination ${className}`.trim()}
    style={{ height: typeof height === 'number' ? `${height}px` : height, '--screen-pagination-gap': `${gap}px` } as CSSProperties}
    aria-label={ariaLabel}
    onKeyDown={onKeyDown}
  >
    <div className="screen-pagination__viewport" ref={viewportRef} tabIndex={0} aria-label={`${ariaLabel}内容`}>
      {currentRange ? <div className="screen-pagination__items">
        {items.map((item, index) => {
          const key = keys[index] ?? token(getKey(item));
          return <div className="screen-pagination__item" data-item-key={key} key={key} hidden={pageByIndex[index] !== currentPage}>
            {renderItem(item, index)}
          </div>;
        })}
      </div> : renderEmpty?.()}
    </div>
    {pageCount > 1 && <nav className="screen-pagination__controls" aria-label={`${ariaLabel}分页`}>
      <button type="button" className="screen-pagination__step" onClick={() => goToPage(currentPage - 1)} disabled={currentPage === 0} aria-label="上一页">上一页</button>
      {pageNumbers(currentPage, pageCount).map((number, index) => number === 'ellipsis-start' || number === 'ellipsis-end'
        ? <span className="screen-pagination__ellipsis" aria-hidden="true" key={`${number}-${index}`}>…</span>
        : <button
            type="button"
            className="screen-pagination__number"
            key={number}
            onClick={() => goToPage(number)}
            aria-label={`第 ${number + 1} 页`}
            aria-current={currentPage === number ? 'page' : undefined}
          >{number + 1}</button>)}
      <button type="button" className="screen-pagination__step" onClick={() => goToPage(currentPage + 1)} disabled={currentPage === pageCount - 1} aria-label="下一页">下一页</button>
      <span className="screen-pagination__status" aria-live="polite">{currentPage + 1} / {pageCount}</span>
    </nav>}
  </section>;
}
