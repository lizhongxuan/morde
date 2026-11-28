import assert from 'node:assert/strict';
import { test } from 'node:test';
import { exchangeHostEmbedTicket } from '../out/morde-embed-auth.js';

const hostOrigin = 'https://h1234567.hosts.example.test';
const portalOrigin = 'https://portal.example.test';
const nonce = 'B'.repeat(32);
const ticket = 'A'.repeat(43);
const location = `${hostOrigin}/__ch/wb/?embed=1&embed_nonce=${nonce}&tab=browser`;
const cookie = 'ch_embed=opaque.signed.value; Path=/; Expires=Wed, 01 Jan 2099 00:00:00 GMT; Max-Age=600; HttpOnly; Secure; SameSite=Strict';

test('Host embed exchange posts the exact Portal origin and returns only a strict host cookie', async () => {
  const originalFetch = globalThis.fetch;
  let call;
  globalThis.fetch = async (url, options) => {
    call = { url: String(url), options };
    return new Response(null, { status: 303, headers: { location, 'set-cookie': cookie } });
  };
  try {
    const result = await exchangeHostEmbedTicket(`${hostOrigin}/__ch/embed-auth`, hostOrigin, portalOrigin,
      ticket, nonce, 'browser', ['browser', 'terminal']);
    assert.equal(call.url, `${hostOrigin}/__ch/embed-auth`);
    assert.equal(call.options.method, 'POST');
    assert.equal(call.options.redirect, 'manual');
    assert.equal(call.options.headers.origin, portalOrigin);
    assert.equal(call.options.headers.authorization, undefined);
    assert.match(call.options.headers.traceparent, /^00-[0-9a-f]{32}-[0-9a-f]{16}-01$/);
    assert.ok(call.options.headers['X-Request-Id']);
    assert.deepEqual(Object.fromEntries(call.options.body.entries()), { ticket, embed_nonce: nonce });
    assert.equal(result.url, location);
    assert.equal(result.cookie.value, 'opaque.signed.value');
    assert.equal(result.cookie.secure, true);
    assert.ok(result.cookie.expirationDate > Date.now() / 1000);
  } finally { globalThis.fetch = originalFetch; }
});

test('Host embed exchange rejects off-host redirects, ticket replay responses and unsafe cookies', async () => {
  const originalFetch = globalThis.fetch;
  try {
    globalThis.fetch = async () => new Response(null, { status: 303,
      headers: { location: location.replace(hostOrigin, 'https://evil.example.test'), 'set-cookie': cookie } });
    await assert.rejects(() => exchangeHostEmbedTicket(`${hostOrigin}/__ch/embed-auth`, hostOrigin, portalOrigin,
      ticket, nonce, 'browser', ['browser']), /invalid Workbench location/);

    globalThis.fetch = async () => new Response(null, { status: 404, headers: { 'X-Trace-Id': 'b'.repeat(32) } });
    await assert.rejects(() => exchangeHostEmbedTicket(`${hostOrigin}/__ch/embed-auth`, hostOrigin, portalOrigin,
      ticket, nonce, 'browser', ['browser']), /HTTP 404.*trace ID b{32}/);

    globalThis.fetch = async () => new Response(null, { status: 303,
      headers: { location, 'set-cookie': 'ch_embed=opaque; Path=/; HttpOnly; Secure; SameSite=Lax; Max-Age=600' } });
    await assert.rejects(() => exchangeHostEmbedTicket(`${hostOrigin}/__ch/embed-auth`, hostOrigin, portalOrigin,
      ticket, nonce, 'browser', ['browser']), /unsafe session cookie/);

    globalThis.fetch = async () => new Response(null, { status: 303,
      headers: { location, 'set-cookie': 'ch_embed=opaque; Path=/; HttpOnly; Secure; SameSite=Strict; Max-Age=-1; Expires=Wed, 01 Jan 2099 00:00:00 GMT' } });
    await assert.rejects(() => exchangeHostEmbedTicket(`${hostOrigin}/__ch/embed-auth`, hostOrigin, portalOrigin,
      ticket, nonce, 'browser', ['browser']), /unsafe session cookie/);

    globalThis.fetch = async () => new Response(null, { status: 303,
      headers: { location, 'set-cookie': 'ch_embed=opaque; Domain=hosts.example.test; Path=/; HttpOnly; Secure; SameSite=Strict; Max-Age=600' } });
    await assert.rejects(() => exchangeHostEmbedTicket(`${hostOrigin}/__ch/embed-auth`, hostOrigin, portalOrigin,
      ticket, nonce, 'browser', ['browser']), /unsafe session cookie/);
  } finally { globalThis.fetch = originalFetch; }
});

test('Host embed exchange rejects credentials embedded in the Workbench location', async () => {
  const originalFetch = globalThis.fetch;
  globalThis.fetch = async () => new Response(null, { status: 303,
    headers: { location: location.replace(hostOrigin, 'https://user:secret@h1234567.hosts.example.test'), 'set-cookie': cookie } });
  try {
    await assert.rejects(() => exchangeHostEmbedTicket(`${hostOrigin}/__ch/embed-auth`, hostOrigin, portalOrigin,
      ticket, nonce, 'browser', ['browser']), /invalid Workbench location/);
  } finally { globalThis.fetch = originalFetch; }
});

test('Host embed exchange accepts only the explicitly scoped local development Gateway', async () => {
  const originalFetch = globalThis.fetch;
  globalThis.fetch = async () => new Response(null, { status: 303,
    headers: { location: 'http://h1234567.hosts.localhost/__ch/wb/?embed=1&embed_nonce=' + nonce + '&tab=browser',
      'set-cookie': 'ch_embed=opaque; Path=/; HttpOnly; SameSite=Strict; Max-Age=600' } });
  try {
    const result = await exchangeHostEmbedTicket('http://h1234567.hosts.localhost/__ch/embed-auth',
      'http://h1234567.hosts.localhost', 'http://localhost', ticket, nonce, 'browser', ['browser']);
    assert.equal(result.cookie.secure, false);
  } finally { globalThis.fetch = originalFetch; }
});
