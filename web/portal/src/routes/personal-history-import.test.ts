import { describe, expect, it } from 'vitest';
import { batchLegacyHistory, readLegacyHistorySelection } from './personal-history-import';

describe('legacy nanoMuse history import', () => {
  it('reads only user and visible assistant text from selected thread files', async () => {
    const index = new File([JSON.stringify([{ id: 'main', title: '旧会话' }])], 'index.json');
    const thread = new File([JSON.stringify({ thread: 'main', events: [
      { id: 'e_user01', type: 'user', text: '你好' },
      { id: 'e_tool001', type: 'tool', summary: '本地工具输出' },
      { id: 'e_asst01', type: 'assistant', text: '你好！', reasoning: '不导入推理' },
      { id: 'e_quiet1', type: 'assistant', text: '后台内容', quiet: true },
      { id: 'e_back01', type: 'assistant', text: '目标任务', source: 'background' },
      { id: 'e_file001', type: 'user', text: '看这张图', files: [{ path: 'attachments/private.png' }] },
      { id: 'e_large01', type: 'assistant', text: 'x'.repeat(32 * 1024 + 1) },
    ] })], 'main.json');

    const selection = await readLegacyHistorySelection([index, thread]);
    expect(selection.conversations).toEqual([{
      source_conversation_id: 'main', title: '旧会话',
      messages: [
        { source_message_id: 'e_user01', role: 'user', content: '你好' },
        { source_message_id: 'e_asst01', role: 'assistant', content: '你好！' },
        { source_message_id: 'e_file001', role: 'user', content: '看这张图' },
      ],
      ignored_events: 4, skipped_attachments: 1,
    }]);
    expect(selection.skipped_files).toBe(0);
    expect(selection.ignored_events).toBe(4);
    expect(selection.skipped_attachments).toBe(1);
    expect(JSON.stringify(selection)).not.toContain('attachments/private.png');
    expect(JSON.stringify(selection)).not.toContain('不导入推理');
  });

  it('rejects unreadable thread files and splits imports at service batch limits', async () => {
    const selection = await readLegacyHistorySelection([new File(['not json'], 'main.json')]);
    expect(selection).toMatchObject({ conversations: [], skipped_files: 1 });

    const messages = Array.from({ length: 201 }, (_, index) => ({
      source_message_id: `e_${index}`, role: 'user' as const, content: 'x'.repeat(6 * 1024),
    }));
    const batches = batchLegacyHistory(messages);
    expect(batches.map((batch) => batch.length)).toEqual([170, 31]);
    expect(batches.flat()).toEqual(messages);
    expect(batches.every((batch) => batch.reduce((total, message) => total + new TextEncoder().encode(message.content).length, 0) <= 1 << 20)).toBe(true);
  });
});
