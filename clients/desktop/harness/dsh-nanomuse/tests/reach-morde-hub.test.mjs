import assert from 'node:assert/strict'
import { test } from 'node:test'
import { apply } from '../lib/reach.js'

const active = (overrides = {}) => ({
  id: 'itarget1', name: 'Phone', platform: 'desktop', online: true, revoked: false,
  expiresAt: new Date(Date.now() + 60_000).toISOString(), ...overrides,
})

function setup({ mordeConnected = true, mordeDevices = [active()], cloudDevices = [], mordeResult } = {}) {
  const tools = new Map()
  const approvals = []
  const mordeCalls = []
  const cloudCalls = []
  const cloudHub = {
    connected: true,
    deviceId: 'cloud-self',
    devices: cloudDevices,
    find(ref) {
      const others = this.devices.filter((device) => device.id !== this.deviceId && device.kind !== 'web')
      if (!ref) return others.length === 1 ? others[0] : undefined
      const wanted = ref.toLowerCase()
      return others.find((device) => device.id.toLowerCase() === wanted) ??
        others.find((device) => device.name.toLowerCase() === wanted) ??
        others.find((device) => device.name.toLowerCase().includes(wanted))
    },
    async call(...args) { cloudCalls.push(args); return args[1] === 'notify' ? {} : { ok: true, url: args[2].url } },
    nextId() { return 'call-1' },
  }
  const mordeStatus = {
    signedIn: true,
    connected: mordeConnected,
    installationID: 'imcur001',
    devices: [
      { id: 'imcur001', name: 'This computer', platform: 'desktop', online: true, revoked: false, expiresAt: new Date(Date.now() + 60_000).toISOString() },
      ...mordeDevices,
    ],
  }
  const cloud = {
    hub: cloudHub,
    signedIn: true,
    mordeHubView: () => mordeStatus,
    async mordeHubCall(...args) { mordeCalls.push(args); return mordeResult ?? { ok: true, url: args[2].url } },
    stageFrame() {},
  }
  const ctx = {
    nanomuseCloud: cloud,
    tools: { register(tool) { tools.set(tool.name, tool); return () => tools.delete(tool.name) } },
    effect(fn) { fn() },
    inject() {},
    get(name) {
      if (name !== 'approval') return undefined
      return { async request(value) { approvals.push(value); return 'allowed-once' } }
    },
    logger: { info() {}, warn() {} },
  }
  apply(ctx)
  const exec = { agent: {}, callId: 'source-call', signal: new AbortController().signal }
  return { tools, approvals, mordeCalls, cloudCalls, mordeStatus, exec }
}

test('devices labels active Morde installations separately from nanoMuse Cloud devices', async () => {
  const out = setup({
    cloudDevices: [{ id: 'legacy-phone', name: 'Phone', kind: 'phone', os: 'Android', online: true, last_seen: 1, controllable: true, actions: ['open'] }],
    mordeDevices: [
      active(),
      active({ id: 'irevoked', name: 'Revoked', revoked: true }),
      active({ id: 'iexpired', name: 'Expired', expiresAt: new Date(Date.now() - 60_000).toISOString() }),
    ],
  })
  const tool = out.tools.get('devices')
  const result = await tool.execute({}, out.exec)
  assert.equal(result.connected, true)
  assert.deepEqual(result.devices.map((device) => [device.id, device.source]), [
    ['legacy-phone', 'nanoMuse Cloud'],
    ['itarget1', 'Morde Hub'],
  ])
  const rendered = tool.output.render({}, result).map((part) => part.text).join('\n')
  assert.match(rendered, /nanoMuse Cloud/)
  assert.match(rendered, /Morde Hub/)
  assert.match(rendered, /itarget1/)
})

test('device_open routes an exact Morde id after approval and rejects local paths before approval', async () => {
  const out = setup({ cloudDevices: [{ id: 'legacy-phone', name: 'Phone', kind: 'phone', os: 'Android', online: true, last_seen: 1, controllable: true, actions: ['open'] }] })
  const tool = out.tools.get('device_open')
  await assert.rejects(tool.execute({ device: 'itarget1', url: '/tmp/report.pdf' }, out.exec), /only open HTTP\(S\) URLs/)
  assert.equal(out.approvals.length, 0)
  assert.equal(out.mordeCalls.length, 0)

  const result = await tool.execute({ device: 'itarget1', url: 'https://example.invalid/report' }, out.exec)
  assert.deepEqual(result, { device: 'Phone', ok: true, url: 'https://example.invalid/report' })
  assert.equal(out.approvals.length, 1)
  assert.equal(out.approvals[0].toolName, 'device_open')
  assert.deepEqual(out.mordeCalls.map(([id, action, args]) => [id, action, args]), [
    ['itarget1', 'open', { url: 'https://example.invalid/report' }],
  ])
  assert.equal(out.cloudCalls.length, 0)
})

test('Morde Hub target selection is exact and refuses stale or disconnected installations', async () => {
  const out = setup({
    mordeDevices: [active(), active({ id: 'iexpired', name: 'Expired', expiresAt: new Date(Date.now() - 60_000).toISOString() })],
  })
  const tool = out.tools.get('device_open')
  await assert.rejects(tool.execute({ device: 'iunknown', url: 'https://example.invalid/' }, out.exec), /no longer active/)
  await assert.rejects(tool.execute({ device: 'Phone', url: 'https://example.invalid/' }, out.exec), /exact Morde Hub installation id/)
  await assert.rejects(tool.execute({ device: 'iexpired', url: 'https://example.invalid/' }, out.exec), /no longer active/)
  assert.equal(out.approvals.length, 0)

  out.mordeStatus.connected = false
  await assert.rejects(tool.execute({ device: 'itarget1', url: 'https://example.invalid/' }, out.exec), /not connected to Morde Hub/)
  assert.equal(out.approvals.length, 0)
  assert.equal(out.mordeCalls.length, 0)
})

test('device_notify routes to Morde desktop without source approval and requires a positive receipt', async () => {
  const out = setup({ mordeResult: { notified: true } })
  const tool = out.tools.get('device_notify')
  const result = await tool.execute({ device: 'itarget1', text: 'Build finished', title: 'Workspace' }, out.exec)
  assert.deepEqual(result, { device: 'Phone', shown: true })
  assert.equal(out.approvals.length, 0)
  assert.deepEqual(out.mordeCalls.map(([id, action, args]) => [id, action, args]), [
    ['itarget1', 'notify', { text: 'Build finished', title: 'Workspace' }],
  ])
  assert.equal(out.mordeCalls[0][3].signal, out.exec.signal)
  assert.equal(out.mordeCalls[0][3].timeoutMs, 30_000)

  for (const mordeResult of [{}, { notified: 'true' }, { notified: false }]) {
    const unknown = setup({ mordeResult })
    const uncertain = await unknown.tools.get('device_notify').execute({ device: 'itarget1', text: 'Build finished' }, unknown.exec)
    assert.deepEqual(uncertain, { device: 'Phone', shown: false })
    assert.equal(unknown.approvals.length, 0)
  }
})

test('Morde mobile installations are rejected for open and notify before any request', async () => {
  const out = setup({ mordeDevices: [active({ id: 'iios0001', name: 'Mobile', platform: 'ios' })] })
  await assert.rejects(
    out.tools.get('device_open').execute({ device: 'iios0001', url: 'https://example.invalid/' }, out.exec),
    /PC client currently supports Morde desktop actions only/,
  )
  await assert.rejects(
    out.tools.get('device_notify').execute({ device: 'iios0001', text: 'Build finished' }, out.exec),
    /PC client currently supports Morde desktop actions only/,
  )
  assert.equal(out.approvals.length, 0)
  assert.equal(out.mordeCalls.length, 0)
})

test('legacy nanoMuse Cloud open still accepts a remote file path', async () => {
  const out = setup({ cloudDevices: [{ id: 'legacy-phone', name: 'Phone', kind: 'phone', os: 'Android', online: true, last_seen: 1, controllable: true, actions: ['open'] }] })
  const result = await out.tools.get('device_open').execute({ device: 'legacy-phone', url: '/sdcard/report.pdf' }, out.exec)
  assert.deepEqual(result, { device: 'Phone', ok: true, url: '/sdcard/report.pdf' })
  assert.equal(out.approvals.length, 1)
  assert.deepEqual(out.cloudCalls.map(([id, action, args]) => [id, action, args]), [
    ['legacy-phone', 'open', { url: '/sdcard/report.pdf' }],
  ])
  assert.equal(out.mordeCalls.length, 0)
})

test('legacy nanoMuse Cloud notify remains unchanged and needs no source approval', async () => {
  const out = setup({ cloudDevices: [{ id: 'legacy-phone', name: 'Phone', kind: 'phone', os: 'Android', online: true, last_seen: 1, controllable: true, actions: ['notify'] }] })
  const result = await out.tools.get('device_notify').execute({ device: 'legacy-phone', text: 'Build finished' }, out.exec)
  assert.deepEqual(result, { device: 'Phone', shown: true })
  assert.equal(out.approvals.length, 0)
  assert.deepEqual(out.cloudCalls.map(([id, action, args]) => [id, action, args]), [
    ['legacy-phone', 'notify', { text: 'Build finished' }],
  ])
  assert.equal(out.mordeCalls.length, 0)
})
