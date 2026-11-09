import { act, cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { AssistantVoiceInput } from './AssistantVoiceInput';

class FakeRecognition {
  static latest: FakeRecognition | null = null;
  static startThrows = false;

  lang = '';
  continuous = false;
  interimResults = true;
  onresult: ((event: { resultIndex?: number; results: ArrayLike<{ isFinal?: boolean; 0?: { transcript?: string } }> }) => void) | null = null;
  onerror: ((event: { error?: string }) => void) | null = null;
  onend: (() => void) | null = null;
  start = vi.fn(() => {
    if (FakeRecognition.startThrows) throw new Error('start failed');
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

type SpeechRecognitionWindow = Window & {
  SpeechRecognition?: typeof FakeRecognition;
  webkitSpeechRecognition?: typeof FakeRecognition;
};

const supportedWindow = window as SpeechRecognitionWindow;

describe('AssistantVoiceInput', () => {
  beforeEach(() => {
    FakeRecognition.latest = null;
    FakeRecognition.startThrows = false;
    supportedWindow.SpeechRecognition = FakeRecognition;
  });

  afterEach(() => {
    cleanup();
    delete supportedWindow.SpeechRecognition;
    delete supportedWindow.webkitSpeechRecognition;
    vi.restoreAllMocks();
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

  it('stops the active recognition session from the same control', () => {
    render(<AssistantVoiceInput onTranscript={vi.fn()} />);
    fireEvent.click(screen.getByRole('button', { name: '开始语音输入' }));
    const recognition = FakeRecognition.latest;

    fireEvent.click(screen.getByRole('button', { name: '停止语音输入' }));

    expect(recognition?.stop).toHaveBeenCalledOnce();
    expect(screen.getByRole('button', { name: '开始语音输入' })).toHaveAttribute('aria-pressed', 'false');
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

  it('returns to idle when the browser ends recognition unexpectedly', () => {
    render(<AssistantVoiceInput onTranscript={vi.fn()} />);
    fireEvent.click(screen.getByRole('button', { name: '开始语音输入' }));

    act(() => FakeRecognition.latest?.onend?.());

    expect(screen.getByRole('button', { name: '开始语音输入' })).toHaveAttribute('aria-pressed', 'false');
  });

  it('shows an actionable error when browser recognition cannot start', () => {
    FakeRecognition.startThrows = true;
    render(<AssistantVoiceInput onTranscript={vi.fn()} />);

    fireEvent.click(screen.getByRole('button', { name: '开始语音输入' }));

    expect(screen.getByLabelText('语音输入状态')).toHaveTextContent('无法启动语音输入');
    expect(screen.getByRole('button', { name: '开始语音输入' })).toHaveAttribute('aria-pressed', 'false');
  });

  it('shows a stable status when the browser has no speech recognition API', () => {
    delete supportedWindow.SpeechRecognition;
    render(<AssistantVoiceInput onTranscript={vi.fn()} />);
    const status = screen.getByLabelText('语音输入状态');
    const initialHeight = status.getBoundingClientRect().height;

    fireEvent.click(screen.getByRole('button', { name: '开始语音输入' }));

    expect(status).toHaveTextContent('当前浏览器不支持语音输入。');
    expect(status.style.height).toBe('1.2em');
    expect(status.getBoundingClientRect().height).toBe(initialHeight);
  });

  it('explains denied microphone permission without emitting transcript text', () => {
    const onTranscript = vi.fn();
    render(<AssistantVoiceInput onTranscript={onTranscript} />);
    fireEvent.click(screen.getByRole('button', { name: '开始语音输入' }));

    act(() => FakeRecognition.latest?.onerror?.({ error: 'not-allowed' }));

    expect(screen.getByLabelText('语音输入状态')).toHaveTextContent('未获得麦克风权限');
    expect(onTranscript).not.toHaveBeenCalled();
    expect(screen.getByRole('button', { name: '开始语音输入' })).toBeEnabled();
  });

  it('aborts browser recognition when the component unmounts', () => {
    const view = render(<AssistantVoiceInput onTranscript={vi.fn()} />);
    fireEvent.click(screen.getByRole('button', { name: '开始语音输入' }));
    const recognition = FakeRecognition.latest;

    view.unmount();

    expect(recognition?.abort).toHaveBeenCalledOnce();
  });
});
