// The words that change the look, read as the phone reads them (AvatarFlow.kt).
import assert from 'node:assert/strict'
import { test } from 'node:test'
import { avatarFence, avatarMemoryLine, parseAvatarChoice, parseAvatarRequest } from '../lib/avatar-flow.js'

test('Chinese requests give the description without the particles', () => {
  assert.deepEqual(parseAvatarRequest('把你的形象换成一只橘猫吧'), { desc: '一只橘猫', lang: 'zh' })
  assert.deepEqual(parseAvatarRequest('帮我换个头像：赛博朋克风格的狐狸'), { desc: '赛博朋克风格的狐狸', lang: 'zh' })
  assert.deepEqual(parseAvatarRequest('变成一个穿西装的熊猫的样子'), { desc: '一个穿西装的熊猫', lang: 'zh' })
  assert.equal(parseAvatarRequest('变成猫'), undefined, 'one character is not a description')
  assert.equal(parseAvatarRequest('帮我看看这个形象设计稿'), undefined)
})

test('English requests drop the article and keep the rest', () => {
  assert.deepEqual(parseAvatarRequest('Please change your avatar to a red fox in a scarf!'), { desc: 'red fox in a scarf', lang: 'en' })
  assert.deepEqual(parseAvatarRequest('new avatar: pixel-art robot'), { desc: 'pixel-art robot', lang: 'en' })
  assert.deepEqual(parseAvatarRequest('become a wise old owl'), { desc: 'wise old owl', lang: 'en' })
  assert.equal(parseAvatarRequest('be concise'), undefined, '"be X" needs a creature or a character')
  assert.equal(parseAvatarRequest('turn the lights off'), undefined)
  assert.equal(parseAvatarRequest('what does my avatar look like'), undefined)
})

test('a choice among four is read in both languages', () => {
  assert.deepEqual(parseAvatarChoice('第二个'), { kind: 'pick', index: 2 })
  assert.deepEqual(parseAvatarChoice('3'), { kind: 'pick', index: 3 })
  assert.deepEqual(parseAvatarChoice('option 4 please'), { kind: 'pick', index: 4 })
  assert.deepEqual(parseAvatarChoice('the first one'), { kind: 'pick', index: 1 })
  assert.deepEqual(parseAvatarChoice('左下那个'), { kind: 'pick', index: 3 })
  assert.deepEqual(parseAvatarChoice('bottom right'), { kind: 'pick', index: 4 })
  assert.deepEqual(parseAvatarChoice('换一批'), { kind: 'regenerate' })
  assert.deepEqual(parseAvatarChoice('None of these'), { kind: 'regenerate' })
  assert.deepEqual(parseAvatarChoice('tell me a joke'), { kind: 'none' })
})

test('the fence and the memory line match the phone', () => {
  assert.equal(avatarFence({ desc: 'a fox', chosen: 2, files: ['idle.webp'] }), '```nanomuse-avatar\n{"desc":"a fox","chosen":2,"files":["idle.webp"]}\n```')
  assert.equal(avatarMemoryLine('a fox', new Date(2026, 9, 4)), '- 2026-10-04: the user changed my avatar to "a fox".')
})
