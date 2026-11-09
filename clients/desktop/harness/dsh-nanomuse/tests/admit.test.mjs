/**
 * The preset's HTTP routes are admitted like the harness's own `/api`: the
 * `connection` service's word (Host/Origin fence, browser session) is asked
 * before the route's handler runs, and its status is what a stranger gets.
 */
import assert from 'node:assert/strict'
import { test } from 'node:test'
import { mountGuarded } from '../lib/admit.js'

/** A cordis-shaped context with a web server that keeps what is registered and a connection with a scripted answer. */
function fakeContext(rejection) {
  const routes = []
  const ctx = {
    webServer: { register: (route) => routes.push(route) },
    connection: { requestRejection: () => rejection },
    inject(_names, fn) {
      fn(ctx)
    },
    effect(fn) {
      fn()
    },
  }
  return { ctx, routes }
}

function fakeResponse() {
  const res = { status: 0, headers: undefined, body: undefined }
  res.writeHead = (status, headers) => {
    res.status = status
    res.headers = headers
    return res
  }
  res.end = (body) => {
    res.body = body
    return res
  }
  return res
}

test('an admitted request reaches the handler', async () => {
  const { ctx, routes } = fakeContext(undefined)
  let seen = 0
  mountGuarded(ctx, '/nanomuse/x', (_req, res) => {
    seen++
    res.writeHead(200).end('ok')
  }, 'test')
  assert.equal(routes.length, 1)
  assert.deepEqual([routes[0].kind, routes[0].path], ['prefix', '/nanomuse/x'])
  const res = fakeResponse()
  await routes[0].handler({ headers: {} }, res)
  assert.equal(seen, 1)
  assert.equal(res.status, 200)
})

test('a request without the browser session is 401 before the handler', async () => {
  const { ctx, routes } = fakeContext(401)
  let seen = 0
  mountGuarded(ctx, '/nanomuse/x', () => void seen++, 'test')
  const res = fakeResponse()
  await routes[0].handler({ headers: {} }, res)
  assert.equal(seen, 0)
  assert.equal(res.status, 401)
  assert.equal(res.body, 'unauthorized')
})

test('a request from another origin is 403', async () => {
  const { ctx, routes } = fakeContext(403)
  mountGuarded(ctx, '/nanomuse/x', () => assert.fail('handler ran'), 'test')
  const res = fakeResponse()
  await routes[0].handler({ headers: { origin: 'http://evil.example' } }, res)
  assert.equal(res.status, 403)
  assert.equal(res.body, 'forbidden')
})
