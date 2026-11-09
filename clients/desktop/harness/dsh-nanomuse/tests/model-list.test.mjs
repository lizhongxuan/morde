// What a model picker shows of a long list (src/client/model-list.ts): each provider group
// folded to eight rows in a fixed order (the catalogue default, the current choice, the
// rest as listed) with the count held back, the Cloud group's recommended model first, the
// search field once the lists are long, and a query showing every match across groups
// with no cap. Pure functions, built from the TypeScript source with esbuild.
import { test, before, after } from 'node:test'
import assert from 'node:assert/strict'
import { mkdtemp, rm } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { pathToFileURL, fileURLToPath } from 'node:url'
import { build } from 'esbuild'

const here = fileURLToPath(new URL('.', import.meta.url))
let dir
let m

before(async () => {
  dir = await mkdtemp(join(tmpdir(), 'nm-model-list-'))
  await build({
    entryPoints: [join(here, '..', 'src', 'client', 'model-list.ts')],
    bundle: true,
    format: 'esm',
    platform: 'node',
    outfile: join(dir, 'model-list.mjs'),
    logLevel: 'silent',
  })
  m = await import(pathToFileURL(join(dir, 'model-list.mjs')).href)
})

after(async () => {
  await rm(dir, { recursive: true, force: true })
})

const rows = (prefix, n) => Array.from({ length: n }, (_, i) => ({ id: `${prefix}-${i + 1}`, name: `${prefix} ${i + 1}` }))
const ids = (list) => list.map((r) => r.id)

test('a short group is shown whole, in the order it came', () => {
  const group = { key: 'zhipu', label: 'Zhipu GLM', rows: rows('glm', 3) }
  const [shown] = m.layoutGroups([group])
  assert.deepEqual(ids(shown.rows), ['glm-1', 'glm-2', 'glm-3'])
  assert.equal(shown.more, 0)
  assert.equal(m.hasSearch([group]), false)
})

test('exactly eight rows need no folding; nine do', () => {
  const eight = { key: 'a', label: 'A', rows: rows('a', 8) }
  assert.equal(m.layoutGroups([eight])[0].more, 0)
  assert.equal(m.layoutGroups([eight])[0].rows.length, 8)
  const nine = { key: 'b', label: 'B', rows: rows('b', 9) }
  const [shown] = m.layoutGroups([nine])
  assert.equal(shown.rows.length, 8)
  assert.equal(shown.more, 1)
  assert.equal(m.COLLAPSE_AT, 8)
})

test('a long group folds to eight: the catalogue default first, the current choice second, the rest as listed', () => {
  const group = { key: 'openrouter', label: 'OpenRouter', rows: rows('or', 40), default: 'or-30' }
  const [shown] = m.layoutGroups([group], { current: { group: 'openrouter', id: 'or-17' } })
  assert.deepEqual(ids(shown.rows), ['or-30', 'or-17', 'or-1', 'or-2', 'or-3', 'or-4', 'or-5', 'or-6'])
  assert.equal(shown.more, 32)
  // the current choice in another group does not move this one
  const [other] = m.layoutGroups([group], { current: { group: 'siliconflow', id: 'or-17' } })
  assert.deepEqual(ids(other.rows), ['or-30', 'or-1', 'or-2', 'or-3', 'or-4', 'or-5', 'or-6', 'or-7'])
})

test('a default the provider does not list is skipped; a current that is the default is not shown twice', () => {
  const group = { key: 'p', label: 'P', rows: rows('p', 10), default: 'p-none' }
  assert.deepEqual(ids(m.orderRows(group)).slice(0, 3), ['p-1', 'p-2', 'p-3'])
  const same = { key: 'p', label: 'P', rows: rows('p', 10), default: 'p-4' }
  const ordered = m.orderRows(same, { group: 'p', id: 'p-4' })
  assert.deepEqual(ids(ordered).slice(0, 3), ['p-4', 'p-1', 'p-2'])
  assert.equal(ordered.length, 10)
})

test('the Cloud group keeps the recommended model first, the current choice after it', () => {
  const cloud = { key: 'nanomuse', label: 'nanoMuse Cloud', rows: [{ id: 'q-rec', name: 'Qwen', recommended: true }, ...rows('q', 12)] }
  const [shown] = m.layoutGroups([cloud], { current: { group: 'nanomuse', id: 'q-9' } })
  assert.deepEqual(ids(shown.rows).slice(0, 3), ['q-rec', 'q-9', 'q-1'])
  assert.equal(shown.more, 5)
})

test('Show more unfolds that group only, for as long as it is expanded', () => {
  const groups = [
    { key: 'a', label: 'A', rows: rows('a', 12) },
    { key: 'b', label: 'B', rows: rows('b', 12) },
  ]
  const folded = m.layoutGroups(groups)
  assert.deepEqual(folded.map((g) => [g.rows.length, g.more]), [[8, 4], [8, 4]])
  const open = m.layoutGroups(groups, { expanded: new Set(['b']) })
  assert.deepEqual(open.map((g) => [g.rows.length, g.more]), [[8, 4], [12, 0]])
})

test('the search field appears once the rows across all groups pass eight', () => {
  assert.equal(m.hasSearch([{ key: 'a', label: 'A', rows: rows('a', 4) }, { key: 'b', label: 'B', rows: rows('b', 4) }]), false)
  assert.equal(m.hasSearch([{ key: 'a', label: 'A', rows: rows('a', 4) }, { key: 'b', label: 'B', rows: rows('b', 5) }]), true)
  assert.equal(m.rowCount([{ key: 'a', label: 'A', rows: rows('a', 4) }, { key: 'b', label: 'B', rows: rows('b', 5) }]), 9)
})

test('a query matches the id or the name, case-insensitively, as a substring', () => {
  assert.equal(m.matchesQuery({ id: 'qwen3-vl-plus', name: 'Qwen3 VL Plus' }, 'VL'), true)
  assert.equal(m.matchesQuery({ id: 'qwen3-vl-plus', name: 'Qwen3 VL Plus' }, 'plus'), true)
  assert.equal(m.matchesQuery({ id: 'glm-4.6v', name: 'GLM 4.6V (vision)' }, 'VISION'), true)
  assert.equal(m.matchesQuery({ id: 'glm-4.6v', name: 'GLM' }, 'gpt'), false)
  assert.equal(m.matchesQuery({ id: 'x', name: 'y' }, '   '), true)
})

test('while a query is present every group shows all its matches, groups without one are hidden', () => {
  const groups = [
    { key: 'cloud', label: 'nanoMuse Cloud', rows: [{ id: 'qwen-vl', name: 'Qwen VL', recommended: true }, { id: 'glm-4.6v', name: 'GLM 4.6V' }] },
    { key: 'openrouter', label: 'OpenRouter', rows: [...rows('qwen', 20), ...rows('gpt', 5)] },
    { key: 'zhipu', label: 'Zhipu GLM', rows: rows('glm', 3) },
  ]
  const hits = m.layoutGroups(groups, { query: 'qwen' })
  assert.deepEqual(hits.map((g) => [g.key, g.rows.length, g.more]), [['cloud', 1, 0], ['openrouter', 20, 0]])
  const gpt = m.layoutGroups(groups, { query: ' GPT ' })
  assert.deepEqual(gpt.map((g) => [g.key, g.rows.length]), [['openrouter', 5]])
  // nothing matches: no groups at all (the panel says so in one line)
  assert.deepEqual(m.layoutGroups(groups, { query: 'claude' }), [])
  // clearing the query brings the folded view back
  const back = m.layoutGroups(groups, { query: '' })
  assert.deepEqual(back.map((g) => [g.key, g.rows.length, g.more]), [['cloud', 2, 0], ['openrouter', 8, 17], ['zhipu', 3, 0]])
})
