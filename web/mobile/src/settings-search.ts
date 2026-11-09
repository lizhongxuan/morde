/**
 * Finding a setting in the web console's settings.
 *
 * Two things answer a query: the titles of the sections in the nav, and the words
 * in the sections themselves. The rule for both lives here, apart from the React,
 * so a test can drive it; marking the words that matched is the same rule seen
 * from the other side. Ported from the desktop app's settings search, which
 * shipped first; the two copies state the same rule for their own screens.
 */

/** The words of a query, lower case, the blanks dropped. */
export function queryWords(query: string): string[] {
  return query
    .toLowerCase()
    .split(/\s+/)
    .filter((word) => word.length > 0);
}

/** Whether a text answers every word of a query. An empty query answers everything. */
export function matches(text: string, query: string): boolean {
  const words = queryWords(query);
  if (words.length === 0) return true;
  const haystack = text.toLowerCase();
  return words.every((word) => haystack.includes(word));
}

/** The text cut around the query's words; each piece says whether it is a match. */
export function splitAround(text: string, query: string): { text: string; hit: boolean }[] {
  const words = queryWords(query);
  if (text.length === 0) return [];
  if (words.length === 0) return [{ text, hit: false }];
  const marks = new Array<boolean>(text.length).fill(false);
  const haystack = text.toLowerCase();
  for (const word of words) {
    let from = 0;
    for (;;) {
      const at = haystack.indexOf(word, from);
      if (at < 0) break;
      for (let i = at; i < at + word.length; i += 1) marks[i] = true;
      from = at + word.length;
    }
  }
  const parts: { text: string; hit: boolean }[] = [];
  let start = 0;
  for (let i = 1; i <= text.length; i += 1) {
    if (i < text.length && marks[i] === marks[start]) continue;
    parts.push({ text: text.slice(start, i), hit: marks[start] === true });
    start = i;
  }
  return parts;
}

/** The pages whose title answers the query, in the order they came in. */
export function filterPages<T extends { label: string }>(pages: readonly T[], query: string): T[] {
  if (queryWords(query).length === 0) return [...pages];
  return pages.filter((page) => matches(page.label, query));
}

/** The class a matched word carries while the query stands. */
export const HIT_CLASS = "nm-settings-hit";

/** The words of a hit, short enough to sit in a list. */
export function hitLabel(element: Element): string {
  const text = (element.textContent ?? "").replace(/\s+/g, " ").trim();
  return text.length > 80 ? `${text.slice(0, 79)}…` : text;
}

/**
 * Mark the query's words in the text of an element and report where they were.
 *
 * The mark goes on the element the word sits in, not on the text node itself, so
 * the React tree underneath is left as it was and the page keeps working.
 */
export function markMatches(root: HTMLElement, query: string): HTMLElement[] {
  clearMarks(root);
  if (queryWords(query).length === 0) return [];
  const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
  const nodes: Text[] = [];
  for (let node = walker.nextNode(); node !== null; node = walker.nextNode()) {
    const text = node.textContent ?? "";
    if (text.trim() === "") continue;
    const parent = node.parentElement;
    if (parent === null) continue;
    if (parent.closest("script, style, input, select, textarea") !== null) continue;
    nodes.push(node as Text);
  }
  const hits: HTMLElement[] = [];
  const seen = new Set<HTMLElement>();
  for (const node of nodes) {
    if (!splitAround(node.textContent ?? "", query).some((part) => part.hit)) continue;
    const parent = node.parentElement;
    if (parent === null) continue;
    const target: HTMLElement = parent;
    if (seen.has(target)) continue;
    seen.add(target);
    target.classList.add(HIT_CLASS);
    hits.push(target);
  }
  return hits;
}

/** Take the marks off again. */
export function clearMarks(root: HTMLElement): void {
  root.classList.remove(HIT_CLASS);
  for (const element of Array.from(root.querySelectorAll(`.${HIT_CLASS}`))) element.classList.remove(HIT_CLASS);
}
