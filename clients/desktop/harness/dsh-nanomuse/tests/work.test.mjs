// The work a real quit or a restart-update protects (the desktop shell's exit
// asks the host for it): the turns in progress across every session, and the
// aggregate built over every source of work. Run after `node build.mjs`.
import assert from 'node:assert/strict'
import { test } from 'node:test'
import { Turns, aggregateWork } from '../lib/work.js'

test('a turn is busy from its start to its end, whatever the session', () => {
  const turns = new Turns()
  assert.equal(turns.sessions().length, 0)
  turns.began('s1')
  turns.began('s2')
  assert.deepEqual(turns.sessions(), ['s1', 's2'])
  turns.began('s1', 5)
  assert.deepEqual(turns.sessions(), ['s1', 's2'], 'a second start on one session is the same turn')
  turns.ended('s1')
  assert.deepEqual(turns.sessions(), ['s2'])
  turns.ended('s2')
  assert.deepEqual(turns.sessions(), [])
  turns.ended('s2')
  assert.deepEqual(turns.sessions(), [], 'an end without a start changes nothing')
})

test('the aggregate names every kind of work there is', () => {
  assert.deepEqual(aggregateWork({ turnSessions: [], taskRuns: [], handsBusy: false, approvals: 0 }), [])
  assert.deepEqual(aggregateWork({ turnSessions: ['s1'], taskRuns: [], handsBusy: false, approvals: 0 }), [{ kind: 'reply', label: '' }])
  assert.deepEqual(aggregateWork({ turnSessions: [], taskRuns: [], handsBusy: true, approvals: 0 }), [{ kind: 'computer', label: '' }])
  assert.deepEqual(aggregateWork({ turnSessions: [], taskRuns: [], handsBusy: false, approvals: 2 }), [{ kind: 'approval', label: '' }])
  assert.deepEqual(aggregateWork({ turnSessions: ['s1'], taskRuns: [], handsBusy: true, approvals: 1 }), [
    { kind: 'reply', label: '' },
    { kind: 'computer', label: '' },
    { kind: 'approval', label: '' },
  ])
})

test('the session of a task counts as the task, not as a reply', () => {
  const items = aggregateWork({ turnSessions: ['s1', 's2'], taskRuns: [{ sessionId: 's1', from: 'Phone' }], handsBusy: false, approvals: 0 })
  assert.deepEqual(items, [{ kind: 'reply', label: '' }, { kind: 'task', label: 'Phone' }])
})

test('the task line names the asking devices once each', () => {
  const items = aggregateWork({
    turnSessions: [],
    taskRuns: [{ sessionId: 's1', from: 'Phone' }, { sessionId: 's2', from: 'Phone' }, { sessionId: 's3', from: 'Tablet' }],
    handsBusy: false,
    approvals: 0,
  })
  assert.deepEqual(items, [{ kind: 'task', label: 'Phone, Tablet' }])
})
