/*
 * Portions adapted from Hermes Agent, Copyright (c) 2025 Nous Research.
 *
 * Permission is hereby granted, free of charge, to any person obtaining a copy
 * of this software and associated documentation files (the "Software"), to deal
 * in the Software without restriction, including without limitation the rights
 * to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
 * copies of the Software, and to permit persons to whom the Software is
 * furnished to do so, subject to the following conditions:
 *
 * The above copyright notice and this permission notice shall be included in all
 * copies or substantial portions of the Software.
 *
 * THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
 * IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
 * FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
 * AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
 * LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
 * OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
 * SOFTWARE.
 */

import { isCompactCheckpointSource, toolPairingBalancedBefore } from '@deepseek-ai/dsh-compaction'

// Hermes ContextCompressor._compute_threshold_tokens and lean_tail_budget.
export function hermesCompactionBudget(contextWindow, outputTokens = 0) {
  if (!Number.isSafeInteger(contextWindow) || contextWindow < 2 ||
      !Number.isSafeInteger(outputTokens) || outputTokens < 0 || outputTokens >= contextWindow) {
    throw new Error('model context capacity cannot accommodate the output reservation')
  }
  const inputWindow = contextWindow - outputTokens
  const ratio = contextWindow < 512_000 ? 0.75 : 0.5
  const percent = Math.floor(inputWindow * ratio)
  let thresholdTokens = Math.max(percent, 64_000)
  const cap = Math.floor(inputWindow * 0.85)
  if (thresholdTokens > percent && thresholdTokens > cap) thresholdTokens = Math.max(percent, cap)
  if (thresholdTokens >= inputWindow) thresholdTokens = Math.max(1, Math.min(cap, inputWindow - 1))

  const share = Math.floor(thresholdTokens * 0.5)
  const floor = Math.min(10_000, share)
  let windowTerm = Math.min(25_000, Math.floor(contextWindow * 0.025))
  if (thresholdTokens > 2_000) windowTerm = Math.min(windowTerm, share)
  const tailTokens = Math.max(1, floor, windowTerm)
  return { thresholdTokens, tailTokens }
}

// Input rows are current DSH surface order, priced by TokenMeter. isUserInput identifies
// real user messages (including non-text input), isSummary identifies prior checkpoints,
// and balancedBefore is DSH's tool-pair balance at that cut. A pinned child retains its
// opening brief. The latest user input is never removed: DSH has no verbatim turn replay.
export function planHermesCompaction(rows, { contextWindow, outputTokens = 0, repeated = false, pinTask = false }) {
  const { tailTokens } = hermesCompactionBudget(contextWindow, outputTokens)
  if (!Array.isArray(rows) || rows.some(row => !row || !Number.isFinite(row.tokens) || row.tokens < 0 ||
      typeof row.balancedBefore !== 'boolean')) throw new Error('invalid compaction surface measurement')
  const n = rows.length
  if (!n) return null

  const systemHead = rows[0].role === 'system' ? 1 : 0
  const resumedSummary = rows.slice(systemHead, systemHead + 7).some(row => row.isSummary)
  let start = Math.min(n, systemHead + (pinTask ? 1 : repeated || resumedSummary ? 0 : 3))
  while (start < n && !rows[start].balancedBefore) start++
  if (start >= n) return null

  const available = Math.max(0, n - start - 1)
  const minTail = available > 1 ? Math.min(20, Math.max(3, available - 2), available) : 0
  const ceiling = Math.max(tailTokens, Math.min(Math.floor(tailTokens * 1.5), Math.floor(contextWindow * 0.2)))
  const walkFloor = ceiling >= minTail * 10 ? 0 : minTail
  let newestSummary = -1
  for (let i = n - 1; i >= start; i--) if (rows[i].isSummary) { newestSummary = i; break }
  const price = i => i <= newestSummary ? 0 : rows[i].tokens
  const walk = (budget, floor, cutAtBreak) => {
    let cut = n
    let total = 0
    for (let i = n - 1; i >= start; i--) {
      const cost = price(i)
      if (total + cost > budget && n - i >= floor) return [cutAtBreak ? i : cut, total]
      total += cost
      cut = i
    }
    return [cut, total]
  }
  let [cut, accumulated] = walk(ceiling, walkFloor, false)
  if (cut <= start && accumulated > 0 && accumulated <= ceiling) [cut] = walk(tailTokens, minTail, true)
  cut = Math.min(cut, n - Math.max(walkFloor, 1))
  if (cut <= start) cut = Math.max(n - minTail, start + 1)

  const alignBackward = boundary => {
    if (boundary <= 0 || boundary >= n) return boundary
    let previous = boundary - 1
    while (previous >= 0 && rows[previous].role === 'tool') previous--
    if (previous >= 0 && rows[previous].role === 'assistant' && rows[previous].hasToolCalls) boundary = previous
    while (boundary > start && !rows[boundary].balancedBefore) boundary--
    return boundary
  }
  cut = alignBackward(cut)

  let lastUser = -1
  let lastAssistant = -1
  let lastTextAssistant = -1
  for (let i = n - 1; i >= start; i--) {
    const row = rows[i]
    if (lastUser < 0 && row.isUserInput && !row.isSummary) lastUser = i
    if (row.role === 'assistant' && !row.isSummary) {
      if (lastAssistant < 0) lastAssistant = i
      if (lastTextAssistant < 0 && row.hasText) lastTextAssistant = i
    }
  }
  if (lastUser >= start && lastUser < cut) cut = lastUser
  const assistant = lastTextAssistant < 0 ? lastAssistant : lastTextAssistant
  if (assistant >= start && assistant < cut) cut = Math.max(alignBackward(assistant), start + 1)

  let progressFloor = start + 1
  if (newestSummary >= start && rows.slice(start, Math.max(cut, newestSummary + 1)).every(row => row.isSummary)) {
    let raised = newestSummary + 2
    while (raised < n && !rows[raised].balancedBefore) raised++
    if (raised < n && (lastUser < 0 || lastUser >= raised)) progressFloor = raised
  }
  cut = Math.max(cut, progressFloor)
  while (cut < n && !rows[cut].balancedBefore) cut++
  if (cut === lastUser && cut > start + 1) {
    let bridge = cut - 1
    if (rows[bridge].role === 'tool') bridge = alignBackward(cut)
    else if (rows[bridge].role !== 'assistant') bridge = -1
    if (bridge >= progressFloor) cut = bridge
  }
  if (cut <= start || cut >= n || (lastUser >= start && lastUser < cut) || !rows[cut].balancedBefore ||
      rows.slice(start, cut).every(row => row.isSummary)) return null
  return { start, end: cut }
}

export function selectHermesCompactionRange(session, measurement, { contextWindow, outputTokens = 0, pinTask = false }) {
  const rows = measurement.nodes.map(node => {
    const event = session.eventAt(node.seq)
    const message = event && session.deriveEventMessage(event)
    const isSummary = !!message?.source && isCompactCheckpointSource(message.source)
    return {
      role: message?.role ?? '',
      tokens: node.tokens,
      balancedBefore: toolPairingBalancedBefore(session, node.seq),
      isUserInput: message?.role === 'user' && message.source?.kind === 'user',
      isSummary,
      hasToolCalls: message?.content.some(block => block.type === 'tool-call') ?? false,
      hasText: message?.content.some(block => block.type === 'text' && block.text.length > 0) ?? false,
    }
  })
  const range = planHermesCompaction(rows, {
    contextWindow, outputTokens, repeated: rows.some(row => row.isSummary), pinTask,
  })
  return range === null ? null : {
    start: measurement.nodes[range.start].seq,
    end: measurement.nodes[range.end - 1].seq,
  }
}

export function originalToolResultsForRange(session, start, end) {
  const surface = session.surface.nodes
  const from = surface.indexOf(start)
  const through = surface.indexOf(end)
  const originals = new Map()
  if (from < 0 || through < from) return originals
  for (const seq of surface.slice(from, through + 1)) {
    const event = session.eventAt(seq)
    if (event?.type !== 'tool/result' || event.surfaceOp === 'append') continue
    const callId = event.data.message?.source?.callId
    if (!callId) continue
    const pending = [...(event.sourceEventSeqs ?? [])]
    const visited = new Set()
    while (pending.length) {
      const sourceSeq = pending.pop()
      if (visited.has(sourceSeq)) continue
      visited.add(sourceSeq)
      const source = session.eventAt(sourceSeq)
      if (source?.type === 'tool/result' && source.data.message?.source?.callId === callId && source.surfaceOp === 'append') {
        const message = session.deriveEventMessage(source)
        if (message) originals.set(message.toolCallId, message)
        break
      }
      pending.push(...(source?.sourceEventSeqs ?? []))
    }
  }
  return originals
}
