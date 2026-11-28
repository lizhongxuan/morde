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
  onstart: (() => void) | null;
  onend: (() => void) | null;
  start(): void;
  stop(): void;
  abort(): void;
}

type SpeechRecognitionConstructor = new () => SpeechRecognitionLike;

type VoiceWaveformBridge = {
  voiceWaveformEnabled?: boolean;
  beginVoiceRecording?: () => Promise<unknown>;
  endVoiceRecording?: (token: string) => Promise<unknown>;
};

type SpeechRecognitionWindow = Window & {
  SpeechRecognition?: SpeechRecognitionConstructor;
  webkitSpeechRecognition?: SpeechRecognitionConstructor;
  AudioContext?: typeof AudioContext;
  webkitAudioContext?: typeof AudioContext;
  morde?: VoiceWaveformBridge;
};

type WaveformCapture = {
  generation: number;
  bridge: VoiceWaveformBridge;
  token: string | null;
  tokenReleased: boolean;
  stream: MediaStream | null;
  context: AudioContext | null;
  source: MediaStreamAudioSourceNode | null;
  analyser: AnalyserNode | null;
  data: Uint8Array<ArrayBuffer> | null;
  frame: number | null;
  contextResume: Promise<void> | null;
  disposed: boolean;
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

function localVoiceWaveformBridge(): VoiceWaveformBridge | undefined {
  if (typeof window === 'undefined' || window.location.pathname !== '/assistant') return undefined;
  return (window as SpeechRecognitionWindow).morde;
}

function releaseVoiceRecordingLease(capture: WaveformCapture): void {
  if (!capture.token || capture.tokenReleased || !capture.bridge.endVoiceRecording) return;
  capture.tokenReleased = true;
  void capture.bridge.endVoiceRecording(capture.token).catch(() => undefined);
}

function disposeWaveformCapture(capture: WaveformCapture | null): void {
  if (!capture || capture.disposed) return;
  capture.disposed = true;
  if (capture.frame !== null) cancelAnimationFrame(capture.frame);
  try { capture.source?.disconnect(); } catch { /* The source may already have been disconnected. */ }
  try { capture.analyser?.disconnect(); } catch { /* The analyser may already have been disconnected. */ }
  capture.stream?.getTracks().forEach((track) => track.stop());
  if (capture.context && capture.context.state !== 'closed') void capture.context.close().catch(() => undefined);
  releaseVoiceRecordingLease(capture);
}

/**
 * Browser-native speech-to-text control. Morde does not read or upload raw
 * samples for the visual meter; Web Audio derives only a live level. Speech
 * recognition is delegated to the browser implementation, whose processing
 * location depends on the browser and its settings.
 */
export function AssistantVoiceInput({ onTranscript, disabled = false, language = 'zh-CN', compact = false }: AssistantVoiceInputProps) {
  const disclosureID = useId();
  const recognitionRef = useRef<SpeechRecognitionLike | null>(null);
  const waveformCaptureRef = useRef<WaveformCapture | null>(null);
  const waveformGenerationRef = useRef(0);
  const recognitionStartedRef = useRef(false);
  const [listening, setListening] = useState(false);
  const [waveformLevel, setWaveformLevel] = useState(0);
  const [waveformVisible, setWaveformVisible] = useState(false);
  const [status, setStatus] = useState('');

  useEffect(() => () => {
    const recognition = recognitionRef.current;
    recognitionRef.current = null;
    try { recognition?.abort(); } catch { /* The recognizer may still be preparing to start. */ }
    recognitionStartedRef.current = false;
    waveformGenerationRef.current += 1;
    const capture = waveformCaptureRef.current;
    waveformCaptureRef.current = null;
    disposeWaveformCapture(capture);
  }, []);

  useEffect(() => {
    if (!disabled) return;
    const recognition = recognitionRef.current;
    recognitionRef.current = null;
    try { recognition?.abort(); } catch { /* The recognizer may still be preparing to start. */ }
    recognitionStartedRef.current = false;
    waveformGenerationRef.current += 1;
    const capture = waveformCaptureRef.current;
    waveformCaptureRef.current = null;
    disposeWaveformCapture(capture);
    setListening(false);
    setWaveformVisible(false);
    setWaveformLevel(0);
  }, [disabled]);

  const stopWaveform = (expected?: WaveformCapture) => {
    if (expected && waveformCaptureRef.current !== expected) {
      disposeWaveformCapture(expected);
      return;
    }
    waveformGenerationRef.current += 1;
    const capture = waveformCaptureRef.current;
    waveformCaptureRef.current = null;
    disposeWaveformCapture(capture);
    setWaveformVisible(false);
    setWaveformLevel(0);
  };

  const startWaveform = (recognition: SpeechRecognitionLike, capture: WaveformCapture) => {
    const bridge = capture.bridge;
    if (!bridge.voiceWaveformEnabled || !capture.token) return;
    if (!capture.context || !navigator.mediaDevices?.getUserMedia) {
      stopWaveform(capture);
      return;
    }
    const isCurrent = () => waveformCaptureRef.current === capture &&
      waveformGenerationRef.current === capture.generation && recognitionRef.current === recognition && !capture.disposed;

    void (async () => {
      try {
        const stream = await navigator.mediaDevices.getUserMedia({ audio: true, video: false });
        if (!isCurrent()) {
          stream.getTracks().forEach((track) => track.stop());
          return;
        }
        capture.stream = stream;
        const context = capture.context;
        if (!context) throw new Error('Audio context is unavailable');
        const source = context.createMediaStreamSource(stream);
        const analyser = context.createAnalyser();
        analyser.fftSize = 256;
        source.connect(analyser);
        capture.source = source;
        capture.analyser = analyser;
        capture.data = new Uint8Array(new ArrayBuffer(analyser.fftSize));
        await capture.contextResume;
        if (context.state === 'suspended') await context.resume();
        if (!isCurrent() || context.state !== 'running') throw new Error('Audio level meter is unavailable');

        setWaveformVisible(true);
        const updateLevel = () => {
          if (!isCurrent() || !capture.analyser || !capture.data) return;
          capture.analyser.getByteTimeDomainData(capture.data);
          let sumSquares = 0;
          for (const sample of capture.data) {
            const amplitude = (sample - 128) / 128;
            sumSquares += amplitude * amplitude;
          }
          setWaveformLevel(Math.min(1, Math.sqrt(sumSquares / capture.data.length) * 2.5));
          capture.frame = requestAnimationFrame(updateLevel);
        };
        updateLevel();
      } catch {
        if (!isCurrent()) {
          disposeWaveformCapture(capture);
          return;
        }
        stopWaveform(capture);
      }
    })();
  };

  const prepareVoiceRecording = (recognition: SpeechRecognitionLike, bridge: VoiceWaveformBridge) => {
    if (!bridge.beginVoiceRecording || !bridge.endVoiceRecording) return false;
    waveformGenerationRef.current += 1;
    const capture: WaveformCapture = {
      generation: waveformGenerationRef.current, bridge, token: null, tokenReleased: false,
      stream: null, context: null, source: null, analyser: null, data: null, frame: null,
      contextResume: null, disposed: false,
    };
    waveformCaptureRef.current = capture;
    const isCurrent = () => waveformCaptureRef.current === capture &&
      waveformGenerationRef.current === capture.generation && recognitionRef.current === recognition && !capture.disposed;
    const supportedWindow = typeof window === 'undefined' ? undefined : window as SpeechRecognitionWindow;
    const AudioContextConstructor = supportedWindow?.AudioContext ?? supportedWindow?.webkitAudioContext;
    if (bridge.voiceWaveformEnabled && AudioContextConstructor) {
      try {
        capture.context = new AudioContextConstructor();
        // Start the audio context in the click call stack to satisfy autoplay rules.
        capture.contextResume = capture.context.resume().catch(() => undefined);
      } catch { /* Speech recognition can still run without a visual meter. */ }
    }

    setListening(true);
    setStatus('正在准备麦克风…');
    void (async () => {
      try {
        const token = await bridge.beginVoiceRecording!();
        if (typeof token !== 'string' || token.length < 1) throw new Error('Invalid voice recording lease');
        capture.token = token;
        if (!isCurrent()) {
          releaseVoiceRecordingLease(capture);
          disposeWaveformCapture(capture);
          return;
        }
        recognition.start();
        recognitionStartedRef.current = true;
        // The browser may never emit SpeechRecognition's onstart event even
        // though start() returned. Start the opt-in local meter independently.
        if (bridge.voiceWaveformEnabled) startWaveform(recognition, capture);
        setStatus('正在聆听…');
      } catch {
        if (!isCurrent()) {
          releaseVoiceRecordingLease(capture);
          disposeWaveformCapture(capture);
          return;
        }
        recognitionRef.current = null;
        recognitionStartedRef.current = false;
        stopWaveform(capture);
        setListening(false);
        setStatus('无法启动语音输入，请检查麦克风权限后重试。');
      }
    })();
    return true;
  };

  const toggleRecognition = () => {
    if (listening) {
      const recognition = recognitionRef.current;
      const recognitionStarted = recognitionStartedRef.current;
      stopWaveform();
      if (recognitionStarted) recognition?.stop();
      else {
        recognitionRef.current = null;
        try { recognition?.abort(); } catch { /* The recognizer may still be preparing to start. */ }
      }
      recognitionStartedRef.current = false;
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
        stopWaveform();
        setListening(false);
        recognitionRef.current = null;
        recognitionStartedRef.current = false;
      };
      recognition.onend = () => {
        if (recognitionRef.current !== recognition) return;
        stopWaveform();
        setListening(false);
        recognitionRef.current = null;
        recognitionStartedRef.current = false;
      };
      recognitionRef.current = recognition;
      const bridge = localVoiceWaveformBridge();
      if (bridge && prepareVoiceRecording(recognition, bridge)) return;
      recognition.start();
      if (recognitionRef.current === recognition) {
        recognitionStartedRef.current = true;
        setListening(true);
        setStatus('正在聆听…');
      }
    } catch {
      if (activeRecognition && recognitionRef.current === activeRecognition) recognitionRef.current = null;
      recognitionStartedRef.current = false;
      stopWaveform();
      setListening(false);
      setStatus('无法启动语音输入，请检查麦克风权限后重试。');
    }
  };

  const cancelRecognition = () => {
    const recognition = recognitionRef.current;
    // Invalidate the active generation before abort can synchronously emit
    // result/error/end callbacks. Results already delivered stay in the draft.
    recognitionRef.current = null;
    try { recognition?.abort(); } catch { /* The recognizer may still be preparing to start. */ }
    recognitionStartedRef.current = false;
    stopWaveform();
    setListening(false);
    setStatus('语音输入已取消。');
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
      {listening && <button
        type="button"
        className="button button-secondary button-small assistant-voice-cancel"
        aria-label="取消语音输入"
        onClick={cancelRecognition}
      >取消</button>}
      {waveformVisible && <span className="assistant-voice-waveform" aria-hidden="true" style={{ display: 'inline-flex', alignItems: 'center', gap: 2, height: 18 }}>
        {[0.38, 0.64, 0.9, 0.64, 0.38].map((weight, index) => <span key={index} style={{ display: 'block', width: 2, height: `${Math.max(2, waveformLevel * 16 * weight)}px`, borderRadius: 2, background: 'currentColor' }} />)}
      </span>}
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
      <span id={disclosureID} className="sr-only">语音识别由浏览器提供，处理位置取决于浏览器和设置，可能使用在线服务。仅在点击按钮后开始。波形功能仅在本机启用时读取实时音量，不保存或上传麦克风采样。</span>
    </div>
  );
}
