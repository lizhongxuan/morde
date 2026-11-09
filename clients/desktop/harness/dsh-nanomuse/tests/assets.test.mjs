// The package's `assets/` must be found from wherever the build put the code: `lib/index.js`
// at the top of lib/, and the shared chunks under `lib/chunks/` that rooms.ts lands in.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { existsSync, readFileSync, readdirSync } from 'node:fs'
import { join, sep } from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'
import { packageAssetsDir } from '../lib/profile.js'
import { ASSETS_DIR } from '../lib/rooms.js'

const root = fileURLToPath(new URL('../', import.meta.url))
const expected = join(root, 'assets')

test('the built rooms module resolves the ideas list from the package root', () => {
  assert.equal(ASSETS_DIR.replace(/[\\/]+$/, ''), expected)
  const ideas = JSON.parse(readFileSync(join(ASSETS_DIR, 'ideas.en.json'), 'utf8'))
  assert.ok(Array.isArray(ideas) || typeof ideas === 'object')
  assert.ok(existsSync(join(ASSETS_DIR, 'ideas.zh.json')))
})

test('the same answer from lib/index.js, from a chunk, and from the sources', () => {
  const chunks = readdirSync(join(root, 'lib', 'chunks')).filter((f) => f.endsWith('.js'))
  assert.ok(chunks.length > 0, 'the build splits shared modules into lib/chunks/')
  for (const file of [join(root, 'lib', 'index.js'), join(root, 'lib', 'chunks', chunks[0]), join(root, 'src', 'rooms.ts')]) {
    assert.equal(packageAssetsDir(pathToFileURL(file).href), expected, file)
  }
})

test('outside the package the old relative guess is the fallback', () => {
  const elsewhere = pathToFileURL(join(root, '..', 'nowhere', 'lib', 'x.js')).href
  const dir = packageAssetsDir(elsewhere)
  assert.ok(dir.endsWith(`${sep}nowhere${sep}assets`), dir)
})
