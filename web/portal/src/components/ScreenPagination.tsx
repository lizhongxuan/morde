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
  const previousKeySignature = useRef<string | null>(null);
  const previousRangeSignature = useRef<string | null>(null);
  const [measuredHeights, setMeasuredHeights] = useState<ReadonlyMap<string, number>>(() => new Map());
  const [viewportHeight, setViewportHeight] = useState(0);
  const [overflowItemKeys, setOverflowItemKeys] = useState<ReadonlySet<string>>(() => new Set());
  const [page, setPage] = useState(0);

  if (items.length === 0 && page !== 0) setPage(0);

  const keys = useMemo(() => items.map((item) => token(getKey(item))), [items, getKey]);
  const keySignature = JSON.stringify(keys);
  const ranges = useMemo<PageRange[]>(() => {
    if (!items.length) return [];
    const available = Math.max(1, viewportHeight || (typeof height === 'number' ? height : 0));
    const result: PageRange[] = [];
    let start = 0;
    let used = 0;
    for (let index = 0; index < items.length; index += 1) {
      const itemHeight = measuredHeights.get(keys[index] ?? '') ?? Math.max(1, estimatedItemHeight);
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
  }, [items.length, keys, estimatedItemHeight, gap, height, viewportHeight, measuredHeights]);
  const rangeSignature = ranges.map(({ start, end }) => `${start}:${end}`).join('|');
  const pageByIndex = useMemo(() => {
    const result = Array<number>(items.length).fill(0);
    ranges.forEach(({ start, end }, pageIndex) => {
      for (let index = start; index < end; index += 1) result[index] = pageIndex;
    });
    return result;
  }, [items.length, ranges]);
  const pageCount = ranges.length;
  const currentPage = Math.min(page, Math.max(0, pageCount - 1));
  const currentRange = ranges[currentPage];

  useLayoutEffect(() => {
    const keysChanged = previousKeySignature.current !== keySignature;
    const rangesChanged = previousRangeSignature.current !== rangeSignature;
    if (!keysChanged && !rangesChanged) return;

    const oldKeys = previousKeys.current;
    previousKeys.current = keys;
    previousKeySignature.current = keySignature;
    previousRangeSignature.current = rangeSignature;
    if (!keys.length) {
      anchorToken.current = null;
      return;
    }

    if (!anchorToken.current) {
      anchorToken.current = keys[ranges[currentPage]?.start ?? 0] ?? null;
      return;
    }

    const index = keys.indexOf(anchorToken.current);
    if (index >= 0) {
      const nextPage = findPage(ranges, index);
      if (nextPage !== currentPage) setPage(nextPage);
    } else {
      const oldAnchorIndex = oldKeys.indexOf(anchorToken.current);
      const fallbackIndex = Math.min(Math.max(oldAnchorIndex >= 0 ? oldAnchorIndex : currentRange?.start ?? 0, 0), keys.length - 1);
      const nextPage = findPage(ranges, fallbackIndex);
      if (nextPage !== currentPage) setPage(nextPage);
      anchorToken.current = keys[ranges[nextPage]?.start ?? fallbackIndex] ?? null;
    }
  }, [keySignature, keys, ranges, rangeSignature, currentPage, currentRange, pageCount]);

  useLayoutEffect(() => {
    const viewport = viewportRef.current;
    if (!viewport) return;
    let active = true;
    const updateViewport = () => {
      if (!active) return;
      const nextHeight = viewport.clientHeight;
      setViewportHeight((current) => Math.abs(current - nextHeight) < 1 ? current : nextHeight);
      const availableHeight = nextHeight || (typeof height === 'number' ? height : viewportHeight);
      const nextOverflowKeys = new Set<string>();
      let heightsChanged = false;
      const nextMeasuredHeights = new Map(measuredHeights);
      for (const element of viewport.querySelectorAll<HTMLElement>('[data-item-key]')) {
        const itemToken = element.dataset.itemKey;
        if (!itemToken) continue;
        const nextItemHeight = Math.max(element.getBoundingClientRect().height, element.scrollHeight);
        if (availableHeight > 0 && nextItemHeight > availableHeight + 1) nextOverflowKeys.add(itemToken);
        const measuredHeight = availableHeight > 0 ? Math.min(nextItemHeight, availableHeight) : nextItemHeight;
        if (measuredHeight > 0 && Math.abs((nextMeasuredHeights.get(itemToken) ?? 0) - measuredHeight) > 1) {
          nextMeasuredHeights.set(itemToken, measuredHeight);
          heightsChanged = true;
        }
      }
      setOverflowItemKeys((current) => current.size === nextOverflowKeys.size
        && [...current].every((itemToken) => nextOverflowKeys.has(itemToken)) ? current : nextOverflowKeys);
      if (heightsChanged) setMeasuredHeights(nextMeasuredHeights);
    };
    queueMicrotask(updateViewport);
    if (typeof ResizeObserver === 'undefined') return () => { active = false; };
    const observer = new ResizeObserver(() => updateViewport());
    observer.observe(viewport);
    viewport.querySelectorAll('[data-item-key]').forEach((element) => observer.observe(element));
    return () => { active = false; observer.disconnect(); };
  }, [currentPage, rangeSignature, keySignature, measuredHeights, height, viewportHeight]);

  function goToPage(nextPage: number) {
    const boundedPage = Math.max(0, Math.min(pageCount - 1, nextPage));
    setPage(boundedPage);
    anchorToken.current = keys[ranges[boundedPage]?.start ?? 0] ?? null;
  }

  function onKeyDown(event: KeyboardEvent<HTMLElement>) {
    const target = event.target;
    if (!(target instanceof HTMLElement)) return;
    if (target.closest('input, textarea, select, button, a, [contenteditable="true"]')) return;
    const scrollRegion = target.closest<HTMLElement>('[data-screen-pagination-scroll]');
    if (scrollRegion) {
      const maxScrollTop = Math.max(0, scrollRegion.scrollHeight - scrollRegion.clientHeight);
      const pageStep = Math.max(40, Math.floor(scrollRegion.clientHeight * 0.9));
      const nextScrollTop = event.key === 'ArrowDown' ? scrollRegion.scrollTop + 40
        : event.key === 'ArrowUp' ? scrollRegion.scrollTop - 40
          : event.key === 'PageDown' || event.key === ' ' || event.key === 'Spacebar' ? scrollRegion.scrollTop + pageStep
            : event.key === 'PageUp' ? scrollRegion.scrollTop - pageStep
              : event.key === 'Home' ? 0
                : event.key === 'End' ? maxScrollTop : null;
      if (nextScrollTop !== null) {
        event.preventDefault();
        scrollRegion.scrollTop = Math.min(maxScrollTop, Math.max(0, nextScrollTop));
      }
      return;
    }
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
          const overflowed = overflowItemKeys.has(key);
          return <div className={`screen-pagination__item${overflowed ? ' screen-pagination__item--overflow' : ''}`}
            data-item-key={key} data-screen-pagination-scroll={overflowed ? '' : undefined} key={key}
            hidden={pageByIndex[index] !== currentPage} tabIndex={overflowed ? 0 : undefined}
            role={overflowed ? 'region' : undefined}
            aria-label={overflowed ? `${ariaLabel}条目内容，可在此区域内滚动查看` : undefined}
            style={overflowed ? { maxHeight: `${viewportHeight || (typeof height === 'number' ? height : 0)}px` } : undefined}>
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
