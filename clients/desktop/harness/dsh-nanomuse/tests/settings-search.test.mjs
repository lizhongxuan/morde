// Finding a setting in the settings dialog: the words of a query, the pages whose
// title answers them, and the text cut around a match. Built from the TypeScript
// source with esbuild.
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
  dir = await mkdtemp(join(tmpdir(), 'nm-settings-search-'))
  await build({
    entryPoints: [join(here, '..', 'src', 'client', 'settings-search.ts')],
    bundle: true,
    format: 'esm',
    platform: 'node',
    outfile: join(dir, 'settings-search.mjs'),
    logLevel: 'silent',
  })
  m = await import(pathToFileURL(join(dir, 'settings-search.mjs')).href)
})

after(async () => {
  await rm(dir, { recursive: true, force: true })
})

test('a query is its words, lower case, the blanks dropped', () => {
  assert.deepEqual(m.queryWords('  Language  '), ['language'])
  assert.deepEqual(m.queryWords('Hot Key'), ['hot', 'key'])
  assert.deepEqual(m.queryWords('   '), [])
})

test('a text answers a query when it holds every word of it', () => {
  assert.equal(m.matches('Language', 'language'), true)
  assert.equal(m.matches('Language', 'LANGUAGE'), true)
  assert.equal(m.matches('Language', 'lang'), true)
  assert.equal(m.matches('Language', 'appearance'), false)
  // every word has to be there, not one of them
  assert.equal(m.matches('Quick chat', 'chat quick'), true)
  assert.equal(m.matches('Quick chat', 'chat voice'), false)
  // an empty query answers everything
  assert.equal(m.matches('Language', ''), true)
  assert.equal(m.matches('Language', '  '), true)
})

test('the text is cut around the words that matched, and the pieces make the text again', () => {
  const parts = m.splitAround('Language and Appearance', 'language')
  assert.deepEqual(parts, [
    { text: 'Language', hit: true },
    { text: ' and Appearance', hit: false },
  ])
  assert.equal(parts.map((p) => p.text).join(''), 'Language and Appearance')

  // every word is marked where it stands
  const two = m.splitAround('hot key', 'hot key')
  assert.deepEqual(two, [
    { text: 'hot', hit: true },
    { text: ' ', hit: false },
    { text: 'key', hit: true },
  ])

  // nothing matched, the text stands as it was
  assert.deepEqual(m.splitAround('Language', 'appearance'), [{ text: 'Language', hit: false }])
  assert.deepEqual(m.splitAround('', 'language'), [])
})

test('the pages whose title answers the query keep the order they came in', () => {
  const pages = [
    { id: 'general', label: 'General' },
    { id: 'models', label: 'Models' },
    { id: 'language', label: 'Language' },
    { id: 'advanced', label: 'Advanced' },
  ]
  assert.deepEqual(m.filterPages(pages, 'model').map((p) => p.id), ['models'])
  assert.deepEqual(m.filterPages(pages, 'lang').map((p) => p.id), ['language'])
  assert.deepEqual(m.filterPages(pages, 'an').map((p) => p.id), ['language', 'advanced'])
  // an empty query keeps them all
  assert.deepEqual(m.filterPages(pages, '').map((p) => p.id), ['general', 'models', 'language', 'advanced'])
})

test('the words of a hit are short enough to sit in a list', () => {
  assert.equal(m.hitLabel({ textContent: '  Language \n  row ' }), 'Language row')
  const long = 'x'.repeat(200)
  assert.equal(m.hitLabel({ textContent: long }).length, 80)
  assert.ok(m.hitLabel({ textContent: long }).endsWith('…'))
})
