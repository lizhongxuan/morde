import assert from 'node:assert/strict';
import { test } from 'node:test';
import { createServer, request as httpRequest } from 'node:http';
import { mkdtempSync, mkdirSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { startLocalPortalServer } from '../out/morde-local-portal.js';

test('local Portal serves bundled routes and safely proxies synthetic API requests', async () => {
  const root = mkdtempSync(join(tmpdir(), 'morde-local-portal-'));
  mkdirSync(join(root, 'assets'));
  writeFileSync(join(root, 'index.html'), '<!doctype html><html><head></head><body><div id="root"></div></body></html>');
  writeFileSync(join(root, 'assets', 'main.js'), 'export {};');
  const upstreamRequests = [];
  const upstream = createServer((request, response) => {
    if (request.url === '/api/events') {
      response.writeHead(200, { 'content-type': 'text/event-stream', 'cache-control': 'no-cache' });
      response.flushHeaders();
      response.write('event: status\ndata: first\n\n');
      setTimeout(() => response.end('event: status\ndata: second\n\n'), 25);
      return;
    }
    const chunks = [];
    request.on('data', (chunk) => chunks.push(chunk));
    request.on('end', () => {
      upstreamRequests.push({ path: request.url, origin: request.headers.origin, referer: request.headers.referer, cookie: request.headers.cookie });
      const body = Buffer.concat(chunks).toString('utf8');
      response.writeHead(200, { 'content-type': 'application/json' });
      response.end(JSON.stringify({ path: request.url, body }));
    });
  });
  upstream.listen(0, '127.0.0.1');
  await new Promise((resolve) => upstream.once('listening', resolve));
  const address = upstream.address();
  const serviceOrigin = `http://127.0.0.1:${address.port}`;
  let portal;
  try {
    portal = await startLocalPortalServer(root, serviceOrigin, 'https://*.hosts.example.test');
    const stablePort = Number(new URL(portal.origin).port);
    const page = await fetch(`${portal.origin}/assistant`);
    assert.equal(page.status, 200);
    assert.match(page.headers.get('content-security-policy'), /frame-src 'self' https:\/\/\*\.hosts\.example\.test/);
    assert.match(await page.text(), /name="morde-service-origin" content="http:\/\/127\.0\.0\.1:/);
    assert.equal((await fetch(`${portal.origin}/assets/main.js`)).status, 200);
    assert.equal((await fetch(`${portal.origin}/admin/login`)).status, 404);
    assert.equal((await fetch(`${portal.origin}/app/hosts`)).status, 404);
    assert.equal((await fetch(`${portal.origin}/unlisted.js`)).status, 404);
    const invalidHostStatus = await new Promise((resolve, reject) => {
      const request = httpRequest(new URL(`${portal.origin}/api/test`), { headers: { host: 'attacker.example' } }, (response) => {
        response.resume();
        resolve(response.statusCode);
      });
      request.once('error', reject);
      request.end();
    });
    assert.equal(invalidHostStatus, 403);

    const response = await fetch(`${portal.origin}/api/test?take=1`, {
      method: 'POST',
      headers: { origin: portal.origin, referer: `${portal.origin}/assistant`, cookie: 'ch_portal=synthetic-session; unrelated=private', 'content-type': 'application/json' },
      body: JSON.stringify({ message: 'local fixture' }),
    });
    assert.equal(response.status, 200);
    assert.deepEqual(await response.json(), { path: '/api/test?take=1', body: '{"message":"local fixture"}' });
    assert.deepEqual(upstreamRequests.at(-1), {
      path: '/api/test?take=1', origin: serviceOrigin, referer: `${serviceOrigin}/assistant`, cookie: 'ch_portal=synthetic-session',
    });

    const stream = await fetch(`${portal.origin}/api/events`, { headers: { origin: portal.origin } });
    assert.equal(stream.headers.get('content-type'), 'text/event-stream');
    const reader = stream.body.getReader();
    const first = await reader.read();
    assert.match(new TextDecoder().decode(first.value), /data: first/);
    const second = await reader.read();
    assert.match(new TextDecoder().decode(second.value), /data: second/);

    const beforeRejectedOrigin = upstreamRequests.length;
    const rejected = await fetch(`${portal.origin}/api/test`, { method: 'POST', headers: { origin: 'https://attacker.example' }, body: '{}' });
    assert.equal(rejected.status, 403);
    assert.equal(upstreamRequests.length, beforeRejectedOrigin);
    portal.setHostFrameSource('https://*.updated-hosts.example.test');
    assert.match((await fetch(`${portal.origin}/settings`)).headers.get('content-security-policy'), /updated-hosts\.example\.test/);
    await portal.close();
    portal = await startLocalPortalServer(root, serviceOrigin, undefined, stablePort);
    assert.equal(new URL(portal.origin).port, String(stablePort));
  } finally {
    if (portal) await portal.close();
    await new Promise((resolve) => upstream.close(resolve));
    rmSync(root, { recursive: true, force: true });
  }
});
