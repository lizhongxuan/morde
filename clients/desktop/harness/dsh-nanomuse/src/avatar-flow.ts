/**
 * The words that change the agent's look from the chat, read the way the phone
 * reads them (android …/avatar/AvatarFlow.kt — the patterns are copied, not
 * paraphrased, so "变成一只橘猫" and "turn your avatar into a fox" mean the same
 * on both). Shared by the browser half (the composer intercept) and the tests;
 * no React, no DOM.
 *
 * A request becomes four candidates drawn by the image model, shown as a
 * `nanomuse-avatar` card; the person picks one by clicking or by words ("the
 * second one", "左下", "3"), or asks for another set. The fence the phone writes
 * into the transcript is the same JSON: `{"desc","chosen","files"}`.
 */

const ZH_REQUESTS = [
  /^(?:请|麻烦|帮我|帮忙|可以|能不能|能否)?\s*(?:把|将)?\s*(?:你的|你|我的|我)?\s*(?:虚拟)?(?:形象|头像|样子|外形)\s*(?:改|换|变|更换|切换|设置|设定|变更)(?:成|为|到|一下成|一下为)?\s*(.+)$/u,
  /^(?:请|麻烦|帮我|帮忙)?\s*(?:换|变|改)(?:个|一个|一下)?\s*(?:新的?)?(?:虚拟)?(?:形象|头像)\s*[：:，,、]?\s*(.+)$/u,
  /^(?:请|麻烦|帮我|帮忙)?\s*(?:变成|化身为|变身为|变身成)\s*(.+?)\s*(?:的)?(?:形象|样子|头像)?$/u,
]

const EN_REQUESTS = [
  /^(?:please\s+)?(?:can you\s+)?(?:change|switch|set|update|turn|make|transform)\s+(?:your|the|my|ur)?\s*(?:virtual\s+)?(?:avatar|appearance|look|character)\s+(?:to|into)\s+(.+)$/iu,
  /^(?:please\s+)?(?:new|another)\s+(?:virtual\s+)?avatar\s*[:,-]?\s*(.+)$/iu,
  /^(?:please\s+)?(?:become|be)\s+(.+)$/iu,
]

/** "be/become X" only counts when X sounds like a creature or a character; otherwise it is a sentence for the agent. */
const CHARACTER_WORDS =
  /\b(cat|dog|fox|wolf|bear|rabbit|bunny|bird|owl|dragon|robot|android|knight|wizard|witch|princess|prince|pirate|ninja|samurai|astronaut|alien|monster|ghost|fairy|elf|dwarf|mermaid|unicorn|panda|tiger|lion|deer|penguin|dinosaur|girl|boy|woman|man|lady|gentleman|character|creature|animal|hero|villain|pilot|doctor|nurse|chef|teacher|detective|cowboy|king|queen|angel|demon|vampire|zombie|otter|koala|frog|duck|hamster|mouse|sheep|cow|horse|pony|whale|dolphin|shark|octopus|spider|bee|butterfly|avatar)\b/i

const TRAILING = /[。.!！~～吧呗呀哦啊嘛\s]+$/u
const ARTICLE = /^(?:an?|the)\s+/iu

export interface AvatarRequest {
  /** What to draw, 1–200 characters, articles and trailing particles dropped. */
  desc: string
  lang: 'zh' | 'en'
}

/** The look the message asks for, or nothing when it is an ordinary message. */
export function parseAvatarRequest(text: string): AvatarRequest | undefined {
  const line = text.trim()
  if (!line || line.length > 400 || line.includes('\n')) return undefined
  for (const [i, re] of ZH_REQUESTS.entries()) {
    const m = re.exec(line)
    if (!m) continue
    const desc = clean(m[1] ?? '')
    if (!desc) continue
    // 变成/化身/变身 want a real description, not one character.
    if (i === 2 && [...desc].length < 2) continue
    return { desc, lang: 'zh' }
  }
  for (const [i, re] of EN_REQUESTS.entries()) {
    const m = re.exec(line)
    if (!m) continue
    const desc = clean(m[1] ?? '')
    if (!desc) continue
    if (i === 2 && !CHARACTER_WORDS.test(desc)) continue
    return { desc, lang: 'en' }
  }
  return undefined
}

function clean(raw: string): string {
  const desc = raw.replace(TRAILING, '').replace(ARTICLE, '').trim()
  if (!desc) return ''
  return [...desc].length > 200 ? [...desc].slice(0, 200).join('') : desc
}

export type AvatarChoice = { kind: 'pick'; index: number } | { kind: 'regenerate' } | { kind: 'none' }

const REGENERATE = /^(重新生成|再来一组|再生成|换一批|都不喜欢|都不好|都不要|再来四个|重来|regenerate|try again|another set|none of (?:these|them)|new options)[。.!！]?$/iu
const ZH_ORDINAL = /第([一二两三四1234])(?:个|只|张|款|号)?/u
const BARE = /^\s*([1-4])\s*[。.!！]?$/u
const EN_NUMBERED = /\b(?:option|number|no\.|#)\s*([1-4])\b/iu
const EN_ORDINALS: [RegExp, number][] = [
  [/\b(?:the\s+)?first(?:\s+one)?\b/iu, 1],
  [/\b(?:the\s+)?second(?:\s+one)?\b/iu, 2],
  [/\b(?:the\s+)?third(?:\s+one)?\b/iu, 3],
  [/\b(?:the\s+)?(?:fourth|last)(?:\s+one)?\b/iu, 4],
]
const CORNERS: [RegExp, number][] = [
  [/左上|top[\s-]?left|upper[\s-]?left/iu, 1],
  [/右上|top[\s-]?right|upper[\s-]?right/iu, 2],
  [/左下|bottom[\s-]?left|lower[\s-]?left/iu, 3],
  [/右下|bottom[\s-]?right|lower[\s-]?right/iu, 4],
]
const ZH_DIGITS: Record<string, number> = { 一: 1, 二: 2, 两: 2, 三: 3, 四: 4, '1': 1, '2': 2, '3': 3, '4': 4 }

/** While four candidates are on screen: which one the words mean, 1-based, or a wish for a new set. */
export function parseAvatarChoice(text: string): AvatarChoice {
  const line = text.trim()
  if (!line || line.length > 80) return { kind: 'none' }
  if (REGENERATE.test(line)) return { kind: 'regenerate' }
  const zh = ZH_ORDINAL.exec(line)
  if (zh?.[1]) return { kind: 'pick', index: ZH_DIGITS[zh[1]] ?? 1 }
  const bare = BARE.exec(line)
  if (bare?.[1]) return { kind: 'pick', index: Number(bare[1]) }
  const en = EN_NUMBERED.exec(line)
  if (en?.[1]) return { kind: 'pick', index: Number(en[1]) }
  for (const [re, index] of EN_ORDINALS) if (re.test(line)) return { kind: 'pick', index }
  for (const [re, index] of CORNERS) if (re.test(line)) return { kind: 'pick', index }
  return { kind: 'none' }
}

/** The card the phone writes into the transcript when a face is adopted. */
export interface AvatarFence {
  desc: string
  /** 1-based, the candidate that was kept. */
  chosen: number
  files: string[]
}

export const AVATAR_FENCE = 'nanomuse-avatar'

export function avatarFence(fence: AvatarFence): string {
  return '```' + AVATAR_FENCE + '\n' + JSON.stringify({ desc: fence.desc, chosen: fence.chosen, files: fence.files }) + '\n```'
}

/** The memory line both apps leave: `- 2026-10-04: the user changed my avatar to "a fox".` */
export function avatarMemoryLine(desc: string, at = new Date()): string {
  const day = `${at.getFullYear()}-${String(at.getMonth() + 1).padStart(2, '0')}-${String(at.getDate()).padStart(2, '0')}`
  return `- ${day}: the user changed my avatar to "${desc.replace(/"/g, '”')}".`
}
