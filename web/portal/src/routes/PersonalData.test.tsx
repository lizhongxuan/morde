import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter, Route, Routes, useLocation } from 'react-router';

const { currentUser, apiMocks } = vi.hoisted(() => ({
  currentUser: { id: 'u_personal_page', name: 'Alice' },
  apiMocks: {
    snapshot: vi.fn(), changes: vi.fn(), getObject: vi.fn(), replace: vi.fn(), delete: vi.fn(), quota: vi.fn(),
    createUpload: vi.fn(), putChunk: vi.fn(), commit: vi.fn(), cancelUpload: vi.fn(), getAttachment: vi.fn(), submitTask: vi.fn(),
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
  apiMocks.getObject.mockRejectedValue(Object.assign(new Error('not found'), { status: 404 }));
  apiMocks.replace.mockReset().mockResolvedValue({ result_revision: 1, account_change_sequence: 1 });
  apiMocks.delete.mockReset().mockResolvedValue({ result_revision: 1, account_change_sequence: 1 });
  apiMocks.quota.mockReset().mockResolvedValue({ used_bytes: 0, reserved_bytes: 0, quota_bytes: 1024, remaining_bytes: 1024, max_attachment_bytes: 1024 });
  apiMocks.createUpload.mockReset();
  apiMocks.putChunk.mockReset();
  apiMocks.commit.mockReset();
  apiMocks.cancelUpload.mockReset();
  apiMocks.getAttachment.mockReset();
  apiMocks.submitTask.mockReset();
  localStorage.clear();
});

afterEach(() => cleanup());

describe('PersonalData', () => {
  it('loads an empty remote snapshot and saves a profile through the personal-object API', async () => {
    const user = userEvent.setup();
    renderPage();
    expect(await screen.findByText(/同步已开启/)).toBeInTheDocument();
    expect(screen.queryByText(/旧 nanoMuse/)).not.toBeInTheDocument();
    expect(screen.queryByLabelText('选择要导入的会话 JSON 文件')).not.toBeInTheDocument();
    expect(screen.queryByLabelText('选择旧版 profile.json')).not.toBeInTheDocument();
    expect(screen.queryByLabelText('选择旧版 rooms.json')).not.toBeInTheDocument();
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

  it('saves feed preferences from the preferences tab', async () => {
    const user = userEvent.setup();
    renderPage();
    await screen.findByText(/同步已开启/);
    await user.click(screen.getByRole('tab', { name: '偏好' }));
    await user.type(screen.getByLabelText('动态偏好'), '关注人工智能');
    await user.click(screen.getByRole('button', { name: '保存动态偏好' }));
    await waitFor(() => expect(apiMocks.replace).toHaveBeenCalledWith(
      'feed_preferences', 'default', 0, { content: '关注人工智能' }, expect.any(String),
    ));
  });

  it('defaults memory authorization to off and only shows the server-confirmed setting', async () => {
    const user = userEvent.setup();
    let confirm!: (value: { result_revision: number; account_change_sequence: number }) => void;
    apiMocks.replace.mockImplementationOnce(() => new Promise((resolve) => { confirm = resolve; }));
    renderPage();
    await screen.findByText(/同步已开启/);
    await user.click(screen.getByRole('tab', { name: '记忆' }));
    const toggle = screen.getByRole('checkbox', { name: '允许 AI 使用个人记忆' });
    expect(toggle).not.toBeChecked();
    await user.click(toggle);
    await waitFor(() => expect(apiMocks.replace).toHaveBeenCalledWith(
      'skill_setting', 'builtin_memory', 0, { enabled: true }, expect.any(String),
    ));
    expect(toggle).not.toBeChecked();
    expect(toggle).toBeDisabled();
    expect(screen.getByText('设置正在同步，服务端确认后生效。')).toBeInTheDocument();
    confirm({ result_revision: 1, account_change_sequence: 1 });
    await waitFor(() => expect(toggle).toBeChecked());
  });

  it('saves a personal Morde SOUL override through the owner-scoped personal-object API', async () => {
    const user = userEvent.setup();
    renderPage();
    await screen.findByText(/同步已开启/);
    await user.click(screen.getByRole('tab', { name: 'AI 个性' }));
    expect(screen.getByText('当前使用系统默认的 Morde 人格。保存后会为你的个人对话创建覆盖版本。')).toBeInTheDocument();
    await user.type(screen.getByLabelText('SOUL 内容'), '简洁回答；先检查现有信息再提问。');
    await user.click(screen.getByRole('button', { name: '保存个人人格' }));
    await waitFor(() => expect(apiMocks.replace).toHaveBeenCalledWith(
      'soul', 'default', 0,
      { name: 'Morde', lang: 'zh-CN', content: '简洁回答；先检查现有信息再提问。' },
      expect.any(String),
    ));
  });

  it('deletes the personal SOUL override to restore the system persona', async () => {
    const user = userEvent.setup();
    apiMocks.snapshot.mockResolvedValue({
      watermark: 1,
      items: [{ object_type: 'soul', object_id: 'default', revision: 3, body: { name: 'Morde', lang: 'zh-CN', content: 'Use short answers.' } }],
      next_cursor: null,
    });
    renderPage();
    await screen.findByText(/同步已开启/);
    await user.click(screen.getByRole('tab', { name: 'AI 个性' }));
    expect(screen.getByLabelText('SOUL 内容')).toHaveValue('Use short answers.');
    await user.click(screen.getByRole('button', { name: '删除个人人格' }));
    await waitFor(() => expect(apiMocks.delete).toHaveBeenCalledWith('soul', 'default', 3, expect.any(String)));
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

});
