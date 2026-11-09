import { useEffect, useId, useRef, useState } from 'react';

type SpeechUtteranceLike = {
  lang: string;
  onend: (() => void) | null;
  onerror: ((event: { error?: string }) => void) | null;
};

type SpeechSynthesisLike = {
  speak(utterance: SpeechUtteranceLike): void;
  cancel(): void;
};

type SpeechUtteranceConstructor = new (text?: string) => SpeechUtteranceLike;

type SpeechWindow = Window & {
  speechSynthesis?: SpeechSynthesisLike;
  SpeechSynthesisUtterance?: SpeechUtteranceConstructor;
};

type ActiveSpeech = {
  token: symbol;
  cancel: () => void;
};

export type AssistantReplySpeechProps = {
  text: string;
  sessionKey: string;
  language?: string;
};

let activeSpeech: ActiveSpeech | null = null;

function speechAPI(): { synthesis: SpeechSynthesisLike; Utterance: SpeechUtteranceConstructor } | null {
  if (typeof window === 'undefined') return null;
  const speechWindow = window as SpeechWindow;
  if (!speechWindow.speechSynthesis || !speechWindow.SpeechSynthesisUtterance) return null;
  return { synthesis: speechWindow.speechSynthesis, Utterance: speechWindow.SpeechSynthesisUtterance };
}

function cancelActiveSpeech(expectedToken?: symbol) {
  const current = activeSpeech;
  if (!current || (expectedToken && current.token !== expectedToken)) return;
  activeSpeech = null;
  current.cancel();
}

function speechErrorMessage(error: string | undefined): string {
  switch (error) {
    case 'not-allowed':
      return '系统语音权限被拒绝。';
    case 'voice-unavailable':
    case 'language-unavailable':
      return '当前系统语音不可用。';
    case 'network':
      return '系统语音服务暂不可用。';
    case 'canceled':
    case 'interrupted':
      return '';
    default:
      return '朗读失败，请重试。';
  }
}

/** Explicitly reads a final assistant reply using the browser/OS speech service. */
export function AssistantReplySpeech({ text, sessionKey, language = 'zh-CN' }: AssistantReplySpeechProps) {
  const disclosureID = useId();
  const tokenRef = useRef<symbol | null>(null);
  const utteranceRef = useRef<SpeechUtteranceLike | null>(null);
  const [speaking, setSpeaking] = useState(false);
  const [status, setStatus] = useState('');

  useEffect(() => () => {
    const token = tokenRef.current;
    if (token) cancelActiveSpeech(token);
  }, [sessionKey, text]);

  const stop = () => {
    const token = tokenRef.current;
    if (token) cancelActiveSpeech(token);
    else setSpeaking(false);
  };

  const start = () => {
    const api = speechAPI();
    if (!api) {
      setStatus('当前浏览器不支持系统语音朗读。');
      return;
    }

    cancelActiveSpeech();
    setStatus('');
    try {
      const utterance = new api.Utterance(text);
      const token = Symbol('assistant-reply-speech');
      utterance.lang = language;
      tokenRef.current = token;
      utteranceRef.current = utterance;
      activeSpeech = {
        token,
        cancel: () => {
          if (tokenRef.current === token) {
            tokenRef.current = null;
            utteranceRef.current = null;
            setSpeaking(false);
            setStatus('');
          }
          try { api.synthesis.cancel(); } catch { /* the browser may have already closed the speech session */ }
        },
      };
      utterance.onend = () => {
        if (activeSpeech?.token !== token) return;
        activeSpeech = null;
        tokenRef.current = null;
        utteranceRef.current = null;
        setSpeaking(false);
        setStatus('');
      };
      utterance.onerror = (event) => {
        if (activeSpeech?.token !== token) return;
        activeSpeech = null;
        tokenRef.current = null;
        utteranceRef.current = null;
        setSpeaking(false);
        setStatus(speechErrorMessage(event.error));
      };
      setSpeaking(true);
      api.synthesis.speak(utterance);
    } catch {
      const token = tokenRef.current;
      if (token) cancelActiveSpeech(token);
      setSpeaking(false);
      setStatus('无法启动系统语音朗读。');
    }
  };

  if (!text.trim()) return null;

  return (
    <div className="assistant-reply-speech" style={{ display: 'flex', alignItems: 'center', gap: 7, minHeight: 28 }}>
      <button
        type="button"
        className="button button-small button-secondary"
        aria-label={speaking ? '停止朗读回复' : '朗读回复'}
        aria-describedby={disclosureID}
        aria-pressed={speaking}
        title="语音由系统或浏览器合成，可能在线处理；Morde 不上传音频。"
        onClick={speaking ? stop : start}
        style={{ minWidth: 68, justifyContent: 'center' }}
      >
        {speaking ? '停止朗读' : '朗读回复'}
      </button>
      <span
        aria-label="朗读状态"
        aria-live="polite"
        aria-atomic="true"
        title={status || undefined}
        style={{ display: 'inline-block', flex: '0 1 172px', width: 172, height: '1.2em', minWidth: 0, maxWidth: 172,
          overflow: 'hidden', lineHeight: 1.2, whiteSpace: 'nowrap', textOverflow: 'ellipsis' }}
      >
        {status || '\u00a0'}
      </span>
      <span id={disclosureID} className="sr-only">
        朗读由系统或浏览器语音合成；处理方式取决于设备设置，可能在线处理。Morde 不上传音频，也不调用外部语音服务。
      </span>
    </div>
  );
}
