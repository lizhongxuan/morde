/**
 * What a model picker shows of a long list (the Models contract, the same on the desktop
 * and the phones): each provider group collapsed to a few rows, in a fixed order, with
 * *Show {n} more* at its foot; a search over every group once the lists are long. Pure
 * functions; `components/ModelPicker.tsx` draws what `layoutGroups` returns, and
 * `model-list.test.ts` checks them.
 */

/** How many rows a group shows before it folds, and how many rows in all before the search field appears. */
export const COLLAPSE_AT = 8;

export interface ModelRow {
  id: string;
  name: string;
  /** The relay's recommended one for that lane (marked in the picker; first in its group). */
  recommended?: boolean;
}

export interface ModelGroup<R extends ModelRow = ModelRow> {
  /** Stable key: the provider's id, or the list's name on a single-provider picker. */
  key: string;
  label: string;
  rows: R[];
  /** The provider's catalogue default for this slot (`defaults.<slot>`); shown first when the group lists it. */
  default?: string;
}

export interface ShownGroup<R extends ModelRow = ModelRow> {
  key: string;
  label: string;
  rows: R[];
  /** Rows the collapsed view holds back behind *Show {n} more*; 0 when every row is shown. */
  more: number;
}

/** The model the picker holds right now: which group it sits in and its id. */
export interface Current {
  group: string;
  id: string;
}

/** Case-insensitive substring on the id and the display name. */
export function matchesQuery(row: ModelRow, query: string): boolean {
  const q = query.trim().toLowerCase();
  if (!q) return true;
  return row.id.toLowerCase().includes(q) || row.name.toLowerCase().includes(q);
}

/**
 * A group's rows in the order the picker shows them: the group's default for the slot (or,
 * without one, the recommended row), then the current choice when it sits in this group,
 * then the rest as the list came.
 */
export function orderRows<R extends ModelRow>(group: ModelGroup<R>, current?: Current): R[] {
  const first = group.default && group.rows.some((r) => r.id === group.default) ? group.default : group.rows.find((r) => r.recommended)?.id;
  const chosen = current && current.group === group.key ? current.id : undefined;
  const head: R[] = [];
  for (const id of [first, chosen]) {
    if (!id || head.some((r) => r.id === id)) continue;
    const row = group.rows.find((r) => r.id === id);
    if (row) head.push(row);
  }
  return [...head, ...group.rows.filter((r) => !head.includes(r))];
}

/** Every model row the picker has, across its groups. */
export function rowCount(groups: readonly ModelGroup[]): number {
  return groups.reduce((n, g) => n + g.rows.length, 0);
}

/** Whether the panel gets a search field: more rows in all than one group shows unfolded. */
export function hasSearch(groups: readonly ModelGroup[]): boolean {
  return rowCount(groups) > COLLAPSE_AT;
}

/**
 * The groups as the panel draws them. With a query: every group's matches, no cap, groups
 * without a match left out (an empty result means *No model matches*). Without: each group
 * in `orderRows` order, folded to `COLLAPSE_AT` rows unless it is in `expanded`, with the
 * number held back in `more`.
 */
export function layoutGroups<R extends ModelRow>(
  groups: readonly ModelGroup<R>[],
  { current, query = "", expanded }: { current?: Current; query?: string; expanded?: ReadonlySet<string> } = {},
): ShownGroup<R>[] {
  const searching = query.trim().length > 0;
  const out: ShownGroup<R>[] = [];
  for (const group of groups) {
    const rows = orderRows(group, current);
    if (searching) {
      const hits = rows.filter((r) => matchesQuery(r, query));
      if (hits.length) out.push({ key: group.key, label: group.label, rows: hits, more: 0 });
      continue;
    }
    if (rows.length > COLLAPSE_AT && !expanded?.has(group.key)) out.push({ key: group.key, label: group.label, rows: rows.slice(0, COLLAPSE_AT), more: rows.length - COLLAPSE_AT });
    else out.push({ key: group.key, label: group.label, rows, more: 0 });
  }
  return out;
}
