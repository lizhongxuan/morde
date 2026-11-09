import assert from 'node:assert/strict'
import { test } from 'node:test'
import {
  apiMessage,
  durationFor,
  failureMessage,
  hostOf,
  imageToVideo,
  imageToVideoBody,
  KNOWN_DASHSCOPE_MODELS,
  looksLikeVideoModel,
  modelFor,
  probe,
  speaksDashScope,
  textToVideoBody,
  uploadTemp,
  VideoGenError,
  wanResolution,
  wanSize,
} from '../lib/video.js'

const EP = { host: 'https://example.invalid', apiKey: 'k', model: 'wan2.2-i2v-flash', label: 'test', instanceId: 'test' }
const FRAME = { bytes: new Uint8Array([1, 2, 3]), mime: 'image/webp', name: 'idle.webp' }

function reply(status, body, type = 'application/json') {
  const text = typeof body === 'string' ? body : JSON.stringify(body)
  return new Response(status === 204 ? null : text, { status, headers: { 'content-type': type } })
}

test('hostOf strips the API paths the person may paste', () => {
  assert.equal(hostOf('https://dashscope.aliyuncs.com/compatible-mode/v1'), 'https://dashscope.aliyuncs.com')
  assert.equal(hostOf('https://dashscope.aliyuncs.com/api/v1/'), 'https://dashscope.aliyuncs.com')
  assert.equal(hostOf('https://cloud.example.invalid/v1'), 'https://cloud.example.invalid')
  assert.equal(hostOf('https://cloud.example.invalid/'), 'https://cloud.example.invalid')
  assert.ok(speaksDashScope('https://dashscope-intl.aliyuncs.com/compatible-mode/v1'))
  assert.ok(!speaksDashScope('https://openrouter.ai/api/v1'))
})

test('model names: video-ish, siblings, resolution, duration, size', () => {
  assert.equal(KNOWN_DASHSCOPE_MODELS[0], 'wan2.2-i2v-flash')
  assert.ok(looksLikeVideoModel('wan2.2-t2v-plus'))
  assert.ok(looksLikeVideoModel('MiniMax/MiniMax-H3'))
  assert.ok(!looksLikeVideoModel('qwen-plus'))
  assert.equal(modelFor('wan2.2-t2v-plus', true), 'wan2.2-i2v-plus')
  assert.equal(modelFor('wan2.2-i2v-flash', false), 'wan2.2-t2v-plus')
  assert.equal(modelFor('wan2.6-i2v', false), 'wan2.6-t2v')
  assert.equal(modelFor('wan2.7-t2v-2026-06-12', true), 'wan2.7-i2v', 'the dated snapshots differ between the two jobs')
  assert.equal(modelFor('wan2.7-i2v-2026-04-25', true), 'wan2.7-i2v-2026-04-25')
  assert.equal(modelFor('wan2.7-i2v', false), 'wan2.7-t2v')
  assert.ok(KNOWN_DASHSCOPE_MODELS.includes('wan2.7-i2v') && KNOWN_DASHSCOPE_MODELS.includes('wan2.7-t2v'))
  assert.equal(modelFor('MiniMax/MiniMax-H3', false), 'MiniMax/MiniMax-H3')
  assert.equal(wanResolution('wan2.2-i2v-flash'), '480P')
  assert.equal(wanResolution('wan2.6-i2v'), '720P')
  assert.equal(durationFor('wan2.2-i2v-flash', 4), undefined)
  assert.equal(durationFor('wan2.5-i2v-preview', 4), 5)
  assert.equal(durationFor('wan2.5-i2v-preview', 9), 10)
  assert.equal(durationFor('wan2.6-i2v', 1), 2)
  assert.equal(durationFor('wan2.6-i2v', 30), 15)
  assert.equal(durationFor('wan2.7-i2v', 4), 4)
  assert.equal(durationFor('MiniMax/MiniMax-H3', 2), 4)
  assert.equal(wanSize('16:9'), '1280*720')
  assert.equal(wanSize('1:1'), '960*960')
})

test('task bodies: Wan takes img_url and resolution, MiniMax takes media and 768P', () => {
  const wan = imageToVideoBody('wan2.2-i2v-flash', 'oss://x/y.webp', 'p', 4)
  assert.deepEqual(wan, { model: 'wan2.2-i2v-flash', input: { prompt: 'p', img_url: 'oss://x/y.webp' }, parameters: { watermark: false, resolution: '480P' } })
  const wan26 = imageToVideoBody('wan2.6-i2v', 'oss://x/y.webp', 'p', 4)
  assert.equal(wan26.parameters.duration, 4)
  const mm = imageToVideoBody('MiniMax/MiniMax-H3', 'oss://x/y.webp', 'p', 4)
  assert.deepEqual(mm.input, { prompt: 'p', media: [{ type: 'first_frame', url: 'oss://x/y.webp' }] })
  assert.deepEqual(mm.parameters, { watermark: false, resolution: '768P', duration: 4 })
  const wan27 = imageToVideoBody('wan2.7-i2v', 'oss://x/y.webp', 'p', 4)
  assert.deepEqual(wan27.input, { prompt: 'p', media: [{ type: 'first_frame', url: 'oss://x/y.webp' }] })
  assert.deepEqual(wan27.parameters, { watermark: false, resolution: '720P', duration: 4 })
  const t2v27 = textToVideoBody('wan2.7-t2v', 'p', 4, '21:9')
  assert.deepEqual(t2v27.parameters, { watermark: false, resolution: '720P', ratio: '16:9', duration: 4 })
  const t2v = textToVideoBody('wan2.2-t2v-plus', 'p', 4, '16:9')
  assert.deepEqual(t2v.parameters, { watermark: false, size: '1280*720' })
  const t2vmm = textToVideoBody('MiniMax/MiniMax-H3', 'p', 4, '9:16')
  assert.deepEqual(t2vmm.parameters, { watermark: false, resolution: '768P', ratio: '9:16', duration: 4 })
})

test('messages: DashScope top-level, relay OpenAI-shaped, activation wording', () => {
  assert.equal(apiMessage('{"message":"Model not exist"}'), ': Model not exist')
  assert.equal(apiMessage('{"error":{"message":"clip allowance used up"}}'), ': clip allowance used up')
  assert.equal(apiMessage('not json'), '')
  assert.match(failureMessage({ message: 'Model not activated' }), /not activated on this account/)
  assert.equal(failureMessage({ code: 'DataInspectionFailed', message: 'blocked' }), 'DataInspectionFailed: blocked')
  assert.equal(failureMessage({ task_status: 'CANCELED' }), 'Video task CANCELED')
})

test('imageToVideo: upload policy, OSS form, async task, polling, download', async () => {
  const calls = []
  let polls = 0
  const fetchImpl = async (url, init = {}) => {
    calls.push({ url: String(url), method: init.method ?? 'GET', headers: init.headers ?? {}, body: init.body })
    const u = String(url)
    if (u.includes('/api/v1/uploads?action=getPolicy&model=wan2.2-i2v-flash')) {
      return reply(200, { data: { upload_host: 'https://oss.example.invalid', upload_dir: 'tmp/dir', oss_access_key_id: 'id', signature: 'sig', policy: 'pol', x_oss_object_acl: 'private', x_oss_forbid_overwrite: 'true' } })
    }
    if (u === 'https://oss.example.invalid') {
      assert.ok(init.body instanceof FormData)
      assert.equal(init.body.get('key'), 'tmp/dir/idle.webp')
      assert.equal(init.body.get('OSSAccessKeyId'), 'id')
      assert.equal(init.body.get('Signature'), 'sig')
      assert.equal(init.body.get('policy'), 'pol')
      assert.equal(init.body.get('x-oss-object-acl'), 'private')
      assert.equal(init.body.get('x-oss-forbid-overwrite'), 'true')
      assert.equal(init.body.get('file').type, 'image/webp')
      return reply(204, '')
    }
    if (u.endsWith('/video-generation/video-synthesis')) {
      assert.equal(init.headers['X-DashScope-Async'], 'enable')
      assert.equal(init.headers['X-DashScope-OssResourceResolve'], 'enable')
      const body = JSON.parse(init.body)
      assert.equal(body.input.img_url, 'oss://tmp/dir/idle.webp')
      assert.equal(body.parameters.resolution, '480P')
      return reply(200, { output: { task_id: 't-1', task_status: 'PENDING' } })
    }
    if (u.endsWith('/api/v1/tasks/t-1')) {
      polls += 1
      if (polls < 3) return reply(200, { output: { task_status: 'RUNNING' } })
      return reply(200, { output: { task_status: 'SUCCEEDED', video_url: 'https://cdn.example.invalid/clip.mp4' } })
    }
    if (u === 'https://cdn.example.invalid/clip.mp4') return new Response(new Uint8Array([9, 9, 9, 9]), { status: 200 })
    throw new Error(`unexpected ${u}`)
  }
  const stages = []
  const bytes = await imageToVideo(EP, FRAME, 'wave', 4, { fetch: fetchImpl, sleep: async () => undefined, onProgress: (p) => stages.push(p.kind) })
  assert.deepEqual([...bytes], [9, 9, 9, 9])
  assert.equal(polls, 3)
  assert.deepEqual(stages.slice(0, 2), ['uploading', 'submitted'])
  assert.ok(stages.includes('running'))
  assert.equal(stages[stages.length - 1], 'downloading')
  assert.equal(calls[0].headers.authorization, 'Bearer k')
})

test('imageToVideo: a failed task carries the reason; a long one times out', async () => {
  const policy = { data: { upload_host: 'https://oss.example.invalid', upload_dir: 'd', oss_access_key_id: 'i', signature: 's', policy: 'p' } }
  const base = async (url, init = {}) => {
    const u = String(url)
    if (u.includes('getPolicy')) return reply(200, policy)
    if (u === 'https://oss.example.invalid') return reply(204, '')
    if (u.endsWith('video-synthesis')) return reply(200, { output: { task_id: 't' } })
    return undefined
  }
  const failed = async (url, init) => (await base(url, init)) ?? reply(200, { output: { task_status: 'FAILED', code: 'InternalError', message: 'boom' } })
  await assert.rejects(imageToVideo(EP, FRAME, 'x', 4, { fetch: failed, sleep: async () => undefined }), (e) => e instanceof VideoGenError && e.message === 'InternalError: boom')
  let t = 0
  const slow = async (url, init) => (await base(url, init)) ?? reply(200, { output: { task_status: 'RUNNING' } })
  await assert.rejects(imageToVideo(EP, FRAME, 'x', 4, { fetch: slow, sleep: async () => undefined, now: () => (t += 60_000), maxWaitMs: 12 * 60_000 }), /Timed out/)
})

test('uploadTemp and probe report the API messages', async () => {
  const denied = async () => reply(403, { message: 'AccessDenied' })
  await assert.rejects(uploadTemp(EP, FRAME, denied), /Upload policy failed \(HTTP 403\): AccessDenied/)
  assert.equal(await probe(EP.host, 'k', 'nope', async () => reply(404, { message: 'Model not exist' })), false)
  assert.equal(await probe(EP.host, 'k', 'wan2.2-i2v-flash', async () => reply(400, { message: 'InvalidParameter: prompt' })), true)
  assert.equal(await probe(EP.host, 'k', 'wan2.2-i2v-flash', async () => reply(401, { message: 'bad key' })), undefined)
  assert.equal(
    await probe(EP.host, 'k', 'x', async () => {
      throw new Error('offline')
    }),
    undefined,
  )
})
