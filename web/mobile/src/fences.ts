/**
 * Fenced blocks the model writes for the app, not for the person (contract C4).
 *
 * The runtime reads them (`nanomuse/fences.py`); the browser only has to hide them. The
 * one tag today is ```` ```nanomuse-naming ````: the first conversation's report of the
 * name the person gave and the name the agent chose. The desktop (`fences.ts`) and the
 * phones strip the same way.
 */

export const FENCE_NAMING = "nanomuse-naming";

function escapeTag(tag: string): string {
  return tag.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}

/**
 * The text without the tagged fences. Complete blocks go whole; an opener without its
 * closing fence takes the rest of the text with it, so the person never sees half a block.
 * While a reply is still streaming (`streaming`), a trailing piece of the opener itself
 * (`` ``` `` or `` ```nanomuse-na ``) goes too, so nothing flashes before the block is
 * recognised. Trailing whitespace the fence left goes; text without the tag is unchanged.
 */
export function stripFences(text: string, tag: string, streaming = false): string {
  const open = "```" + tag;
  let out = text;
  if (out.includes(open)) {
    const whole = new RegExp("```" + escapeTag(tag) + "[ \\t]*\\r?\\n[\\s\\S]*?```", "g");
    out = out.replace(whole, "");
    const opener = out.lastIndexOf(open);
    if (opener >= 0) out = out.slice(0, opener);
  }
  if (streaming) {
    const tail = out.lastIndexOf("```");
    if (tail >= 0 && open.startsWith(out.slice(tail))) out = out.slice(0, tail);
  }
  return out === text ? text : out.replace(/\s+$/, "");
}

/** `stripFences` for the naming block, the one the chat hides. */
export function stripNamingFence(text: string, streaming = false): string {
  return stripFences(text, FENCE_NAMING, streaming);
}
