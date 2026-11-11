import { useEffect, useId, useRef, useState } from 'react';
import { MordeIcon } from './MordeIcon';

type SpeechAlternativeLike = { transcript?: string };
type SpeechResultLike = ArrayLike<SpeechAlternativeLike> & { isFinal?: boolean };
type SpeechResultEventLike = { resultIndex?: number; results: ArrayLike<SpeechResultLike> };
type SpeechErrorEventLike = { error?: string };

interface SpeechRecognitionLike {
  lang: string;
  continuous: boolean;
  interimResults: boolean;
  onresult: ((event: SpeechResultEventLike) => void) | null;
  onerror: ((event: SpeechErrorEventLike) => void) | null;
  onend: (() => void) | null;
  start(): void;
  stop(): void;
  abort(): void;
}

type SpeechRecognitionConstructor = new () => SpeechRecognitionLike;

type SpeechRecognitionWindow = Window & {
  SpeechRecognition?: SpeechRecognitionConstructor;
  webkitSpeechRecognition?: SpeechRecognitionConstructor;
};

export type AssistantVoiceInputProps = {
  onTranscript: (transcript: string) => void;
  disabled?: boolean;
  language?: string;
  compact?: boolean;
};

function speechRecognitionConstructor(): SpeechRecognitionConstructor | undefined {
  if (typeof window === 'undefined') return undefined;
  const supportedWindow = window as SpeechRecognitionWindow;
  return supportedWindow.SpeechRecognition ?? supportedWindow.webkitSpeechRecognition;
}

function speechErrorMessage(error: string | undefined): string {
  switch (error) {
    case 'not-allowed':
    case 'service-not-allowed':
      return '未获得麦克风权限，请允许浏览器使用麦克风。';
    case 'audio-capture':
      return '未检测到可用麦克风。';
    case 'no-speech':
      return '没有听到语音，请重试。';
    case 'network':
      return '浏览器语音识别暂不可用，请稍后重试。';
    case 'aborted':
      return '';
    default:
      return '语音输入失败，请重试。';
  }
}

/**
 * Browser-native speech-to-text control. Morde does not read or upload raw
 * audio; recognition is delegated to the browser implementation, whose
 * processing location depends on the browser and its settings.
 */
export function AssistantVoiceInput({ onTranscript, disabled = false, language = 'zh-CN', compact = false }: AssistantVoiceInputProps) {
  const disclosureID = useId();
  const recognitionRef = useRef<SpeechRecognitionLike | null>(null);
  const [listening, setListening] = useState(false);
  const [status, setStatus] = useState('');

  useEffect(() => () => {
    const recognition = recognitionRef.current;
    recognitionRef.current = null;
    recognition?.abort();
  }, []);

  const toggleRecognition = () => {
    if (listening) {
      recognitionRef.current?.stop();
      setListening(false);
      setStatus('');
      return;
    }

    const Constructor = speechRecognitionConstructor();
    if (!Constructor) {
      setStatus('当前浏览器不支持语音输入。');
      return;
    }

    setStatus('');
    let activeRecognition: SpeechRecognitionLike | null = null;
    try {
      const recognition = new Constructor();
      activeRecognition = recognition;
      recognition.lang = language;
      recognition.continuous = true;
      recognition.interimResults = false;
      recognition.onresult = (event) => {
        if (recognitionRef.current !== recognition) return;
        const firstResult = Math.max(0, event.resultIndex ?? 0);
        for (let index = firstResult; index < event.results.length; index += 1) {
          const result = event.results[index];
          const transcript = result?.[0]?.transcript?.trim();
          if (result?.isFinal && transcript) onTranscript(transcript);
        }
      };
      recognition.onerror = (event) => {
        if (recognitionRef.current !== recognition) return;
        const message = speechErrorMessage(event.error);
        if (message) setStatus(message);
        setListening(false);
        recognitionRef.current = null;
      };
      recognition.onend = () => {
        if (recognitionRef.current !== recognition) return;
        setListening(false);
        recognitionRef.current = null;
      };
      recognitionRef.current = recognition;
      recognition.start();
      if (recognitionRef.current === recognition) {
        setListening(true);
        setStatus('正在聆听…');
      }
    } catch {
      if (activeRecognition && recognitionRef.current === activeRecognition) recognitionRef.current = null;
      setListening(false);
      setStatus('无法启动语音输入，请检查麦克风权限后重试。');
    }
  };

  return (
    <div className="assistant-voice-control" style={{ display: 'flex', alignItems: 'center', gap: 8, minWidth: 0 }}>
      <button
        type="button"
        className={`button button-secondary button-small${compact ? ' assistant-voice-button-compact' : ''}`}
        aria-label={listening ? '停止语音输入' : '开始语音输入'}
        aria-describedby={disclosureID}
        aria-pressed={listening}
        title="语音识别由浏览器提供，可能使用在线服务。"
        disabled={disabled && !listening}
        onClick={toggleRecognition}
        style={compact ? undefined : { minWidth: 76, justifyContent: 'center' }}
      >
        {compact ? <MordeIcon name="microphone" /> : listening ? '停止语音' : '语音输入'}
      </button>
      <span
        className={`assistant-voice-status${compact && !status ? ' assistant-voice-status-idle' : ''}`}
        aria-label="语音输入状态"
        aria-live="polite"
        aria-atomic="true"
        title={status || undefined}
        style={{ display: 'inline-block', flex: '0 1 172px', width: 172, height: '1.2em', minWidth: 0, maxWidth: 172, overflow: 'hidden',
          lineHeight: 1.2, whiteSpace: 'nowrap', textOverflow: 'ellipsis' }}
      >
        {status || '\u00a0'}
      </span>
      <span id={disclosureID} className="sr-only">语音识别由浏览器提供，处理位置取决于浏览器和设置，可能使用在线服务。仅在点击按钮后开始。</span>
    </div>
  );
}
