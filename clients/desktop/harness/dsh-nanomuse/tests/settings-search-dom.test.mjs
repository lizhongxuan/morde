import { after, before, test } from 'node:test'
import assert from 'node:assert/strict'
import { mkdtemp, rm } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'
import { build } from 'esbuild'

const here = fileURLToPath(new URL('.', import.meta.url))
let directory
let search

before(async () => {
  directory = await mkdtemp(join(tmpdir(), 'nm-settings-search-dom-'))
  const output = join(directory, 'search.mjs')
  await build({
    entryPoints: [join(here, '..', 'src', 'client', 'settings-search.ts')],
    outfile: output,
    bundle: true,
    platform: 'node',
    format: 'esm',
    logLevel: 'silent',
  })
  search = await import(pathToFileURL(output).href)
})

after(async () => {
  await rm(directory, { recursive: true, force: true })
})

function element(textContent, parentElement = null, excluded = false) {
  const classes = new Set()
  return {
    textContent,
    parentElement,
    classes,
    classList: { add: (name) => classes.add(name), remove: (name) => classes.delete(name) },
    closest: () => excluded ? {} : null,
  }
}

function withTextNodes(nodes, run) {
  const previousDocument = Object.getOwnPropertyDescriptor(globalThis, 'document')
  const previousNodeFilter = Object.getOwnPropertyDescriptor(globalThis, 'NodeFilter')
  Object.defineProperty(globalThis, 'NodeFilter', { configurable: true, value: { SHOW_TEXT: 4 } })
  Object.defineProperty(globalThis, 'document', {
    configurable: true,
    value: {
      createTreeWalker: () => {
        let index = 0
        return { nextNode: () => nodes[index++] ?? null }
      },
    },
  })
  try {
    run()
  } finally {
    if (previousDocument) Object.defineProperty(globalThis, 'document', previousDocument)
    else delete globalThis.document
    if (previousNodeFilter) Object.defineProperty(globalThis, 'NodeFilter', previousNodeFilter)
    else delete globalThis.NodeFilter
  }
}

test('a long matching paragraph is highlighted without marking the entire settings body', () => {
  const text = 'Language settings ' + 'explanation '.repeat(50)
  const root = element(text + 'Unrelated setting')
  const paragraph = element(text, root)
  const unrelated = element('Unrelated setting', root)
  root.querySelectorAll = () => [paragraph, unrelated].filter(child => child.classes.has(search.HIT_CLASS))
  withTextNodes([
    { textContent: text, parentElement: paragraph },
    { textContent: unrelated.textContent, parentElement: unrelated },
  ], () => {
    assert.deepEqual(search.markMatches(root, 'language'), [paragraph])
    assert.equal(paragraph.classes.has(search.HIT_CLASS), true)
    assert.equal(root.classes.has(search.HIT_CLASS), false)
    assert.equal(unrelated.classes.has(search.HIT_CLASS), false)
    assert.deepEqual(search.markMatches(root, 'unrelated'), [unrelated])
    assert.equal(paragraph.classes.has(search.HIT_CLASS), false)
    assert.deepEqual(search.markMatches(root, ''), [])
    assert.equal(unrelated.classes.has(search.HIT_CLASS), false)
  })
})

test('matching text nodes on one element produce one hit and excluded controls stay unmarked', () => {
  const root = element('Language language language')
  const paragraph = element('Language language', root)
  const control = element('language', root, true)
  root.querySelectorAll = () => [paragraph, control].filter(child => child.classes.has(search.HIT_CLASS))
  withTextNodes([
    { textContent: 'Language', parentElement: paragraph },
    { textContent: 'language', parentElement: paragraph },
    { textContent: 'language', parentElement: control },
  ], () => {
    assert.deepEqual(search.markMatches(root, 'language'), [paragraph])
    assert.equal(control.classes.has(search.HIT_CLASS), false)
  })
})
