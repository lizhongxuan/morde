// The app fences and the Ideas list, read as the phone reads them (GoalFlow.kt, FeedFlow.kt, Ideas.kt).
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { test } from 'node:test'
import { FENCE_FEED, FENCE_GOAL, FENCE_GOAL_UPDATE, findFences, goalCategory, goalCheckPrompt, goalCreationNote, goalOpener, parseCheckTime, parseFeedDraft, parseGoalBlock, parseGoalUpdate, parseIdeas } from '../lib/fences.js'

const here = new URL('.', import.meta.url)

test('fences are found by tag, complete ones only', () => {
  const text = 'Done.\n```nanomuse-goal\n{"title":"Run 5k"}\n```\nand\n```nanomuse-goal-update\n{"progress":40}\n```\n```nanomuse-goal\n{"title":"streaming'
  assert.deepEqual(findFences(text, FENCE_GOAL), ['{"title":"Run 5k"}\n'])
  assert.deepEqual(findFences(text, FENCE_GOAL_UPDATE), ['{"progress":40}\n'])
  assert.deepEqual(findFences('nothing here', FENCE_FEED), [])
})

test('a goal block is read with the phone defaults and limits', () => {
  const block = parseGoalBlock('{"title":"Run a 5k by June","why":"Energy","category":"interests","check_every_hours":"6","check_time":"7:05","steps":["Shoes","Plan","", "Run", "Rest", "Log", "Extra", "Too many"],"first_check":"Find a plan"}')
  assert.equal(block.title, 'Run a 5k by June')
  assert.equal(block.category, 'hobbies', 'the phone says interests where this side says hobbies')
  assert.equal(block.checkEveryHours, 6)
  assert.equal(block.checkTime, '07:05')
  assert.equal(block.steps.length, 6)
  assert.equal(block.firstCheck, 'Find a plan')
  assert.equal(parseGoalBlock('{"why":"no title"}'), undefined)
  assert.equal(parseGoalBlock('not json'), undefined)
  assert.equal(parseCheckTime('at 21:30 please'), '21:30')
  assert.equal(parseCheckTime(''), '09:00')
  assert.equal(goalCategory('FINANCE'), 'finance')
  assert.equal(goalCategory('weird'), 'other')
})

test('a goal update is clamped and defaults to on_track', () => {
  assert.deepEqual(parseGoalUpdate('{"goal_id":"g1","progress":140,"status":"done","note":"All done"}'), { goalId: 'g1', progress: 100, status: 'done', note: 'All done' })
  assert.deepEqual(parseGoalUpdate('{"note":"hmm","status":"weird"}'), { goalId: '', progress: -1, status: 'on_track', note: 'hmm' })
  assert.equal(parseGoalUpdate('[]'), undefined)
})

test('a feed draft needs a title and a body; type and source are normalised', () => {
  assert.deepEqual(parseFeedDraft('{"emoji":"📰","title":"Rates held","type":"BRIEF","body":"- The bank held.","source":"ft.com; reuters.com"}'), { title: 'Rates held', body: '- The bank held.', type: 'brief', emoji: '📰', source: ['ft.com', 'reuters.com'] })
  assert.equal(parseFeedDraft('{"title":"no body"}'), undefined)
  assert.equal(parseFeedDraft('{"title":"x","body":"y","type":"odd"}').type, 'note')
})

test('the words around a goal match the phone, in both languages', () => {
  assert.equal(goalOpener('Health', false), "I'd like to create a Health goal.")
  assert.equal(goalOpener('健康', true), '我想创建一个健康目标。')
  assert.equal(goalCheckPrompt('Run 5k', false, 'Find a plan'), 'Time to check on this goal: Run 5k\nFirst time round: Find a plan')
  assert.equal(goalCheckPrompt('跑五公里', true), '到点了，看看这个目标：跑五公里')
  assert.match(goalCreationNote('health', false), /EXACTLY ONE fenced code block tagged `nanomuse-goal`/)
  assert.match(goalCreationNote('health', true), /"category": "health"/)
})

test('the Ideas list is shared with Morde iOS and mobile Web, byte for byte', () => {
  for (const lang of ['en', 'zh']) {
    const ours = readFileSync(new URL(`../assets/ideas.${lang}.json`, here), 'utf8')
    const ios = readFileSync(new URL(`../../../../nanoMuse/android/src/ios/NanoMuse/Resources/ideas.${lang}.json`, here), 'utf8')
    const mobile = readFileSync(new URL(`../../../../../web/mobile/src/ideas/ideas.${lang}.json`, here), 'utf8')
    assert.equal(ours, ios, `assets/ideas.${lang}.json must match the Morde iOS file`)
    assert.equal(ours, mobile, `assets/ideas.${lang}.json must match the Morde mobile Web file`)
    const ideas = parseIdeas(ours)
    assert.equal(ideas.length, 24)
    assert.deepEqual([...new Set(ideas.map((i) => i.section))], ['travel', 'work', 'life', 'learn', 'family', 'more'])
    assert.ok(ideas.every((i) => i.id.includes('/') && i.title && i.prompt && ['chat', 'routine', 'goal'].includes(i.kind)))
    assert.ok(ideas.filter((i) => i.kind === 'routine').every((i) => /^\d{2}:\d{2}$/.test(i.time)), 'routine ideas carry a time')
    assert.ok(ideas.filter((i) => i.kind === 'goal').every((i) => i.category), 'goal ideas carry a category')
  }
  assert.deepEqual(parseIdeas('{"sections":[{"id":"x","title":"X","ideas":[{"id":"a","title":"A","kind":"ODD"}]}]}')[0].kind, 'chat')
  assert.deepEqual(parseIdeas('nope'), [])
})
