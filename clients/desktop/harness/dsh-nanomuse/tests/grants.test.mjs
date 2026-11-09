// The Permissions page's standing grants (src/desk.ts): every remembered permission as one
// list — the remote-control switch, the devices allowed without asking, the hands' per-app
// grants — with the tier each belongs to, and the grouping by tier the page draws.
import assert from 'node:assert/strict'
import { test } from 'node:test'
import { appLabel, appNames, DEVICE_GRANT_PREFIX, GRANT_TIERS, grantCovers, groupGrants, isOwnApp, REMOTE_CONTROL_GRANT_ID, scheduleRunIds, standingGrants, withGrant } from '../lib/desk.js'

test('the three tiers, highest first, are the contract', () => {
  assert.deepEqual([...GRANT_TIERS], ['highest', 'confirm', 'notice'])
})

test('nothing remembered is an empty list and no groups', () => {
  assert.deepEqual(standingGrants({}), [])
  assert.deepEqual(standingGrants({ grants: [], trusted: [], remoteControl: false }), [])
  assert.deepEqual(groupGrants([]), [])
})

test('every store lands in the list with its kind, tier and a revocable id', () => {
  const list = standingGrants({
    grants: [{ id: 'g-1a2b', target: 'computer_app:Safari', at: 3000 }],
    trusted: [{ id: 'dev-9', name: 'Pixel', at: 2000 }, { id: 'dev-nameless', name: '', at: 1000 }],
    remoteControl: true,
  })
  assert.deepEqual(list, [
    { id: REMOTE_CONTROL_GRANT_ID, kind: 'remote_control', tier: 'highest', target: '', at: 0 },
    { id: `${DEVICE_GRANT_PREFIX}dev-9`, kind: 'device', tier: 'confirm', target: 'Pixel', at: 2000 },
    { id: `${DEVICE_GRANT_PREFIX}dev-nameless`, kind: 'device', tier: 'confirm', target: 'dev-nameless', at: 1000 },
    { id: 'g-1a2b', kind: 'computer_app', tier: 'confirm', target: 'Safari', at: 3000 },
  ])
  // the hands' grant keeps the id the host gave it, so the Computer page's Revoke and this one are the same call
  assert.equal(list[3].id, 'g-1a2b')
})

test('the switch off and no devices leaves the hands alone in the card tier', () => {
  const list = standingGrants({ grants: [{ id: 'g-1', target: 'computer_app:Finder', at: 1 }], remoteControl: false })
  assert.equal(list.length, 1)
  assert.equal(list[0].tier, 'confirm')
  assert.equal(list[0].target, 'Finder')
})

test('groups come by tier, highest first, newest first inside, empty tiers left out; a grant in one chat or task stays in its tier', () => {
  const groups = groupGrants(standingGrants({
    grants: [
      { id: 'g-old', target: 'computer_app:Safari', at: 100 },
      { id: 'g-new', target: 'computer_app:Mail', at: 200 },
      { id: 'g-chat', target: 'computer_app:WeChat', at: 150, sessionId: 's1', sessionTitle: 'Morning' },
      { id: 'g-task', target: 'computer_app:WeChat', at: 160, sessionId: 's1', scheduleId: 'schedule-a', scheduleTitle: 'Note' },
    ],
    trusted: [{ id: 'x', name: 'Pixel', at: 300 }],
    remoteControl: true,
  }))
  assert.deepEqual(groups.map((g) => g.tier), ['highest', 'confirm'])
  assert.deepEqual(groups[0].grants.map((g) => g.id), ['remote-control'])
  assert.deepEqual(groups[1].grants.map((g) => g.id), ['device:x', 'g-new', 'g-task', 'g-chat', 'g-old'])
  const byId = Object.fromEntries(groups[1].grants.map((g) => [g.id, g]))
  assert.equal(byId['g-chat'].sessionTitle, 'Morning')
  assert.equal(byId['g-task'].scheduleTitle, 'Note')
})

test('a task grant whose task was deleted says so', () => {
  const grants = [{ id: 'g', target: 'computer_app:WeChat', at: 1, sessionId: 's1', scheduleId: 'schedule-a' }, { id: 'h', target: 'computer_app:WeChat', at: 1, sessionId: 's1', scheduleId: 'schedule-b' }]
  const list = standingGrants({ grants, goneSchedules: ['schedule-a'] })
  assert.deepEqual(list.map((g) => g.scheduleGone ?? false), [true, false])
})

test('a grant in one conversation covers that conversation only; one for every conversation covers all', () => {
  const grants = [
    { id: 'g-chat', target: 'computer_app:WeChat', at: 1, sessionId: 's-morning', sessionTitle: 'Morning message' },
    { id: 'g-all', target: 'computer_app:Safari', at: 2 },
  ]
  assert.equal(grantCovers(grants, 'WeChat', 's-morning'), true)
  assert.equal(grantCovers(grants, 'WeChat', 's-other'), false)
  assert.equal(grantCovers(grants, 'Safari', 's-other'), true)
  assert.equal(grantCovers(grants, 'Mail', 's-morning'), false)
  // no app in front: nothing is covered
  assert.equal(grantCovers(grants, '', 's-morning'), false)
})

test('giving the same grant again replaces it; the conversation and everywhere are kept apart', () => {
  let grants = withGrant([], { id: 'g-1', target: 'computer_app:WeChat', at: 1, sessionId: 's1', sessionTitle: 'A' })
  grants = withGrant(grants, { id: 'g-2', target: 'computer_app:WeChat', at: 2, sessionId: 's1', sessionTitle: 'A' })
  assert.deepEqual(grants.map((g) => g.id), ['g-2'])
  grants = withGrant(grants, { id: 'g-3', target: 'computer_app:WeChat', at: 3 })
  grants = withGrant(grants, { id: 'g-4', target: 'computer_app:WeChat', at: 4, sessionId: 's2', sessionTitle: 'B' })
  assert.deepEqual(grants.map((g) => g.id), ['g-2', 'g-3', 'g-4'])
  // the newest 64 are kept
  for (let i = 0; i < 70; i++) grants = withGrant(grants, { id: `g-x${i}`, target: `computer_app:App${i}`, at: 10 + i })
  assert.equal(grants.length, 64)
  assert.equal(grants.at(-1).id, 'g-x69')
})

test('a grant limited to a conversation carries it into the list', () => {
  const list = standingGrants({ grants: [{ id: 'g-c', target: 'computer_app:WeChat', at: 5, sessionId: 's1', sessionTitle: 'Morning message' }, { id: 'g-a', target: 'computer_app:Mail', at: 6 }] })
  assert.deepEqual(list, [
    { id: 'g-c', kind: 'computer_app', tier: 'confirm', target: 'WeChat', at: 5, sessionId: 's1', sessionTitle: 'Morning message' },
    { id: 'g-a', kind: 'computer_app', tier: 'confirm', target: 'Mail', at: 6 },
  ])
})

test('one app under its several head-line names is one app to a grant', () => {
  assert.deepEqual(appNames('微信 (WeChat)'), ['微信', 'WeChat'])
  assert.deepEqual(appNames('微信 (com.tencent.xinWeChat)'), ['微信', 'com.tencent.xinWeChat'])
  assert.deepEqual(appNames('WeChat'), ['WeChat'])
  assert.equal(appLabel('微信 (com.tencent.xinWeChat)'), '微信')
  const grants = [{ id: 'g', target: 'computer_app:微信 (WeChat)', at: 1, sessionId: 's1' }]
  // given on the whole screen, it holds in window mode and under the bare id
  assert.equal(grantCovers(grants, '微信 (com.tencent.xinWeChat)', 's1'), true)
  assert.equal(grantCovers(grants, 'WeChat', 's1'), true)
  // another app with a name of its own is not covered
  assert.equal(grantCovers(grants, '日历 (Calendar)', 's1'), false)
  assert.equal(grantCovers([{ id: 'g2', target: 'computer_app:Safari', at: 1 }], 'Safari (com.apple.Safari)', 's9'), true)
})

test('a task grant holds only in the runs that task starts, in its conversation', () => {
  const grants = [{ id: 'g', target: 'computer_app:微信 (WeChat)', at: 1, sessionId: 's1', scheduleId: 'schedule-a', scheduleTitle: 'Morning note' }]
  assert.equal(grantCovers(grants, '微信 (WeChat)', 's1', ['schedule-a']), true)
  // the same conversation, typed by hand
  assert.equal(grantCovers(grants, '微信 (WeChat)', 's1'), false)
  // another task of the conversation, or a run of both
  assert.equal(grantCovers(grants, '微信 (WeChat)', 's1', ['schedule-b']), false)
  assert.equal(grantCovers(grants, '微信 (WeChat)', 's1', ['schedule-a', 'schedule-b']), false)
  // another conversation
  assert.equal(grantCovers(grants, '微信 (WeChat)', 's2', ['schedule-a']), false)
  // a grant without a task still holds in a task's run
  assert.equal(grantCovers([{ id: 'h', target: 'computer_app:WeChat', at: 1, sessionId: 's1' }], 'WeChat', 's1', ['schedule-a']), true)
})

test('a task grant sits beside the conversation grant for the same app', () => {
  const chat = { id: 'g1', target: 'computer_app:WeChat', at: 1, sessionId: 's1' }
  const task = { id: 'g2', target: 'computer_app:WeChat', at: 2, sessionId: 's1', scheduleId: 'schedule-a' }
  assert.deepEqual(withGrant([chat], task).map((g) => g.id), ['g1', 'g2'])
  assert.deepEqual(withGrant([chat, task], { ...task, id: 'g3' }).map((g) => g.id), ['g1', 'g3'])
  const [listed] = standingGrants({ grants: [{ ...task, scheduleTitle: 'Morning note' }] })
  assert.equal(listed.scheduleId, 'schedule-a')
  assert.equal(listed.scheduleTitle, 'Morning note')
})

test('the tasks a reminder starts are read from the harness framing', () => {
  assert.deepEqual(scheduleRunIds('[SCHEDULE REMINDER]\nThis is a scheduled message from the user\nschedule_id_json: "schedule-a"\noccurrence_at: x\nreminder_prompt_json: "hi"'), ['schedule-a'])
  const batch = `[SCHEDULE REMINDER BATCH]\nThis is a scheduled message from the user\nreminders_json: ${JSON.stringify([{ schedule_id: 'schedule-a', occurrence_at: 'x', reminder_prompt: 'p' }, { schedule_id: 'schedule-b', occurrence_at: 'x', reminder_prompt: 'q\nr' }])}`
  assert.deepEqual(scheduleRunIds(batch), ['schedule-a', 'schedule-b'])
  assert.deepEqual(scheduleRunIds('[SCHEDULE REMINDER BATCH]\nreminders_json: not json'), [])
  assert.deepEqual(scheduleRunIds('send [SCHEDULE REMINDER] now'), [])
})

test('the own window of nanoMuse is never an app to allow', () => {
  assert.equal(isOwnApp('Electron'), true)
  assert.equal(isOwnApp('nanoMuse (io.github.nanomuse.desktop)'), true)
  assert.equal(isOwnApp('微信 (com.tencent.xinWeChat)'), false)
  assert.equal(isOwnApp(''), false)
})

test('a grant in one chat lists the scheduled tasks of that chat, which it covers too', () => {
  const grants = [{ id: 'c', target: 'computer_app:WeChat', at: 1, sessionId: 's1', sessionTitle: 'From iPhone' }, { id: 't', target: 'computer_app:WeChat', at: 2, sessionId: 's1', scheduleId: 'schedule-a' }]
  const [chat, task] = standingGrants({ grants, chatTasks: { s1: ['Morning note'] } })
  assert.deepEqual(chat.chatTasks, ['Morning note'])
  assert.equal(task.chatTasks, undefined)
  assert.equal(standingGrants({ grants }).at(0).chatTasks, undefined)
})
