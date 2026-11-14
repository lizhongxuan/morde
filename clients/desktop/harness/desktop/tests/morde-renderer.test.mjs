import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import vm from 'node:vm';

class Element {
  hidden = false;
  value = '';
  textContent = '';
  className = '';
  disabled = false;
  onclick;
  onsubmit;
}

async function mount(api) {
  const elements = new Map();
  const document = { getElementById(id) { if (!elements.has(id)) elements.set(id, new Element()); return elements.get(id); } };
  ['login', 'offline-page', 'offline-message', 'login-form', 'origin', 'name', 'password', 'submit', 'message', 'register', 'download', 'retry-assistant', 'sign-out'].forEach((id) => document.getElementById(id));
  const source = await readFile(new URL('../resources/morde.js', import.meta.url), 'utf8');
  vm.runInNewContext(source, { window: { morde: api }, document }, { filename: 'morde.js' });
  await new Promise((resolve) => setImmediate(resolve));
  return elements;
}

test('login reports failures and clears the password after a successful login', async () => {
  const calls = [];
  const api = {
    state: async () => ({ authenticated: false, origin: 'https://portal.example', storageReady: true, view: 'web' }),
    login: async (...args) => {
      calls.push(args);
      if (calls.length === 1) throw new Error("Error invoking remote method 'morde:login': Error: 用户名或密码错误，请重试。");
      return { authenticated: true, origin: 'https://portal.example', storageReady: true, view: 'web' };
    },
    onState() {},
    onSync() {},
  };
  const elements = await mount(api);
  elements.get('name').value = 'user';
  elements.get('password').value = 'secret';
  elements.get('login-form').onsubmit({ preventDefault() {} });
  await new Promise((resolve) => setImmediate(resolve));
  assert.equal(elements.get('message').textContent, '用户名或密码错误，请重试。');
  elements.get('login-form').onsubmit({ preventDefault() {} });
  await new Promise((resolve) => setImmediate(resolve));
  assert.deepEqual(calls, [
    ['https://portal.example', 'user', 'secret'],
    ['https://portal.example', 'user', 'secret'],
  ]);
  assert.equal(elements.get('password').value, '');
  assert.equal(elements.get('login').hidden, true);
  assert.equal(elements.get('offline-page').hidden, true);
});

test('offline fallback retries the PC page instead of opening a standalone sync view', async () => {
  let current = { authenticated: true, origin: 'https://portal.example', storageReady: true, view: 'offline' };
  const calls = [];
  let logoutCount = 0;
  const api = {
    state: async () => current,
    openAssistant: async () => { calls.push('open-assistant'); current = { ...current, view: 'web' }; },
    logout: async () => { logoutCount++; return { authenticated: false, origin: 'https://portal.example', storageReady: true, view: 'web' }; },
    publicPage: async (...args) => calls.push(args),
    onState() {},
    onSync() {},
  };
  const elements = await mount(api);
  assert.equal(elements.get('offline-page').hidden, false);
  assert.equal(elements.get('login').hidden, true);
  assert.match(elements.get('offline-message').textContent, /本机项目/);
  elements.get('retry-assistant').onclick();
  await new Promise((resolve) => setImmediate(resolve));
  assert.deepEqual(calls[0], 'open-assistant');
  assert.equal(elements.get('offline-page').hidden, true);
  elements.get('sign-out').onclick();
  await new Promise((resolve) => setImmediate(resolve));
  assert.equal(logoutCount, 1);
  assert.equal(elements.get('login').hidden, false);
  elements.get('origin').value = 'https://portal.example';
  elements.get('register').onclick();
  await new Promise((resolve) => setImmediate(resolve));
  assert.deepEqual(calls[1], ['register', 'https://portal.example']);
});

test('desktop fallback contains no separate sync page or fixed notification overlay', async () => {
  const html = await readFile(new URL('../resources/morde.html', import.meta.url), 'utf8');
  const css = await readFile(new URL('../resources/morde.css', import.meta.url), 'utf8');
  assert.match(html, /id="offline-page"/);
  assert.match(html, /本机项目/);
  assert.doesNotMatch(html, /sync-page|project-upload|workspace-sync|local-sync/);
  assert.doesNotMatch(css, /position\s*:\s*fixed/);
});
