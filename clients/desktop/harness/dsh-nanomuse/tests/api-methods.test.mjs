// The window's `call` and `roomsCall` send GET when they are given no body and POST when they
// are (api.ts, rooms.ts). The host serves its routes by method, so a button that calls a
// POST-only route without a body never reaches it: the host answers 404 and the click does
// nothing. This cross-checks every body-less call in the client against the routes the host
// serves to GET, for both the cloud API (`src/cloud.ts`) and the rooms API (`src/rooms.ts`).
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readdirSync, readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'

const src = (rel) => readFileSync(fileURLToPath(new URL(`../src/${rel}`, import.meta.url)), 'utf8').replace(/\r\n/g, '\n')
const clientDir = fileURLToPath(new URL('../src/client/', import.meta.url))
const clientFiles = readdirSync(clientDir).filter((f) => /\.(ts|tsx)$/.test(f))

/**
 * The routes a host file serves to GET: `req.method === 'GET' && route === '/x'`, and the
 * routes matched on a line that does not insist on POST (`route === '/x' || route.startsWith('/x/')`).
 * Each comes back as the path without its leading slash; a `startsWith` prefix ends in `/`.
 */
function getRoutes(text) {
  const out = new Set()
  for (const line of text.split('\n')) {
    if (/req\.method\s*===\s*'POST'/.test(line) || /req\.method\s*!==\s*'GET'/.test(line)) continue
    for (const m of line.matchAll(/route\s*===\s*'\/([^']*)'/g)) out.add(m[1])
    for (const m of line.matchAll(/route\.startsWith\('\/([^']*)'\)/g)) out.add(m[1])
  }
  return out
}

/** Every call of `fn` in the client with a literal or template path and no body: `{ file, path }`. */
function bodylessCalls(fn) {
  const calls = []
  const re = new RegExp(`\\b${fn}(?:<[^>]*>)?\\(\\s*(?:'([^']*)'|\`([^\`]*)\`)\\s*\\)`, 'g')
  for (const file of clientFiles) {
    const text = readFileSync(clientDir + file, 'utf8')
    for (const m of text.matchAll(re)) {
      const path = m[1] ?? m[2].split('${')[0]
      calls.push({ file, path: path.split('?')[0] })
    }
  }
  return calls
}

function servedToGet(routes, path) {
  if (routes.has(path)) return true
  for (const r of routes) if (r.endsWith('/') && (path + '/').startsWith(r)) return true
  return false
}

test('every body-less `call` in the window targets a route the cloud API serves to GET', () => {
  const routes = getRoutes(src('cloud.ts'))
  assert.ok(routes.has('status') && routes.has('me'), 'the GET routes were found')
  const calls = bodylessCalls('call')
  assert.ok(calls.length >= 10, `the body-less calls were found (${calls.length})`)
  for (const { file, path } of calls) {
    assert.ok(servedToGet(routes, path), `${file}: call('${path}') has no body, so it is a GET, but the host serves /${path} to POST only; pass {} to post it`)
  }
})

test('every body-less `roomsCall` in the window targets a route the rooms API serves to GET', () => {
  const routes = getRoutes(src('rooms.ts'))
  assert.ok(routes.has('files') && routes.has('connectors'), 'the GET routes were found')
  const calls = bodylessCalls('roomsCall')
  assert.ok(calls.length >= 2, `the body-less calls were found (${calls.length})`)
  for (const { file, path } of calls) {
    assert.ok(servedToGet(routes, path), `${file}: roomsCall('${path}') has no body, so it is a GET, but the rooms API serves /${path} to POST only; pass {} to post it`)
  }
})
