/**
 * What a tool call is doing, in a line a person can read: the model's own words for the
 * step when it gave them (`step` or `description`), else the call's main argument (the
 * command, the path, the query). The rooms' activity record and the presence the other
 * devices see (`SyncEngine.step`, 0.31) read the same line.
 */
export function parseArgs(raw: string): Record<string, unknown> {
  try {
    const parsed: unknown = JSON.parse(raw)
    return parsed && typeof parsed === 'object' && !Array.isArray(parsed) ? (parsed as Record<string, unknown>) : {}
  } catch {
    return {}
  }
}

/** The model's own words for a step, where the tool takes them: dsh's bash, pwsh and run_code
 * (`description`), the runtime's tools over MCP (`step`). Empty when it gave none. */
export function ownWords(raw: string): string {
  const args = parseArgs(raw)
  for (const key of ['step', 'description']) {
    const v = args[key]
    if (typeof v === 'string' && v.trim()) return v.replace(/\s+/g, ' ').trim()
  }
  return ''
}

export function describeCall(name: string, raw: string): string {
  const args = parseArgs(raw)
  const s = (key: string) => (typeof args[key] === 'string' ? (args[key] as string).replace(/\s+/g, ' ').trim() : '')
  const short = (v: string, n = 80) => (v.length > n ? `${v.slice(0, n - 1)}…` : v)
  const base = name.replace(/^mcp__[^_]+(?:_[^_]+)*?__/, '')
  const own = ownWords(raw)
  if (own) return short(own)
  switch (base) {
    case 'bash':
    case 'pwsh':
      return short(s('command') || s('cmd'))
    case 'read_file':
    case 'write_file':
    case 'edit_file':
    case 'str_replace_editor':
    case 'create_file':
      return short(s('path') || s('file_path') || s('file'))
    case 'web_search':
      return short(s('query'))
    case 'web_fetch':
      return short(s('url'))
    case 'glob':
    case 'grep':
      return short(s('pattern'))
    case 'schedule_create':
      return short(s('title') || s('prompt'))
    case 'ask_user_question':
      return short(String((args.questions as { question?: string }[] | undefined)?.[0]?.question ?? ''))
    case 'present':
      return short(String((args.files as { path?: string }[] | undefined)?.map((f) => f.path).join(', ') ?? ''))
    default: {
      const first = Object.values(args).find((v) => typeof v === 'string' && v.trim())
      return short(typeof first === 'string' ? first : '')
    }
  }
}
