import { Mic, MicOff } from "lucide-react";
import { useCallback, useEffect, useRef, useState } from "react";
import { useT } from "../i18n";
import { cx } from "../util";

/* Voice input for the composer: the browser's own speech recognition (Web Speech API),
 * where it exists — Chrome, Edge, Safari, the Android WebView; nothing is sent by us and no
 * model is involved. One tap starts listening, the words land in the box as they are
 * recognised, a second tap (or silence) stops. Browsers without it show no button. */

interface RecognitionLike {
  lang: string;
  continuous: boolean;
  interimResults: boolean;
  onresult: ((e: { resultIndex: number; results: ArrayLike<ArrayLike<{ transcript: string }> & { isFinal: boolean }> }) => void) | null;
  onend: (() => void) | null;
  onerror: ((e: { error?: string }) => void) | null;
  start(): void;
  stop(): void;
  abort(): void;
}

function recognitionClass(): (new () => RecognitionLike) | null {
  if (typeof window === "undefined") return null;
  const w = window as unknown as { SpeechRecognition?: new () => RecognitionLike; webkitSpeechRecognition?: new () => RecognitionLike };
  return w.SpeechRecognition ?? w.webkitSpeechRecognition ?? null;
}

export function dictationSupported(): boolean {
  return recognitionClass() !== null;
}

/** `onText(final, interim)`: the words recognised so far in this session — `final` is settled,
 * `interim` is the browser's current guess for the phrase still being spoken. */
export function useDictation(onText: (final: string, interim: string) => void, onError?: (message: string) => void) {
  const [listening, setListening] = useState(false);
  const rec = useRef<RecognitionLike | null>(null);
  const finalRef = useRef("");
  const t = useT();

  const stop = useCallback(() => {
    rec.current?.stop();
  }, []);

  const start = useCallback(() => {
    const Cls = recognitionClass();
    if (!Cls) return;
    const r = new Cls();
    r.lang = navigator.language || "en-US";
    r.continuous = true;
    r.interimResults = true;
    finalRef.current = "";
    r.onresult = (e) => {
      let interim = "";
      for (let i = e.resultIndex; i < e.results.length; i++) {
        const res = e.results[i];
        const text = res?.[0]?.transcript ?? "";
        if (res?.isFinal) finalRef.current += text;
        else interim += text;
      }
      onText(finalRef.current, interim);
    };
    r.onerror = (e) => {
      const code = e.error || "";
      if (code === "aborted" || code === "no-speech") return;
      onError?.(
        code === "not-allowed" || code === "service-not-allowed"
          ? t("The microphone is not allowed. Allow it in the browser and try again.")
          : t("Voice input stopped ({code}).", { code }),
      );
    };
    r.onend = () => {
      rec.current = null;
      setListening(false);
    };
    rec.current = r;
    try {
      r.start();
      setListening(true);
    } catch {
      rec.current = null;
      setListening(false);
    }
  }, [onText, onError, t]);

  const toggle = useCallback(() => {
    if (rec.current) stop();
    else start();
  }, [start, stop]);

  useEffect(() => () => rec.current?.abort(), []);

  return { supported: dictationSupported(), listening, toggle, stop };
}

export function MicButton({ listening, onClick, className }: { listening: boolean; onClick: () => void; className?: string }) {
  const t = useT();
  return (
    <button
      type="button"
      onClick={onClick}
      aria-label={listening ? t("Stop voice input") : t("Voice input")}
      aria-pressed={listening}
      className={cx(
        "flex h-9 w-9 shrink-0 items-center justify-center rounded-full transition active:scale-95",
        listening ? "bg-accent/15 text-accent animate-pulse" : "text-fg/70 hover:bg-surface-2",
        className,
      )}
    >
      {listening ? <MicOff size={19} /> : <Mic size={19} />}
    </button>
  );
}
