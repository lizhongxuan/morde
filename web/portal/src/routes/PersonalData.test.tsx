import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter, Route, Routes, useLocation } from 'react-router';

const { currentUser, apiMocks } = vi.hoisted(() => ({
  currentUser: { id: 'u_personal_page', name: 'Alice' },
  apiMocks: {
    snapshot: vi.fn(), changes: vi.fn(), getObject: vi.fn(), replace: vi.fn(), delete: vi.fn(), quota: vi.fn(),
    createUpload: vi.fn(), putChunk: vi.fn(), commit: vi.fn(), cancelUpload: vi.fn(), getAttachment: vi.fn(), submitTask: vi.fn(),
    importHistory: vi.fn(),
  },
}));

vi.mock('../app/auth', () => ({ useAuth: () => ({ user: currentUser }) }));

vi.mock('../app/api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../app/api')>();
  return {
    ...actual,
    portalApi: {
      ...actual.portalApi,
      personalDataSnapshot: apiMocks.snapshot,
      personalDataChanges: apiMocks.changes,
      getPersonalObject: apiMocks.getObject,
      replacePersonalObject: apiMocks.replace,
      deletePersonalObject: apiMocks.delete,
      personalDataQuota: apiMocks.quota,
      createPersonalAttachmentUpload: apiMocks.createUpload,
      putPersonalAttachmentChunk: apiMocks.putChunk,
      commitPersonalAttachment: apiMocks.commit,
      cancelPersonalAttachmentUpload: apiMocks.cancelUpload,
      personalAttachment: apiMocks.getAttachment,
      importPersonalHistory: apiMocks.importHistory,
      submitTask: apiMocks.submitTask,
    },
  };
});

vi.mock('./personal-data-store', async (importOriginal) => {
  const actual = await importOriginal<typeof import('./personal-data-store')>();
  const storage = actual.createMemoryPersonalDataStorage();
  class TestPersonalDataStore extends actual.PersonalDataStore {
    constructor(userID: string, origin?: string) { super(userID, origin, storage); }
  }
  return { ...actual, PersonalDataStore: TestPersonalDataStore };
});

import { PersonalData } from './PersonalData';

function LocationProbe() {
  const location = useLocation();
  return <output aria-label="助手路由状态">{JSON.stringify({ pathname: location.pathname, search: location.search, state: location.state })}</output>;
}

function renderPage() {
  return render(<MemoryRouter initialEntries={['/personal']}><Routes>
    <Route path="/personal" element={<PersonalData />} />
    <Route path="/assistant" element={<LocationProbe />} />
  </Routes></MemoryRouter>);
}

beforeEach(() => {
  currentUser.id = `u_personal_${Math.random().toString(36).slice(2, 8)}`;
  currentUser.name = 'Alice';
  apiMocks.snapshot.mockReset().mockResolvedValue({ watermark: 0, items: [], next_cursor: null });
  apiMocks.changes.mockReset().mockImplementation(async (after: number, through?: number) => ({
    items: [], through_sequence: through ?? after, next_after_sequence: through ?? after, has_more: false,
  }));
  apiMocks.getObject.mockReset();
  apiMocks.replace.mockReset().mockResolvedValue({ result_revision: 1, account_change_sequence: 1 });
  apiMocks.delete.mockReset().mockResolvedValue({ result_revision: 1, account_change_sequence: 1 });
  apiMocks.quota.mockReset().mockResolvedValue({ used_bytes: 0, reserved_bytes: 0, quota_bytes: 1024, remaining_bytes: 1024, max_attachment_bytes: 1024 });
  apiMocks.createUpload.mockReset();
  apiMocks.putChunk.mockReset();
  apiMocks.commit.mockReset();
  apiMocks.cancelUpload.mockReset();
  apiMocks.getAttachment.mockReset();
  apiMocks.submitTask.mockReset();
  apiMocks.importHistory.mockReset().mockImplementation(async (body: { target_conversation_id?: string }) => ({
    conversation_id: body.target_conversation_id ?? 'c1234567', title: 'Imported', replay: false, messages: [],
  }));
  localStorage.clear();
});

afterEach(() => cleanup());

describe('PersonalData', () => {
  it('loads an empty remote snapshot and saves a profile through the personal-object API', async () => {
    const user = userEvent.setup();
    renderPage();
    expect(await screen.findByText(/同步已开启/)).toBeInTheDocument();
    expect(screen.getByText('还没有个人资料。保存后会创建默认资料。')).toBeInTheDocument();
    expect(apiMocks.snapshot).toHaveBeenCalledWith(undefined, 100);

    await user.clear(screen.getByLabelText('名称'));
    await user.type(screen.getByLabelText('名称'), 'Alice Morde');
    await user.click(screen.getByRole('button', { name: '保存个人资料' }));
    await waitFor(() => expect(apiMocks.replace).toHaveBeenCalledTimes(1));
    expect(apiMocks.replace.mock.calls[0]).toEqual([
      'profile', 'default', 0,
      { name: 'Alice Morde', avatar: 'dragon', emoji: '', color: '', description: '', style: '' },
      expect.any(String),
    ]);
    expect(apiMocks.submitTask).not.toHaveBeenCalled();
  });

  it('shows a retryable sync error and retries without clearing local data', async () => {
    const user = userEvent.setup();
    apiMocks.snapshot.mockRejectedValueOnce(new Error('网络暂不可用')).mockResolvedValueOnce({ watermark: 0, items: [], next_cursor: null });
    renderPage();
    expect(await screen.findByRole('alert')).toHaveTextContent('网络暂不可用');
    await user.click(screen.getByRole('button', { name: '重试同步' }));
    await waitFor(() => expect(apiMocks.snapshot).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(screen.queryByRole('alert')).not.toBeInTheDocument());
  });

  it('carries an idea into Assistant route state without putting prompt text in the URL or submitting a task', async () => {
    const user = userEvent.setup();
    renderPage();
    await screen.findByText(/同步已开启/);
    await user.click(screen.getByRole('tab', { name: '灵感' }));
    await user.click(screen.getAllByRole('link', { name: '带入助手' })[0]!);

    const location = JSON.parse(screen.getByLabelText('助手路由状态').textContent ?? '{}') as { pathname: string; search: string; state: { ideaID: string; ideaPrompt: string } };
    expect(location.pathname).toBe('/assistant');
    expect(location.search).toBe('');
    expect(location.state.ideaID).toBe('train-watch');
    expect(location.state.ideaPrompt).toMatch(/盯一趟火车票/);
    expect(apiMocks.submitTask).not.toHaveBeenCalled();
  });

  it('previews selected legacy text and imports only after the user confirms', async () => {
    const user = userEvent.setup();
    renderPage();
    await screen.findByText(/同步已开启/);
    const file = new File([JSON.stringify({ thread: 'main', events: [
      { id: 'e_user01', type: 'user', text: '旧用户消息' },
      { id: 'e_tool001', type: 'tool', summary: '本地工具数据' },
      { id: 'e_asst01', type: 'assistant', text: '旧助手回复' },
    ] })], 'main.json', { type: 'application/json' });

    await user.upload(screen.getByLabelText('选择要导入的会话 JSON 文件'), file);
    expect(await screen.findByText(/可导入 1 个会话、2 条文本消息/)).toBeInTheDocument();
    expect(apiMocks.importHistory).not.toHaveBeenCalled();
    expect(apiMocks.submitTask).not.toHaveBeenCalled();

    await user.click(screen.getByRole('button', { name: '明确导入所选历史（2 条）' }));
    await waitFor(() => expect(apiMocks.importHistory).toHaveBeenCalledTimes(1));
    expect(apiMocks.importHistory.mock.calls[0]![0]).toMatchObject({
      source_id: 'nanomuse-export-v1', device_account_id: 'nanomuse-local-v1',
      source_conversation_id: 'main', title: 'main',
      messages: [
        { source_message_id: 'e_user01', role: 'user', content: '旧用户消息' },
        { source_message_id: 'e_asst01', role: 'assistant', content: '旧助手回复' },
      ],
    });
    expect(apiMocks.submitTask).not.toHaveBeenCalled();
    expect(await screen.findByText(/已导入 1 个会话、2 条文本消息/)).toBeInTheDocument();
  });
});
