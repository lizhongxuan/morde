import { useEffect, useRef, type RefObject } from "react";

const ITEMS = '[role="menuitem"]:not([disabled])';

/**
 * The keyboard of a small dropdown menu (`role="menu"` under its button), the way native
 * menus behave: while it is open the first entry takes focus, ArrowDown and ArrowUp walk
 * the entries and wrap, Home and End jump to the ends, Escape closes and a pointer down
 * outside closes. When the menu closes, focus goes back to the button that opened it,
 * unless it was Tab that closed it, which leaves focus where Tab put it. `ref` is the
 * element that holds both the button and the menu.
 */
export function useMenuKeys(ref: RefObject<HTMLElement | null>, open: boolean, close: () => void): void {
  // the latest close without re-running the main effect (and re-focusing) on every render
  const closeRef = useRef(close);
  useEffect(() => {
    closeRef.current = close;
  }, [close]);
  useEffect(() => {
    if (!open) return;
    const root = ref.current;
    if (!root) return;
    const before = document.activeElement as HTMLElement | null;
    let returnFocus = true;
    const items = () => Array.from(root.querySelectorAll<HTMLElement>(ITEMS));
    items()[0]?.focus();
    const onDown = (e: PointerEvent) => {
      if (!root.contains(e.target as Node)) closeRef.current();
    };
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        e.preventDefault();
        closeRef.current();
        return;
      }
      if (e.key === "Tab") {
        returnFocus = false;
        closeRef.current();
        return;
      }
      const list = items();
      if (list.length === 0) return;
      const at = list.indexOf(document.activeElement as HTMLElement);
      let next = -1;
      if (e.key === "ArrowDown") next = at < 0 ? 0 : (at + 1) % list.length;
      else if (e.key === "ArrowUp") next = at < 0 ? list.length - 1 : (at - 1 + list.length) % list.length;
      else if (e.key === "Home") next = 0;
      else if (e.key === "End") next = list.length - 1;
      if (next < 0) return;
      e.preventDefault();
      list[next].focus();
    };
    window.addEventListener("pointerdown", onDown);
    window.addEventListener("keydown", onKey);
    return () => {
      window.removeEventListener("pointerdown", onDown);
      window.removeEventListener("keydown", onKey);
      if (returnFocus && before && before.isConnected && typeof before.focus === "function") before.focus();
    };
  }, [ref, open]);
}
