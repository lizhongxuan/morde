import type { ComponentPropsWithoutRef } from "react";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import { safeDecodeURIComponent } from "../util";

/**
 * Which of the files the agent made does this text name?
 *
 * A reply says "saved it to `kyoto-notes/packing-list.html`" or just "`packing-list.html`":
 * accept the workspace path, a path suffix, or the bare file name (when it is unambiguous).
 */
export function matchFile(text: string, files: readonly string[]): string | null {
  const t = text.trim().replace(/^\.?\//, "");
  if (!t || files.length === 0) return null;
  if (files.includes(t)) return t;
  const suffix = files.filter((f) => f.endsWith(`/${t}`));
  return suffix.length === 1 ? suffix[0] : null;
}

const escapeRe = (s: string) => s.replace(/[.*+?^${}()|[\]\\/-]/g, "\\$&");

/**
 * Put backticks around bare mentions of the thread's files, so "saved as packing-list.html"
 * gets the same tappable chip as "`packing-list.html`". Text already in code, fenced blocks
 * or links is left alone; a bare name is marked only when it names exactly one file.
 */
export function markFiles(text: string, files: readonly string[]): string {
  if (!text || files.length === 0) return text;
  const names = new Set<string>(files);
  for (const f of files) {
    const base = f.slice(f.lastIndexOf("/") + 1);
    if (base.includes(".") && matchFile(base, files) === f) names.add(base);
  }
  // longest first, so a path wins over the file name inside it
  const alts = [...names].sort((a, b) => b.length - a.length).map(escapeRe);
  const mention = new RegExp(`(?<![\\w./\\\\-])(${alts.join("|")})(?![\\w/-]|\\.\\w)`, "g");
  const untouched = /(```[\s\S]*?```|`[^`\n]*`|\[[^\]]*\]\([^)]*\)|<[^>]*>)/;
  return text
    .split(untouched)
    .map((part, i) => (i % 2 === 1 ? part : part.replace(mention, "`$1`")))
    .join("");
}

function inlineCode(props: ComponentPropsWithoutRef<"code">): boolean {
  // react-markdown gives fenced blocks a `language-*` class and a newline in the text
  const text = typeof props.children === "string" ? props.children : "";
  return !String(props.className ?? "").includes("language-") && !text.includes("\n");
}

/**
 * Muse breaks a long reply into several bubbles — one per top-level block (a paragraph, a list,
 * a heading with what follows it). Fenced code, tables and raw HTML stay whole and are marked
 * `bare`, since they carry their own frame. Blocks are split on blank lines outside fences.
 */
export function splitBlocks(text: string): Array<{ text: string; bare: boolean }> {
  const out: Array<{ text: string; bare: boolean }> = [];
  let buf: string[] = [];
  let fence: string | null = null;
  const flush = () => {
    const body = buf.join("\n").trim();
    buf = [];
    if (!body) return;
    const t = body.trimStart();
    const bare = t.startsWith("```") || t.startsWith("~~~") || t.startsWith("|") || t.startsWith("<");
    const last = out[out.length - 1];
    // a heading is the title of the block after it, not a bubble of its own
    if (last && !last.bare && /^#{1,6}\s/.test(last.text) && !last.text.includes("\n") && !bare) {
      last.text = `${last.text}\n\n${body}`;
      return;
    }
    out.push({ text: body, bare });
  };
  for (const line of text.split("\n")) {
    const open = /^\s*(```|~~~)/.exec(line);
    if (fence) {
      buf.push(line);
      if (open && open[1] === fence) fence = null;
      continue;
    }
    if (open) {
      if (buf.length) flush();
      fence = open[1];
      buf.push(line);
      continue;
    }
    if (line.trim() === "") {
      flush();
      continue;
    }
    buf.push(line);
  }
  flush();
  return out;
}

export function Markdown({
  text,
  files = [],
  onOpenFile,
}: {
  text: string;
  /** workspace paths of files this reply may refer to (the thread's artifacts) */
  files?: readonly string[];
  onOpenFile?: (path: string) => void;
}) {
  // which of the thread's files does this text name, if we can open it at all
  const fileFor = (text: string | undefined): string | null =>
    onOpenFile && text && files.length > 0 ? matchFile(text, files) : null;
  const open = (path: string) => onOpenFile?.(path);
  const source = onOpenFile && files.length > 0 ? markFiles(text, files) : text;
  return (
    <div className="md text-[15px] leading-[1.5] break-words">
      <ReactMarkdown
        remarkPlugins={[remarkGfm]}
        components={{
          a: ({ href, children, ...props }) => {
            const path = href && !/^[a-z]+:/i.test(href) ? fileFor(safeDecodeURIComponent(href)) : null;
            if (path) {
              return (
                <button type="button" className="md-file" onClick={() => open(path)}>
                  {children}
                </button>
              );
            }
            return (
              <a href={href} {...props} target="_blank" rel="noreferrer noopener">
                {children}
              </a>
            );
          },
          code: ({ children, ...props }) => {
            const path = inlineCode({ children, ...props }) && typeof children === "string" ? fileFor(children) : null;
            if (path) {
              return (
                <button type="button" className="md-file" onClick={() => open(path)} title={path}>
                  {children}
                </button>
              );
            }
            return <code {...props}>{children}</code>;
          },
        }}
      >
        {source}
      </ReactMarkdown>
    </div>
  );
}
