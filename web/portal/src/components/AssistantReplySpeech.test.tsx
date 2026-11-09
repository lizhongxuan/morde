import { act, cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { AssistantReplySpeech } from './AssistantReplySpeech';

class FakeUtterance {
  static all: FakeUtterance[] = [];
  lang = '';
  onend: (() => void) | null = null;
  onerror: ((event: { error?: string }) => void) | null = null;

  constructor(readonly text = '') {
    FakeUtterance.all.push(this);
  }
}

class FakeSpeechSynthesis {
  utterances: FakeUtterance[] = [];
  speak = vi.fn((utterance: FakeUtterance) => { this.utterances.push(utterance); });
  cancel = vi.fn();
}

type SpeechWindow = {
  speechSynthesis?: FakeSpeechSynthesis;
  SpeechSynthesisUtterance?: typeof FakeUtterance;
};

const speechWindow = window as unknown as SpeechWindow;
let synthesis: FakeSpeechSynthesis;
let fetchMock: ReturnType<typeof vi.fn>;

describe('AssistantReplySpeech', () => {
  beforeEach(() => {
    FakeUtterance.all = [];
    synthesis = new FakeSpeechSynthesis();
    speechWindow.speechSynthesis = synthesis;
    speechWindow.SpeechSynthesisUtterance = FakeUtterance;
    fetchMock = vi.fn();
    vi.stubGlobal('fetch', fetchMock);
  });

  afterEach(() => {
    cleanup();
    delete speechWindow.speechSynthesis;
    delete speechWindow.SpeechSynthesisUtterance;
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  it('speaks only after an explicit click and passes the final reply with its locale', () => {
    const view = render(<AssistantReplySpeech text="这是最终答复。" sessionKey="conversation-1" language="zh-CN" />);
    expect(synthesis.speak).not.toHaveBeenCalled();
    view.rerender(<AssistantReplySpeech text="这是刷新后的答复。" sessionKey="conversation-1" language="zh-CN" />);
    expect(synthesis.speak).not.toHaveBeenCalled();

    fireEvent.click(screen.getByRole('button', { name: '朗读回复' }));

    expect(synthesis.speak).toHaveBeenCalledOnce();
    expect(FakeUtterance.all[0]!.text).toBe('这是刷新后的答复。');
    expect(FakeUtterance.all[0]!.lang).toBe('zh-CN');
    expect(screen.getByRole('button', { name: '停止朗读回复' })).toHaveAttribute('aria-pressed', 'true');
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it('cancels playback when the user stops it and clears the control state', () => {
    render(<AssistantReplySpeech text="正在朗读的内容。" sessionKey="conversation-1" />);
    fireEvent.click(screen.getByRole('button', { name: '朗读回复' }));

    fireEvent.click(screen.getByRole('button', { name: '停止朗读回复' }));

    expect(synthesis.cancel).toHaveBeenCalledOnce();
    expect(screen.getByRole('button', { name: '朗读回复' })).toHaveAttribute('aria-pressed', 'false');
    expect(screen.getByLabelText('朗读状态').textContent).toBe('\u00a0');
  });

  it('cancels the previous reply when another reply starts and ignores its late events', () => {
    render(<div>
      <AssistantReplySpeech text="第一条答复。" sessionKey="conversation-1" />
      <AssistantReplySpeech text="第二条答复。" sessionKey="conversation-1" />
    </div>);
    const buttons = screen.getAllByRole('button', { name: '朗读回复' });
    const firstButton = buttons[0]!;
    const secondButton = buttons[1]!;
    fireEvent.click(firstButton);
    const firstUtterance = FakeUtterance.all[0]!;

    fireEvent.click(secondButton);
    act(() => firstUtterance.onend?.());

    expect(synthesis.cancel).toHaveBeenCalledOnce();
    expect(synthesis.speak).toHaveBeenCalledTimes(2);
    expect(screen.getAllByRole('button', { name: '停止朗读回复' })).toHaveLength(1);
    expect(screen.getByRole('button', { name: '停止朗读回复' })).toHaveAttribute('aria-pressed', 'true');
  });

  it('cancels when the conversation changes or the reply component unmounts', () => {
    const view = render(<AssistantReplySpeech text="第一条答复。" sessionKey="conversation-1" />);
    fireEvent.click(screen.getByRole('button', { name: '朗读回复' }));
    view.rerender(<AssistantReplySpeech text="第一条答复。" sessionKey="conversation-2" />);
    expect(synthesis.cancel).toHaveBeenCalledOnce();

    fireEvent.click(screen.getByRole('button', { name: '朗读回复' }));
    view.unmount();
    expect(synthesis.cancel).toHaveBeenCalledTimes(2);
  });

  it('shows a fixed-height unsupported status without making a speech or network request', () => {
    delete speechWindow.speechSynthesis;
    render(<AssistantReplySpeech text="需要朗读的答复。" sessionKey="conversation-1" />);
    const status = screen.getByLabelText('朗读状态');
    const height = status.style.height;

    fireEvent.click(screen.getByRole('button', { name: '朗读回复' }));

    expect(status).toHaveTextContent('当前浏览器不支持系统语音朗读。');
    expect(status.style.height).toBe(height);
    expect(height).toBe('1.2em');
    expect(synthesis.speak).not.toHaveBeenCalled();
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it('reports synthesis errors and returns to a retryable idle state', () => {
    render(<AssistantReplySpeech text="语音服务可能失败。" sessionKey="conversation-1" />);
    fireEvent.click(screen.getByRole('button', { name: '朗读回复' }));
    act(() => FakeUtterance.all[0]!.onerror?.({ error: 'network' }));

    expect(screen.getByLabelText('朗读状态')).toHaveTextContent('系统语音服务暂不可用。');
    expect(screen.getByRole('button', { name: '朗读回复' })).toHaveAttribute('aria-pressed', 'false');
  });

  it('handles missing utterance support and synchronous speak failures', () => {
    delete speechWindow.SpeechSynthesisUtterance;
    const view = render(<AssistantReplySpeech text="答复。" sessionKey="conversation-1" />);
    fireEvent.click(screen.getByRole('button', { name: '朗读回复' }));
    expect(screen.getByLabelText('朗读状态')).toHaveTextContent('当前浏览器不支持系统语音朗读。');

    speechWindow.SpeechSynthesisUtterance = FakeUtterance;
    synthesis.speak.mockImplementation(() => { throw new Error('speech is unavailable'); });
    fireEvent.click(screen.getByRole('button', { name: '朗读回复' }));
    expect(screen.getByLabelText('朗读状态')).toHaveTextContent('无法启动系统语音朗读。');
    expect(screen.getByRole('button', { name: '朗读回复' })).toHaveAttribute('aria-pressed', 'false');
    view.unmount();
  });
});
