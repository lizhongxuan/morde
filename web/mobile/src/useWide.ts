import { useEffect, useState } from "react";

const QUERY = "(min-width: 64rem)";

function isWide(): boolean {
  if (typeof window === "undefined") return false;
  if (document.documentElement.classList.contains("desktop")) return true;
  return typeof window.matchMedia === "function" && window.matchMedia(QUERY).matches;
}

/**
 * The `wide:` layout of index.css, from JavaScript: a browser from 1024px, the desktop app at
 * any width. Screens that take a different shape on the phone (one list of rows that opens
 * pages) and on a wide window (everything on one page) branch on it.
 */
export function useWide(): boolean {
  const [wide, setWide] = useState(isWide);
  useEffect(() => {
    if (typeof window.matchMedia !== "function") return;
    const mq = window.matchMedia(QUERY);
    const on = () => setWide(isWide());
    mq.addEventListener("change", on);
    return () => mq.removeEventListener("change", on);
  }, []);
  return wide;
}
