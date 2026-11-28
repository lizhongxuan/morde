import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { AssistantVoiceInput } from './AssistantVoiceInput';

class FakeRecognition {
  static latest: FakeRecognition | null = null;
  static startThrows = false;
  static omitStartEvent = false;
  static install: ((options: { langs: string[]; processLocally: true }) => Promise<boolean>) | undefined;

  lang = '';
  continuous = false;
  interimResults = true;
  processLocally: boolean | undefined;
  onresult: ((event: { resultIndex?: number; results: ArrayLike<{ isFinal?: boolean; 0?: { transcript?: string } }> }) => void) | null = null;
  onerror: ((event: { error?: string }) => void) | null = null;
  onstart: (() => void) | null = null;
  onend: (() => void) | null = null;
  start = vi.fn(() => {
    if (FakeRecognition.startThrows) throw new Error('start failed');
    if (!FakeRecognition.omitStartEvent) this.onstart?.();
  });
  stop = vi.fn();
  abort = vi.fn();

  constructor() {
    FakeRecognition.latest = this;
  }

  emitFinal(transcript: string) {
    this.onresult?.({ resultIndex: 0, results: [{ isFinal: true, 0: { transcript } }] });
  }

  emitResults(results: Array<{ isFinal: boolean; transcript: string }>, resultIndex = 0) {
    this.onresult?.({ resultIndex, results: results.map((result) => ({ isFinal: result.isFinal, 0: { transcript: result.transcript } })) });
  }
}

class FakeAudioContext {
  static latest: FakeAudioContext | null = null;
  state: 'running' | 'closed' = 'running';
  analyser = {
    fftSize: 256,
    getByteTimeDomainData: vi.fn((data: Uint8Array) => data.fill(160)),
    disconnect: vi.fn(),
  };
  source = { connect: vi.fn(), disconnect: vi.fn() };
  resume = vi.fn(async () => undefined);
  close = vi.fn(async () => { this.state = 'closed'; });
  createMediaStreamSource = vi.fn(() => this.source);
  createAnalyser = vi.fn(() => this.analyser);

  constructor() {
    FakeAudioContext.latest = this;
  }
}

type SpeechRecognitionWindow = Window & {
  SpeechRecognition?: typeof FakeRecognition;
  webkitSpeechRecognition?: typeof FakeRecognition;
  morde?: {
    voiceWaveformEnabled?: boolean;
    beginVoiceRecording?: () => Promise<unknown>;
    endVoiceRecording?: (token: string) => Promise<unknown>;
  };
};

const supportedWindow = window as unknown as SpeechRecognitionWindow;
const audioContextWindow = window as unknown as { AudioContext?: typeof AudioContext };
const navigatorWithOptionalMediaDevices = navigator as unknown as { mediaDevices?: MediaDevices };
let previousMediaDevices: PropertyDescriptor | undefined;

function fakeStream() {
  const track = { stop: vi.fn() };
  const stream = { getTracks: () => [track] } as unknown as MediaStream;
  return { stream, track };
}

function setMediaDevices(getUserMedia: (constraints: MediaStreamConstraints) => Promise<MediaStream>) {
  Object.defineProperty(navigator, 'mediaDevices', { configurable: true, value: { getUserMedia } });
}

describe('AssistantVoiceInput', () => {
  beforeEach(() => {
    FakeRecognition.latest = null;
    FakeRecognition.startThrows = false;
    FakeRecognition.omitStartEvent = false;
    FakeRecognition.install = undefined;
    FakeAudioContext.latest = null;
    previousMediaDevices = Object.getOwnPropertyDescriptor(navigator, 'mediaDevices');
    window.history.replaceState({}, '', '/assistant');
    supportedWindow.SpeechRecognition = FakeRecognition;
  });

  afterEach(() => {
    cleanup();
    delete supportedWindow.SpeechRecognition;
    delete supportedWindow.webkitSpeechRecognition;
    delete audioContextWindow.AudioContext;
    delete supportedWindow.morde;
    window.history.replaceState({}, '', '/');
    if (previousMediaDevices) Object.defineProperty(navigator, 'mediaDevices', previousMediaDevices);
    else delete navigatorWithOptionalMediaDevices.mediaDevices;
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  it('starts only after an explicit click and sends final text to the caller', () => {
    const onTranscript = vi.fn();
    render(<AssistantVoiceInput onTranscript={onTranscript} />);

    expect(FakeRecognition.latest).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: '开始语音输入' }));

    const recognition = FakeRecognition.latest;
    expect(recognition).not.toBeNull();
    expect(recognition?.start).toHaveBeenCalledOnce();
    expect(recognition?.lang).toBe('zh-CN');
    expect(recognition?.continuous).toBe(true);
    expect(recognition?.interimResults).toBe(false);
    expect(screen.getByLabelText('语音输入状态')).toHaveTextContent('正在聆听');

    act(() => recognition?.emitFinal('帮我总结这份文件'));
    expect(onTranscript).toHaveBeenCalledWith('帮我总结这份文件');
  });

  it('installs the local language pack and uses on-device recognition when supported', async () => {
    const install = vi.fn(async () => true);
    FakeRecognition.install = install;
    render(<AssistantVoiceInput onTranscript={vi.fn()} />);

    fireEvent.click(screen.getByRole('button', { name: '开始语音输入' }));

    await waitFor(() => expect(FakeRecognition.latest?.start).toHaveBeenCalledOnce());
    expect(install).toHaveBeenCalledWith({ langs: ['zh-CN'], processLocally: true });
    expect(FakeRecognition.latest?.processLocally).toBe(true);
  });

  it('stops the active recognition session from the same control', () => {
    render(<AssistantVoiceInput onTranscript={vi.fn()} />);
    fireEvent.click(screen.getByRole('button', { name: '开始语音输入' }));
    const recognition = FakeRecognition.latest;

    fireEvent.click(screen.getByRole('button', { name: '停止语音输入' }));

    expect(recognition?.stop).toHaveBeenCalledOnce();
    expect(screen.getByRole('button', { name: '开始语音输入' })).toHaveAttribute('aria-pressed', 'false');
  });

  it('keeps final recognition results after stopping normally', () => {
    const onTranscript = vi.fn();
    render(<AssistantVoiceInput onTranscript={onTranscript} />);
    fireEvent.click(screen.getByRole('button', { name: '开始语音输入' }));
    const recognition = FakeRecognition.latest;

    fireEvent.click(screen.getByRole('button', { name: '停止语音输入' }));
    act(() => recognition?.emitFinal('停止前的收尾文字'));

    expect(onTranscript).toHaveBeenCalledOnce();
    expect(onTranscript).toHaveBeenCalledWith('停止前的收尾文字');
  });

  it('invalidates the active generation before cancelling and preserves text already delivered', () => {
    const onTranscript = vi.fn();
    render(<AssistantVoiceInput onTranscript={onTranscript} />);
    fireEvent.click(screen.getByRole('button', { name: '开始语音输入' }));
    const recognition = FakeRecognition.latest;
    act(() => recognition?.emitFinal('已写入草稿的文字'));

    fireEvent.click(screen.getByRole('button', { name: '取消语音输入' }));
    expect(recognition?.abort).toHaveBeenCalledOnce();
    expect(screen.queryByRole('button', { name: '取消语音输入' })).not.toBeInTheDocument();
    expect(screen.getByLabelText('语音输入状态')).toHaveTextContent('语音输入已取消');

    act(() => {
      recognition?.emitFinal('取消后的迟到文字');
      recognition?.onerror?.({ error: 'aborted' });
      recognition?.onend?.();
    });

    expect(onTranscript).toHaveBeenCalledOnce();
    expect(onTranscript).toHaveBeenCalledWith('已写入草稿的文字');
    expect(screen.getByLabelText('语音输入状态')).toHaveTextContent('语音输入已取消');
  });

  it('ignores late events from a stopped session after a new one starts', () => {
    const onTranscript = vi.fn();
    render(<AssistantVoiceInput onTranscript={onTranscript} />);
    fireEvent.click(screen.getByRole('button', { name: '开始语音输入' }));
    const first = FakeRecognition.latest;

    fireEvent.click(screen.getByRole('button', { name: '停止语音输入' }));
    fireEvent.click(screen.getByRole('button', { name: '开始语音输入' }));
    const second = FakeRecognition.latest;
    expect(second).not.toBe(first);

    act(() => {
      first?.emitFinal('旧会话文字');
      first?.onend?.();
    });
    expect(onTranscript).not.toHaveBeenCalled();
    expect(screen.getByRole('button', { name: '停止语音输入' })).toHaveAttribute('aria-pressed', 'true');
    expect(screen.getByLabelText('语音输入状态')).toHaveTextContent('正在聆听');

    act(() => second?.emitFinal('新会话文字'));
    expect(onTranscript).toHaveBeenCalledOnce();
    expect(onTranscript).toHaveBeenCalledWith('新会话文字');
  });

  it('ignores callbacks from a cancelled session after recognition is reopened', () => {
    const onTranscript = vi.fn();
    render(<AssistantVoiceInput onTranscript={onTranscript} />);
    fireEvent.click(screen.getByRole('button', { name: '开始语音输入' }));
    const cancelled = FakeRecognition.latest;

    fireEvent.click(screen.getByRole('button', { name: '取消语音输入' }));
    fireEvent.click(screen.getByRole('button', { name: '开始语音输入' }));
    const reopened = FakeRecognition.latest;
    expect(reopened).not.toBe(cancelled);

    act(() => {
      cancelled?.emitFinal('取消后迟到的文字');
      cancelled?.onerror?.({ error: 'aborted' });
      cancelled?.onend?.();
    });

    expect(onTranscript).not.toHaveBeenCalled();
    expect(screen.getByRole('button', { name: '停止语音输入' })).toHaveAttribute('aria-pressed', 'true');
    expect(screen.getByLabelText('语音输入状态')).toHaveTextContent('正在聆听');

    act(() => reopened?.emitFinal('重新打开后的文字'));
    expect(onTranscript).toHaveBeenCalledOnce();
    expect(onTranscript).toHaveBeenCalledWith('重新打开后的文字');
  });

  it('emits only non-empty final results beginning at resultIndex', () => {
    const onTranscript = vi.fn();
    render(<AssistantVoiceInput onTranscript={onTranscript} />);
    fireEvent.click(screen.getByRole('button', { name: '开始语音输入' }));
    const recognition = FakeRecognition.latest;

    act(() => recognition?.emitResults([
      { isFinal: true, transcript: '先前的结果' },
      { isFinal: false, transcript: '未定稿' },
      { isFinal: true, transcript: '  ' },
      { isFinal: true, transcript: '  最终文字  ' },
    ], 1));

    expect(onTranscript).toHaveBeenCalledOnce();
    expect(onTranscript).toHaveBeenCalledWith('最终文字');
  });

  it('uses the WebKit-prefixed API and configured language', () => {
    delete supportedWindow.SpeechRecognition;
    supportedWindow.webkitSpeechRecognition = FakeRecognition;
    render(<AssistantVoiceInput onTranscript={vi.fn()} language="en-US" />);

    fireEvent.click(screen.getByRole('button', { name: '开始语音输入' }));

    expect(FakeRecognition.latest?.lang).toBe('en-US');
    expect(FakeRecognition.latest?.start).toHaveBeenCalledOnce();
  });

  it('does not start recognition when the control is disabled', () => {
    render(<AssistantVoiceInput onTranscript={vi.fn()} disabled />);

    expect(screen.getByRole('button', { name: '开始语音输入' })).toBeDisabled();
    expect(FakeRecognition.latest).toBeNull();
  });

  it('keeps the real microphone meter off when the local setting is absent', async () => {
    const getUserMedia = vi.fn(async () => fakeStream().stream);
    const beginVoiceRecording = vi.fn(async () => 'recognition-token');
    setMediaDevices(getUserMedia);
    audioContextWindow.AudioContext = FakeAudioContext as unknown as typeof AudioContext;
    supportedWindow.morde = { voiceWaveformEnabled: false, beginVoiceRecording, endVoiceRecording: vi.fn(async () => ({})) };
    render(<AssistantVoiceInput onTranscript={vi.fn()} />);

    fireEvent.click(screen.getByRole('button', { name: '开始语音输入' }));
    await waitFor(() => expect(FakeRecognition.latest?.start).toHaveBeenCalledOnce());

    expect(FakeRecognition.latest?.start).toHaveBeenCalledOnce();
    expect(beginVoiceRecording).toHaveBeenCalledOnce();
    expect(getUserMedia).not.toHaveBeenCalled();
    expect(document.querySelector('.assistant-voice-waveform')).toBeNull();
  });

  it('does not request the desktop microphone lease outside the Assistant route', () => {
    window.history.replaceState({}, '', '/projects');
    const beginVoiceRecording = vi.fn(async () => 'project-token');
    supportedWindow.morde = {
      voiceWaveformEnabled: true,
      beginVoiceRecording,
      endVoiceRecording: vi.fn(async () => ({ released: true })),
    };
    render(<AssistantVoiceInput onTranscript={vi.fn()} />);

    fireEvent.click(screen.getByRole('button', { name: '开始语音输入' }));

    expect(beginVoiceRecording).not.toHaveBeenCalled();
    expect(FakeRecognition.latest?.start).toHaveBeenCalledOnce();
    expect(document.querySelector('.assistant-voice-waveform')).toBeNull();
  });

  it('renders audio-derived levels and releases the microphone when stopped', async () => {
    const { stream, track } = fakeStream();
    const getUserMedia = vi.fn(async () => stream);
    const beginVoiceRecording = vi.fn(async () => 'audio-token');
    const endVoiceRecording = vi.fn(async () => ({ released: true }));
    setMediaDevices(getUserMedia);
    audioContextWindow.AudioContext = FakeAudioContext as unknown as typeof AudioContext;
    supportedWindow.morde = { voiceWaveformEnabled: true, beginVoiceRecording, endVoiceRecording };
    const callbacks: FrameRequestCallback[] = [];
    vi.stubGlobal('requestAnimationFrame', (callback: FrameRequestCallback) => { callbacks.push(callback); return callbacks.length; });
    vi.stubGlobal('cancelAnimationFrame', vi.fn());
    render(<AssistantVoiceInput onTranscript={vi.fn()} />);

    fireEvent.click(screen.getByRole('button', { name: '开始语音输入' }));
    const waveform = await waitFor(() => {
      const element = document.querySelector('.assistant-voice-waveform');
      expect(element).not.toBeNull();
      return element as HTMLElement;
    });

    expect(getUserMedia).toHaveBeenCalledWith({ audio: true, video: false });
    expect(beginVoiceRecording).toHaveBeenCalledOnce();
    expect(FakeAudioContext.latest?.analyser.getByteTimeDomainData).toHaveBeenCalled();
    expect(waveform.firstElementChild).toHaveStyle({ height: '3.8px' });

    fireEvent.click(screen.getByRole('button', { name: '停止语音输入' }));

    expect(track.stop).toHaveBeenCalledOnce();
    expect(FakeAudioContext.latest?.close).toHaveBeenCalledOnce();
    expect(endVoiceRecording).toHaveBeenCalledWith('audio-token');
    expect(document.querySelector('.assistant-voice-waveform')).toBeNull();
  });

  it('starts the local meter when the browser omits the recognition start event', async () => {
    FakeRecognition.omitStartEvent = true;
    const { stream, track } = fakeStream();
    const getUserMedia = vi.fn(async () => stream);
    const endVoiceRecording = vi.fn(async () => ({ released: true }));
    setMediaDevices(getUserMedia);
    audioContextWindow.AudioContext = FakeAudioContext as unknown as typeof AudioContext;
    supportedWindow.morde = {
      voiceWaveformEnabled: true,
      beginVoiceRecording: vi.fn(async () => 'no-start-event-token'),
      endVoiceRecording,
    };
    render(<AssistantVoiceInput onTranscript={vi.fn()} />);

    fireEvent.click(screen.getByRole('button', { name: '开始语音输入' }));
    const waveform = await waitFor(() => {
      const element = document.querySelector('.assistant-voice-waveform');
      expect(element).not.toBeNull();
      return element as HTMLElement;
    });

    expect(getUserMedia).toHaveBeenCalledWith({ audio: true, video: false });
    expect(waveform.firstElementChild).toHaveStyle({ height: '3.8px' });
    fireEvent.click(screen.getByRole('button', { name: '取消语音输入' }));
    expect(track.stop).toHaveBeenCalledOnce();
    expect(endVoiceRecording).toHaveBeenCalledWith('no-start-event-token');
  });

  it('keeps speech recognition running and hides the meter when microphone capture fails', async () => {
    const getUserMedia = vi.fn(async () => { throw new Error('microphone busy'); });
    const endVoiceRecording = vi.fn(async () => ({ released: true }));
    setMediaDevices(getUserMedia);
    audioContextWindow.AudioContext = FakeAudioContext as unknown as typeof AudioContext;
    supportedWindow.morde = {
      voiceWaveformEnabled: true,
      beginVoiceRecording: vi.fn(async () => 'busy-token'),
      endVoiceRecording,
    };
    render(<AssistantVoiceInput onTranscript={vi.fn()} />);

    fireEvent.click(screen.getByRole('button', { name: '开始语音输入' }));
    await waitFor(() => expect(endVoiceRecording).toHaveBeenCalledWith('busy-token'));

    expect(FakeRecognition.latest?.start).toHaveBeenCalledOnce();
    expect(screen.getByRole('button', { name: '停止语音输入' })).toHaveAttribute('aria-pressed', 'true');
    expect(document.querySelector('.assistant-voice-waveform')).toBeNull();
  });

  it('releases the waveform lease when local audio metering is unavailable', async () => {
    const endVoiceRecording = vi.fn(async () => ({ released: true }));
    const getUserMedia = vi.fn(async () => fakeStream().stream);
    setMediaDevices(getUserMedia);
    supportedWindow.morde = {
      voiceWaveformEnabled: true,
      beginVoiceRecording: vi.fn(async () => 'unavailable-token'),
      endVoiceRecording,
    };
    render(<AssistantVoiceInput onTranscript={vi.fn()} />);

    fireEvent.click(screen.getByRole('button', { name: '开始语音输入' }));
    await waitFor(() => expect(endVoiceRecording).toHaveBeenCalledWith('unavailable-token'));

    expect(FakeRecognition.latest?.start).toHaveBeenCalledOnce();
    expect(screen.getByRole('button', { name: '停止语音输入' })).toHaveAttribute('aria-pressed', 'true');
    expect(getUserMedia).not.toHaveBeenCalled();
    expect(document.querySelector('.assistant-voice-waveform')).toBeNull();
  });

  it('stops a getUserMedia stream that resolves after voice input was cancelled', async () => {
    const { stream, track } = fakeStream();
    let resolveMedia: ((value: MediaStream) => void) | undefined;
    const getUserMedia = vi.fn(() => new Promise<MediaStream>((resolve) => { resolveMedia = resolve; }));
    const endVoiceRecording = vi.fn(async () => ({ released: true }));
    setMediaDevices(getUserMedia);
    audioContextWindow.AudioContext = FakeAudioContext as unknown as typeof AudioContext;
    supportedWindow.morde = {
      voiceWaveformEnabled: true,
      beginVoiceRecording: vi.fn(async () => 'late-token'),
      endVoiceRecording,
    };
    render(<AssistantVoiceInput onTranscript={vi.fn()} />);

    fireEvent.click(screen.getByRole('button', { name: '开始语音输入' }));
    await waitFor(() => expect(getUserMedia).toHaveBeenCalledOnce());
    fireEvent.click(screen.getByRole('button', { name: '取消语音输入' }));
    resolveMedia?.(stream);
    await waitFor(() => expect(track.stop).toHaveBeenCalledOnce());

    expect(endVoiceRecording).toHaveBeenCalledWith('late-token');
    expect(document.querySelector('.assistant-voice-waveform')).toBeNull();
  });

  it.each(['cancel', 'unmount'] as const)(
    'does not start recognition and releases a pending voice recording lease after %s',
    async (interruption) => {
      let resolveLease: ((token: string) => void) | undefined;
      const beginVoiceRecording = vi.fn(() => new Promise<string>((resolve) => { resolveLease = resolve; }));
      const endVoiceRecording = vi.fn(async () => ({ released: true }));
      supportedWindow.morde = { voiceWaveformEnabled: false, beginVoiceRecording, endVoiceRecording };
      const onTranscript = vi.fn();
      const view = render(<AssistantVoiceInput onTranscript={onTranscript} />);

      fireEvent.click(screen.getByRole('button', { name: '开始语音输入' }));
      const recognition = FakeRecognition.latest;
      expect(recognition?.start).not.toHaveBeenCalled();
      expect(beginVoiceRecording).toHaveBeenCalledOnce();

      if (interruption === 'cancel') fireEvent.click(screen.getByRole('button', { name: '取消语音输入' }));
      else view.unmount();

      await act(async () => {
        resolveLease?.(`late-${interruption}-token`);
      });

      await waitFor(() => expect(endVoiceRecording).toHaveBeenCalledWith(`late-${interruption}-token`));
      expect(recognition?.start).not.toHaveBeenCalled();
      expect(onTranscript).not.toHaveBeenCalled();
    },
  );

  it('returns to idle when the browser ends recognition unexpectedly', () => {
    render(<AssistantVoiceInput onTranscript={vi.fn()} />);
    fireEvent.click(screen.getByRole('button', { name: '开始语音输入' }));

    act(() => FakeRecognition.latest?.onend?.());

    expect(screen.getByRole('button', { name: '开始语音输入' })).toHaveAttribute('aria-pressed', 'false');
  });

  it('marks the voice control unavailable when browser recognition cannot start', () => {
    FakeRecognition.startThrows = true;
    render(<AssistantVoiceInput onTranscript={vi.fn()} />);

    fireEvent.click(screen.getByRole('button', { name: '开始语音输入' }));

    const button = screen.getByRole('button', { name: '语音输入不可用' });
    expect(button).toHaveClass('is-unavailable');
    expect(button).toHaveAttribute('aria-pressed', 'false');
    expect(screen.getByLabelText('语音输入状态').textContent?.trim()).toBe('');
  });

  it('marks the voice control unavailable when the browser has no speech recognition API', () => {
    delete supportedWindow.SpeechRecognition;
    render(<AssistantVoiceInput onTranscript={vi.fn()} />);
    const status = screen.getByLabelText('语音输入状态');
    const initialHeight = status.getBoundingClientRect().height;

    fireEvent.click(screen.getByRole('button', { name: '开始语音输入' }));

    expect(screen.getByRole('button', { name: '语音输入不可用' })).toHaveClass('is-unavailable');
    expect(status).toHaveClass('assistant-voice-status-idle');
    expect(status.textContent?.trim()).toBe('');
    expect(status.style.height).toBe('1.2em');
    expect(status.getBoundingClientRect().height).toBe(initialHeight);
  });

  it('marks denied microphone permission red without showing a status message', () => {
    const onTranscript = vi.fn();
    render(<AssistantVoiceInput onTranscript={onTranscript} />);
    fireEvent.click(screen.getByRole('button', { name: '开始语音输入' }));

    act(() => FakeRecognition.latest?.onerror?.({ error: 'not-allowed' }));

    expect(screen.getByRole('button', { name: '语音输入不可用' })).toHaveClass('is-unavailable');
    expect(screen.getByLabelText('语音输入状态').textContent?.trim()).toBe('');
    expect(onTranscript).not.toHaveBeenCalled();
    expect(screen.getByRole('button', { name: '语音输入不可用' })).toBeEnabled();
  });

  it('marks browser network recognition failures red without showing status text', () => {
    render(<AssistantVoiceInput onTranscript={vi.fn()} compact />);
    fireEvent.click(screen.getByRole('button', { name: '开始语音输入' }));

    act(() => FakeRecognition.latest?.onerror?.({ error: 'network' }));

    expect(screen.getByRole('button', { name: '语音输入不可用' })).toHaveClass('is-unavailable');
    expect(screen.getByLabelText('语音输入状态')).toHaveClass('assistant-voice-status-idle');
    expect(screen.getByLabelText('语音输入状态').textContent?.trim()).toBe('');
  });

  it('keeps the red state until a retry starts successfully', () => {
    render(<AssistantVoiceInput onTranscript={vi.fn()} compact />);
    fireEvent.click(screen.getByRole('button', { name: '开始语音输入' }));
    act(() => FakeRecognition.latest?.onerror?.({ error: 'network' }));

    fireEvent.click(screen.getByRole('button', { name: '语音输入不可用' }));

    expect(screen.getByRole('button', { name: '停止语音输入' })).not.toHaveClass('is-unavailable');
  });

  it('aborts browser recognition and ignores late callbacks when the component unmounts', () => {
    const onTranscript = vi.fn();
    const view = render(<AssistantVoiceInput onTranscript={onTranscript} />);
    fireEvent.click(screen.getByRole('button', { name: '开始语音输入' }));
    const recognition = FakeRecognition.latest;

    view.unmount();

    expect(recognition?.abort).toHaveBeenCalledOnce();
    act(() => {
      recognition?.emitFinal('窗口关闭后的迟到文字');
      recognition?.onerror?.({ error: 'aborted' });
      recognition?.onend?.();
    });
    expect(onTranscript).not.toHaveBeenCalled();
  });
});
