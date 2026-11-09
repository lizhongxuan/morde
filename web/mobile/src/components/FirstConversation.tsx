import { Check, Sparkles } from "lucide-react";
import { useState } from "react";
import { useT } from "../i18n";
import { cx } from "../util";

/**
 * The first conversation's pieces in the chat (contract C4; `nanomuse/server/firstrun.py`
 * owns the state). The app speaks three lines first, as the agent, at no cost in tokens;
 * the model then asks the person's name and, when it has it, the chooser offers two names
 * for the agent — the model's suggestions or two from the built-in pool — and "Something
 * else" for a name of the person's own: an input on the card, saved by the pick endpoint
 * the way a chip is, never waiting on the model to repeat the name in a `nanomuse-naming`
 * block. The desktop (`firstrun.ts`) and the phones draw the same card.
 */

/** The opening lines, drawn as the agent's grey bubbles; they are not messages the model sees. */
export function IntroLines({ lines }: { lines: string[] }) {
  return (
    <div className="flex items-end gap-2 pr-10">
      <div className="min-w-0 max-w-full">
        {lines.map((line, i) => (
          <div key={i} className="my-1 max-w-[340px] rounded-[20px] bg-surface-2 px-3.5 py-2.5 text-[14.5px] leading-snug whitespace-pre-wrap break-words wide:max-w-[600px]">
            {line}
          </div>
        ))}
      </div>
    </div>
  );
}

/** The chooser under the model's latest reply: two names, "Something else" with an input
 * for a name of the person's own, and the chosen one ticked. */
export function NamingCard({
  chips,
  chosen,
  busy,
  onPick,
}: {
  chips: string[];
  chosen: string | null;
  busy?: boolean;
  onPick: (name: string) => void;
}) {
  const t = useT();
  const [editing, setEditing] = useState(false);
  const [value, setValue] = useState("");
  if (chips.length === 0 && !chosen && !editing) return null;
  const typed = value.trim();
  return (
    <section className="rise mx-1 my-2 rounded-[22px] border border-border/70 bg-surface p-3.5">
      <div className="flex items-center gap-2 text-[13px] font-medium text-fg">
        <Sparkles size={14} className="text-accent" /> {t("Pick a name for me, or type one")}
      </div>
      <div className="mt-2.5 flex flex-wrap gap-2">
        {chips.map((name) => {
          const picked = chosen === name;
          return (
            <button
              key={name}
              type="button"
              disabled={busy || !!chosen}
              onClick={() => onPick(name)}
              className={cx(
                "inline-flex items-center gap-1.5 rounded-full px-3.5 py-1.5 text-[13.5px] font-medium transition-colors disabled:opacity-60",
                picked ? "bg-accent text-accent-fg" : "bg-surface-2 hover:bg-border/60",
              )}
            >
              {picked && <Check size={13} strokeWidth={3} />}
              {name}
            </button>
          );
        })}
        {!chosen && !editing && (
          <button type="button" disabled={busy} onClick={() => setEditing(true)} className="rounded-full border border-border px-3.5 py-1.5 text-[13.5px] text-muted hover:bg-surface-2 disabled:opacity-60">
            {t("Something else")}
          </button>
        )}
      </div>
      {!chosen && editing && (
        <form
          className="mt-2.5 flex gap-2"
          onSubmit={(e) => {
            e.preventDefault();
            if (typed) onPick(typed);
          }}
        >
          <input
            autoFocus
            value={value}
            maxLength={16}
            onChange={(e) => setValue(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Escape") {
                setEditing(false);
                setValue("");
              }
            }}
            placeholder={t("Type a name for me")}
            className="min-w-0 flex-1 rounded-full border border-border bg-surface-2 px-3.5 py-1.5 text-[13.5px] text-fg outline-none focus:border-accent"
          />
          <button
            type="submit"
            disabled={busy || !typed}
            className="rounded-full bg-accent px-3.5 py-1.5 text-[13.5px] font-medium text-accent-fg disabled:opacity-60"
          >
            {t("Call me this")}
          </button>
          <button
            type="button"
            onClick={() => {
              setEditing(false);
              setValue("");
            }}
            className="rounded-full border border-border px-3.5 py-1.5 text-[13.5px] text-muted hover:bg-surface-2"
          >
            {t("Back")}
          </button>
        </form>
      )}
    </section>
  );
}
