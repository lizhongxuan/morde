import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter, Route, Routes, useLocation } from 'react-router';

const { currentUser, apiMocks } = vi.hoisted(() => ({
  currentUser: { id: 'u_personal_page', name: 'Alice' },
  apiMocks: {
    capabilities: vi.fn(), snapshot: vi.fn(), changes: vi.fn(), getObject: vi.fn(), replace: vi.fn(), delete: vi.fn(), quota: vi.fn(),
    createUpload: vi.fn(), putChunk: vi.fn(), commit: vi.fn(), cancelUpload: vi.fn(), getAttachment: vi.fn(), submitTask: vi.fn(),
  },
}));

vi.mock('../app/auth', () => ({ useAuth: () => ({ user: currentUser, signOut: async () => {} }) }));

vi.mock('../app/api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../app/api')>();
  return {
    ...actual,
    portalApi: {
      ...actual.portalApi,
      capabilities: apiMocks.capabilities,
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
    constructor(userID: string, origin?: string, _unused?: unknown, serviceInstanceID?: string) { super(userID, origin, storage, serviceInstanceID); }
  }
  return { ...actual, PersonalDataStore: TestPersonalDataStore };
});

function sharedTestLocks(): LockManager {
  const tails = new Map<string, Promise<void>>();
  return {
    request: async <T,>(name: string, _options: LockOptions, callback: (lock: Lock | null) => T | PromiseLike<T>): Promise<T> => {
      const previous = tails.get(name) ?? Promise.resolve();
      let release!: () => void;
      const current = new Promise<void>((resolve) => { release = resolve; });
      const tail = previous.then(() => current);
      tails.set(name, tail);
      await previous;
      try { return await callback({ name, mode: 'exclusive' } as Lock); }
      finally {
        release();
        if (tails.get(name) === tail) tails.delete(name);
      }
    },
  } as unknown as LockManager;
}

import { PersonalData } from './PersonalData';
import { PersonalDataStore } from './personal-data-store';
import { PortalLayout } from '../components/PortalLayout';
import { ToastProvider } from '../app/toast';

function LocationProbe() {
  const location = useLocation();
  return <output aria-label="助手路由状态">{JSON.stringify({ pathname: location.pathname, search: location.search, state: location.state })}</output>;
}

function renderPage() {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(<QueryClientProvider client={queryClient}><ToastProvider><MemoryRouter initialEntries={['/personal']}><Routes>
    <Route element={<PortalLayout />}>
      <Route path="/personal" element={<PersonalData />} />
      <Route path="/assistant" element={<LocationProbe />} />
    </Route>
  </Routes></MemoryRouter></ToastProvider></QueryClientProvider>);
}

beforeEach(() => {
  currentUser.id = `u_personal_${Math.random().toString(36).slice(2, 8)}`;
  currentUser.name = 'Alice';
  apiMocks.capabilities.mockReset().mockResolvedValue({ service_instance_id: 'service-personal-test' });
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
  Object.defineProperty(navigator, 'locks', { configurable: true, value: sharedTestLocks() });
});

afterEach(() => cleanup());

describe('PersonalData', () => {
  it('waits for service identity before starting cloud sync', async () => {
    let confirmCapabilities!: (value: { service_instance_id: string }) => void;
    apiMocks.capabilities.mockImplementationOnce(() => new Promise((resolve) => { confirmCapabilities = resolve; }));
    renderPage();

    expect(await screen.findByText(/正在确认服务实例/)).toBeInTheDocument();
    expect(apiMocks.snapshot).not.toHaveBeenCalled();

    confirmCapabilities({ service_instance_id: 'service-personal-test' });
    await waitFor(() => expect(apiMocks.snapshot).toHaveBeenCalledTimes(1));
    expect(await screen.findByText(/保存后自动更新云端/)).toBeInTheDocument();
  });

  it('keeps the page viewport fitted and exposes long memory lists through pagination', async () => {
    const user = userEvent.setup();
    apiMocks.snapshot.mockResolvedValue({
      watermark: 1,
      items: [
        { object_type: 'memory', object_id: 'm_pager_1', revision: 1, body: { content: '记忆分页第一项' } },
        { object_type: 'memory', object_id: 'm_pager_2', revision: 1, body: { content: '记忆分页第二项' } },
        { object_type: 'memory', object_id: 'm_pager_3', revision: 1, body: { content: '记忆分页第三项' } },
      ],
      next_cursor: null,
    });
    renderPage();
    await screen.findByText(/保存后自动更新云端/);
    expect(document.querySelector('.personal-data-page[data-layout="viewport-fit"]')).toBeInTheDocument();
    await user.click(screen.getByRole('link', { name: '记忆' }));

    const pagination = await screen.findByRole('navigation', { name: '已保存记忆分页' });
    expect(screen.getByDisplayValue('记忆分页第一项')).toBeInTheDocument();
    expect(screen.queryByDisplayValue('记忆分页第三项')).not.toBeVisible();
    await user.click(within(pagination).getByRole('button', { name: '下一页' }));
    expect(screen.getByDisplayValue('记忆分页第二项')).toBeVisible();
    await user.click(within(pagination).getByRole('button', { name: '下一页' }));
    await waitFor(() => expect(screen.getByDisplayValue('记忆分页第三项')).toBeVisible());
  });

  it('loads an empty remote snapshot and saves a profile through the personal-object API', async () => {
    const user = userEvent.setup();
    const legacySettingKey = `morde.personal-data.sync.v1:${encodeURIComponent(window.location.origin)}:${encodeURIComponent(currentUser.id)}`;
    localStorage.setItem(legacySettingKey, 'false');
    renderPage();
    expect(await screen.findByText(/保存后自动更新云端/)).toBeInTheDocument();
    expect(screen.queryByRole('checkbox', { name: '同步到 Morde' })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: '立即同步' })).not.toBeInTheDocument();
    expect(screen.queryByText(/旧 nanoMuse/)).not.toBeInTheDocument();
    expect(screen.queryByLabelText('选择要导入的会话 JSON 文件')).not.toBeInTheDocument();
    expect(screen.queryByLabelText('选择旧版 profile.json')).not.toBeInTheDocument();
    expect(screen.queryByLabelText('选择旧版 rooms.json')).not.toBeInTheDocument();
    await waitFor(() => expect(apiMocks.snapshot).toHaveBeenCalledWith(undefined, 100));
    expect(screen.getByText('还没有个人资料。保存后会创建默认资料。')).toBeInTheDocument();

    await user.clear(screen.getByLabelText('名称'));
    await user.type(screen.getByLabelText('名称'), 'Alice Morde');
    await user.click(screen.getByRole('button', { name: '保存个人资料' }));
    await waitFor(() => expect(apiMocks.replace).toHaveBeenCalledTimes(1));
    expect(apiMocks.replace.mock.calls[0]).toEqual([
      'profile', 'default', 0,
      { name: 'Alice Morde', avatar: 'dragon', emoji: '', color: '', description: '', style: '' },
      expect.any(String), currentUser.id, 'service-personal-test',
    ]);
    expect(apiMocks.submitTask).not.toHaveBeenCalled();
  });

  it('cancels a stale personal attachment upload with the confirmed account and service identity', async () => {
    const store = new PersonalDataStore(currentUser.id, window.location.origin, undefined, 'service-personal-test');
    const attachment = await store.queueAttachment({
      objectID: 'library-stale-upload', name: 'stale.txt', kind: 'artifact', mediaType: 'text/plain',
      sizeBytes: 0, sha256: '0'.repeat(64), blob: new Blob([]), baseRevision: 0,
    });
    await store.saveUploadID(attachment.id, 'pu_stale-upload');
    await store.markAttachmentConflict(attachment.id, '上传版本已变化', true);
    renderPage();

    await waitFor(() => expect(apiMocks.cancelUpload).toHaveBeenCalledWith(
      'pu_stale-upload', 'service-personal-test', currentUser.id,
    ));
    expect(apiMocks.createUpload).not.toHaveBeenCalled();
    expect(apiMocks.putChunk).not.toHaveBeenCalled();
  });

  it('keeps previous-instance drafts out of automatic sync and offers explicit import using the current revision', async () => {
    const user = userEvent.setup();
    const oldStore = new PersonalDataStore(currentUser.id, window.location.origin, undefined, 'service-old');
    const oldDraft = await oldStore.queueReplace('profile', 'default', { name: 'Old service draft', avatar: 'dragon' });
    renderPage();
    await screen.findByText(/保存后自动更新云端/);
    expect(await screen.findByText('其他服务实例保留的草稿')).toBeInTheDocument();
    expect(screen.getByText(/Old service draft/)).toBeInTheDocument();
    expect(apiMocks.replace).not.toHaveBeenCalled();

    await user.click(screen.getByRole('button', { name: '复制到当前服务并提交' }));
    await waitFor(() => expect(apiMocks.replace).toHaveBeenCalledTimes(1));
    expect(apiMocks.replace.mock.calls[0]).toMatchObject([
      'profile', 'default', 0, { name: 'Old service draft', avatar: 'dragon' }, expect.any(String), currentUser.id, 'service-personal-test',
    ]);
    const retained = await oldStore.read();
    expect(retained.outbox).toHaveLength(1);
    expect(retained.outbox[0]).toMatchObject({ id: oldDraft.id, key: oldDraft.key, status: 'conflict', resolutionQueued: true });
  });

  it('requires a second confirmation before carrying an old-service deletion to the current service', async () => {
    const user = userEvent.setup();
    const currentProfile = { object_type: 'profile' as const, object_id: 'default', revision: 4, body: { name: 'Current profile', avatar: 'dragon' } };
    apiMocks.snapshot.mockResolvedValue({ watermark: 12, items: [currentProfile], next_cursor: null });
    const oldStore = new PersonalDataStore(currentUser.id, window.location.origin, undefined, 'service-old-delete');
    await oldStore.applySnapshot([{ ...currentProfile, revision: 2, body: { name: 'Old profile', avatar: 'dragon' } }], 3);
    const oldDraft = await oldStore.queueDelete('profile', 'default');
    expect(oldDraft.operation).toBe('delete');
    expect(await oldStore.read()).toMatchObject({ outbox: [{ id: oldDraft.id, status: 'pending' }] });
    renderPage();
    await screen.findByText(/保存后自动更新云端/);
    expect(await screen.findByText('其他服务实例保留的草稿')).toBeInTheDocument();
    expect(screen.getByText(/profile \/ default/)).toBeInTheDocument();
    expect(apiMocks.delete).not.toHaveBeenCalled();

    await user.click(screen.getByRole('button', { name: '在当前服务重试删除' }));
    expect(screen.getByRole('group', { name: '确认在当前服务重试删除 default' })).toBeInTheDocument();
    expect(apiMocks.delete).not.toHaveBeenCalled();

    await user.click(screen.getByRole('button', { name: '确认并提交删除' }));
    await waitFor(() => expect(apiMocks.delete).toHaveBeenCalledWith(
      'profile', 'default', 4, expect.any(String), 'service-personal-test', currentUser.id,
    ));
    expect((await oldStore.read()).outbox[0]).toMatchObject({ id: oldDraft.id, key: oldDraft.key, status: 'conflict', resolutionQueued: true });
  });

  it('automatically submits changes queued during an in-progress cloud update', async () => {
    const user = userEvent.setup();
    let completeProfileSave!: (value: { result_revision: number; account_change_sequence: number }) => void;
    apiMocks.replace.mockImplementationOnce(() => new Promise((resolve) => { completeProfileSave = resolve; }));
    renderPage();
    await screen.findByText(/保存后自动更新云端/);
    await screen.findByLabelText('名称');

    await user.clear(screen.getByLabelText('名称'));
    await user.type(screen.getByLabelText('名称'), 'Alice Morde');
    await user.click(screen.getByRole('button', { name: '保存个人资料' }));
    await waitFor(() => expect(apiMocks.replace).toHaveBeenCalledTimes(1));

    await user.click(screen.getByRole('link', { name: '动态' }));
    await user.type(screen.getByLabelText('新增动态标题'), '同步期间新增');
    await user.type(screen.getByLabelText('内容'), '这条内容不能留在队列里');
    await user.click(screen.getByRole('button', { name: '发布到个人动态' }));
    expect(apiMocks.replace).toHaveBeenCalledTimes(1);

    completeProfileSave({ result_revision: 1, account_change_sequence: 1 });
    await waitFor(() => expect(apiMocks.replace).toHaveBeenCalledTimes(2));
    expect(apiMocks.replace.mock.calls[1]?.slice(0, 4)).toEqual([
      'feed_post', expect.any(String), 0,
      expect.objectContaining({ title: '同步期间新增', body: '这条内容不能留在队列里' }),
    ]);
  });

  it('keeps memory input when local queue persistence fails', async () => {
    const user = userEvent.setup();
    renderPage();
    await screen.findByText(/保存后自动更新云端/);
    await user.click(screen.getByRole('link', { name: '记忆' }));
    await user.type(screen.getByLabelText('新增记忆'), '保留这段输入');
    vi.spyOn(PersonalDataStore.prototype, 'queueReplace').mockRejectedValueOnce(new Error('本地队列不可用'));

    await user.click(screen.getByRole('button', { name: '保存记忆' }));

    expect(await screen.findByRole('alert')).toHaveTextContent('本地队列不可用');
    expect(screen.getByLabelText('新增记忆')).toHaveValue('保留这段输入');
  });

  it('keeps feed input when local queue persistence fails', async () => {
    const user = userEvent.setup();
    renderPage();
    await screen.findByText(/保存后自动更新云端/);
    await user.click(screen.getByRole('link', { name: '动态' }));
    await user.type(screen.getByLabelText('新增动态标题'), '保留标题');
    await user.type(screen.getByLabelText('内容'), '保留正文');
    vi.spyOn(PersonalDataStore.prototype, 'queueReplace').mockRejectedValueOnce(new Error('本地队列不可用'));

    await user.click(screen.getByRole('button', { name: '发布到个人动态' }));

    expect(await screen.findByRole('alert')).toHaveTextContent('本地队列不可用');
    expect(screen.getByLabelText('新增动态标题')).toHaveValue('保留标题');
    expect(screen.getByLabelText('内容')).toHaveValue('保留正文');
  });

  it('shows a retryable sync error and retries without clearing local data', async () => {
    const user = userEvent.setup();
    apiMocks.snapshot.mockRejectedValueOnce(new Error('网络暂不可用')).mockResolvedValueOnce({ watermark: 0, items: [], next_cursor: null });
    renderPage();
    expect(await screen.findByRole('alert')).toHaveTextContent('网络暂不可用');
    await user.click(screen.getByRole('button', { name: '重试保存' }));
    await waitFor(() => expect(apiMocks.snapshot).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(screen.queryByRole('alert')).not.toBeInTheDocument());
  });

  it('saves feed preferences from the preferences tab', async () => {
    const user = userEvent.setup();
    renderPage();
    await screen.findByText(/保存后自动更新云端/);
    await user.click(screen.getByRole('link', { name: '偏好' }));
    await user.type(screen.getByLabelText('动态偏好'), '关注人工智能');
    await user.click(screen.getByRole('button', { name: '保存动态偏好' }));
    await waitFor(() => expect(apiMocks.replace).toHaveBeenCalledWith(
      'feed_preferences', 'default', 0, { content: '关注人工智能' }, expect.any(String), currentUser.id, 'service-personal-test',
    ));
  });

  it('defaults personal memory to on and only shows the server-confirmed setting', async () => {
    const user = userEvent.setup();
    let confirm!: (value: { result_revision: number; account_change_sequence: number }) => void;
    apiMocks.replace.mockImplementationOnce(() => new Promise((resolve) => { confirm = resolve; }));
    renderPage();
    await screen.findByText(/保存后自动更新云端/);
    await user.click(screen.getByRole('link', { name: '记忆' }));
    const toggle = screen.getByRole('checkbox', { name: '允许 AI 使用个人记忆' });
    expect(toggle).toBeChecked();
    expect(screen.getByText(/默认开启；关闭后/)).toBeInTheDocument();
    await user.click(toggle);
    await waitFor(() => expect(apiMocks.replace).toHaveBeenCalledWith(
      'skill_setting', 'builtin_memory', 0, { enabled: false }, expect.any(String), currentUser.id, 'service-personal-test',
    ));
    expect(toggle).toBeChecked();
    expect(toggle).toBeDisabled();
    expect(screen.getByText('设置正在同步，服务端确认后生效。')).toBeInTheDocument();
    confirm({ result_revision: 1, account_change_sequence: 1 });
    await waitFor(() => expect(toggle).not.toBeChecked());
  });

  it('creates an owner-scoped memory through the personal-object API', async () => {
    const user = userEvent.setup();
    renderPage();
    await screen.findByText(/保存后自动更新云端/);
    await user.click(screen.getByRole('link', { name: '记忆' }));
    await user.type(screen.getByLabelText('新增记忆'), '喜欢先看结论，再看细节。');
    await user.type(screen.getByLabelText('分类（可选）'), '沟通偏好');
    await user.click(screen.getByRole('button', { name: '保存记忆' }));

    await waitFor(() => expect(apiMocks.replace).toHaveBeenCalledTimes(1));
    const [type, id, baseRevision, body, idempotencyKey] = apiMocks.replace.mock.calls[0] as [string, string, number, { content: string; category?: string }, string];
    expect(type).toBe('memory');
    expect(id).toMatch(/^m_[0-9a-f]{8}$/);
    expect(baseRevision).toBe(0);
    expect(body).toEqual({ content: '喜欢先看结论，再看细节。', category: '沟通偏好' });
    expect(idempotencyKey).toEqual(expect.any(String));
    expect(apiMocks.submitTask).not.toHaveBeenCalled();
  });

  it('edits an existing memory with its current revision', async () => {
    const user = userEvent.setup();
    apiMocks.snapshot.mockResolvedValue({
      watermark: 4,
      items: [{ object_type: 'memory', object_id: 'm_saved01', revision: 3, body: { content: '原偏好', category: '沟通' } }],
      next_cursor: null,
    });
    renderPage();
    await screen.findByText(/保存后自动更新云端/);
    await user.click(screen.getByRole('link', { name: '记忆' }));
    await user.clear(screen.getByLabelText('记忆内容'));
    await user.type(screen.getByLabelText('记忆内容'), '更新后的偏好');
    await user.clear(screen.getByLabelText('分类'));
    await user.type(screen.getByLabelText('分类'), '格式');
    await user.click(screen.getByRole('button', { name: '保存修改' }));

    await waitFor(() => expect(apiMocks.replace).toHaveBeenCalledWith(
      'memory', 'm_saved01', 3, { content: '更新后的偏好', category: '格式' }, expect.any(String), currentUser.id, 'service-personal-test',
    ));
    expect(apiMocks.submitTask).not.toHaveBeenCalled();
  });

  it('deletes an existing memory with its current revision', async () => {
    const user = userEvent.setup();
    apiMocks.snapshot.mockResolvedValue({
      watermark: 5,
      items: [{ object_type: 'memory', object_id: 'm_saved02', revision: 7, body: { content: '待删除记忆' } }],
      next_cursor: null,
    });
    renderPage();
    await screen.findByText(/保存后自动更新云端/);
    await user.click(screen.getByRole('link', { name: '记忆' }));
    await user.click(screen.getByRole('button', { name: '删除' }));

    await waitFor(() => expect(apiMocks.delete).toHaveBeenCalledWith(
      'memory', 'm_saved02', 7, expect.any(String), 'service-personal-test', currentUser.id,
    ));
    expect(apiMocks.submitTask).not.toHaveBeenCalled();
  });

  it('saves a personal Morde SOUL override through the owner-scoped personal-object API', async () => {
    const user = userEvent.setup();
    renderPage();
    await screen.findByText(/保存后自动更新云端/);
    await user.click(screen.getByRole('link', { name: 'AI 个性' }));
    expect(screen.getByText('当前使用系统默认的 Morde 人格。保存后会为你的个人对话创建覆盖版本。')).toBeInTheDocument();
    await user.type(screen.getByLabelText('SOUL 内容'), '简洁回答；先检查现有信息再提问。');
    await user.click(screen.getByRole('button', { name: '保存个人人格' }));
    await waitFor(() => expect(apiMocks.replace).toHaveBeenCalledWith(
      'soul', 'default', 0,
      { name: 'Morde', lang: 'zh-CN', content: '简洁回答；先检查现有信息再提问。' },
      expect.any(String), currentUser.id, 'service-personal-test',
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
    await screen.findByText(/保存后自动更新云端/);
    await user.click(screen.getByRole('link', { name: 'AI 个性' }));
    expect(screen.getByLabelText('SOUL 内容')).toHaveValue('Use short answers.');
    await user.click(screen.getByRole('button', { name: '删除个人人格' }));
    await waitFor(() => expect(apiMocks.delete).toHaveBeenCalledWith(
      'soul', 'default', 3, expect.any(String), 'service-personal-test', currentUser.id,
    ));
  });

  it('carries an idea into Assistant route state without putting prompt text in the URL or submitting a task', async () => {
    const user = userEvent.setup();
    renderPage();
    await screen.findByText(/保存后自动更新云端/);
    await user.click(screen.getByRole('link', { name: '灵感' }));
    expect(screen.getByRole('heading', { name: /整理近期 AI 新闻/ })).toBeInTheDocument();
    expect(screen.queryByText(/盯一趟 12306/)).not.toBeInTheDocument();
    await user.click(screen.getAllByRole('link', { name: '带入助手' })[0]!);

    const location = JSON.parse(screen.getByLabelText('助手路由状态').textContent ?? '{}') as { pathname: string; search: string; state: { ideaID: string; ideaPrompt: string } };
    expect(location.pathname).toBe('/assistant');
    expect(location.search).toBe('');
    expect(location.state.ideaID).toBe('morde-ai-news');
    expect(location.state.ideaPrompt).toMatch(/搜索近期的 AI 新闻/);
    expect(apiMocks.submitTask).not.toHaveBeenCalled();
  });

  it('offers a contextual next step for an active goal and only prefills Assistant after selection', async () => {
    const user = userEvent.setup();
    apiMocks.snapshot.mockResolvedValue({
      watermark: 1,
      items: [{
        object_type: 'goal', object_id: 'g_learn_go', revision: 4,
        body: { title: '学习 Go', description: '', status: 'active', steps: [
          { id: 's_done', title: '安装 Go', status: 'done' },
          { id: 's_current', title: '写一个 HTTP 服务', status: 'in_progress' },
          { id: 's_next', title: '补充测试', status: 'pending' },
        ], related_material_ids: [] },
      }, {
        object_type: 'goal', object_id: 'g_deploy', revision: 2,
        body: { title: '完成部署', description: '', status: 'active', steps: [
          { id: 's_blocked', title: '修复服务启动失败', status: 'blocked' },
          { id: 's_pending', title: '发布版本', status: 'pending' },
        ], related_material_ids: [] },
      }],
      next_cursor: null,
    });
    renderPage();
    await screen.findByText(/保存后自动更新云端/);
    await user.click(screen.getByRole('link', { name: '灵感' }));

    expect(screen.getByRole('heading', { name: '🎯 学习 Go' })).toBeInTheDocument();
    expect(screen.getByText('当前步骤：写一个 HTTP 服务')).toBeInTheDocument();
    expect(screen.getByRole('heading', { name: '🎯 完成部署' })).toBeInTheDocument();
    expect(screen.getByText('受阻步骤：修复服务启动失败')).toBeInTheDocument();
    await user.click(screen.getAllByRole('link', { name: '继续目标' })[0]!);

    const location = JSON.parse(screen.getByLabelText('助手路由状态').textContent ?? '{}') as {
      pathname: string; search: string; state: { ideaID: string; ideaPrompt: string };
    };
    expect(location.pathname).toBe('/assistant');
    expect(location.search).toBe('');
    expect(location.state.ideaID).toBe('goal-g_learn_go');
    expect(location.state.ideaPrompt).toContain('学习 Go');
    expect(location.state.ideaPrompt).toContain('写一个 HTTP 服务');
    expect(apiMocks.submitTask).not.toHaveBeenCalled();
  });

  it('turns saved feed preferences into search ideas that only prefill Assistant', async () => {
    const user = userEvent.setup();
    apiMocks.snapshot.mockResolvedValue({
      watermark: 1,
      items: [{ object_type: 'feed_preferences', object_id: 'default', revision: 2, body: { content: '大模型，AI 芯片' } }],
      next_cursor: null,
    });
    renderPage();
    await screen.findByText(/保存后自动更新云端/);
    await user.click(screen.getByRole('link', { name: '灵感' }));

    expect(screen.getByRole('heading', { name: '🔎 搜索：大模型' })).toBeInTheDocument();
    expect(screen.getByRole('heading', { name: '🔎 搜索：AI 芯片' })).toBeInTheDocument();
    await user.click(screen.getAllByRole('link', { name: '按主题搜索' })[0]!);

    const location = JSON.parse(screen.getByLabelText('助手路由状态').textContent ?? '{}') as {
      pathname: string; search: string; state: { ideaID: string; ideaPrompt: string };
    };
    expect(location.pathname).toBe('/assistant');
    expect(location.search).toBe('');
    expect(location.state.ideaPrompt).toContain('主题：大模型');
    expect(apiMocks.submitTask).not.toHaveBeenCalled();
  });

  it('persists the opt-out for personalized suggestions and hides goal/feed ideas', async () => {
    const user = userEvent.setup();
    apiMocks.snapshot.mockResolvedValue({
      watermark: 1,
      items: [
        { object_type: 'feed_preferences', object_id: 'default', revision: 2, body: { content: 'AI 新闻' } },
        { object_type: 'goal', object_id: 'g_active', revision: 3, body: { title: '完成项目', status: 'active', steps: [{ id: 's1', title: '写实施方案', status: 'in_progress' }] } },
      ],
      next_cursor: null,
    });
    renderPage();
    await screen.findByText(/保存后自动更新云端/);
    await user.click(screen.getByRole('link', { name: '灵感' }));

    const toggle = screen.getByRole('checkbox', { name: '显示个性化建议' });
    expect(toggle).toBeChecked();
    expect(screen.getByRole('heading', { name: '🔎 搜索：AI 新闻' })).toBeInTheDocument();
    expect(screen.getByRole('heading', { name: '🎯 完成项目' })).toBeInTheDocument();
    await user.click(toggle);

    await waitFor(() => expect(apiMocks.replace).toHaveBeenCalledWith(
      'skill_setting', 'builtin_ideas', 0, { enabled: false }, expect.any(String), currentUser.id, 'service-personal-test',
    ));
    await waitFor(() => expect(toggle).not.toBeChecked());
    expect(screen.queryByRole('heading', { name: '🔎 搜索：AI 新闻' })).not.toBeInTheDocument();
    expect(screen.queryByRole('heading', { name: '🎯 完成项目' })).not.toBeInTheDocument();
    expect(screen.getByRole('heading', { name: /整理近期 AI 新闻/ })).toBeInTheDocument();
    expect(apiMocks.submitTask).not.toHaveBeenCalled();
  });

  it('keeps automatic Goal follow-ups off by default and persists explicit opt-in', async () => {
    const user = userEvent.setup();
    renderPage();
    await screen.findByText(/保存后自动更新云端/);
    await user.click(screen.getByRole('link', { name: '灵感' }));

    const toggle = screen.getByRole('checkbox', { name: '自动分析新受阻的目标' });
    expect(toggle).not.toBeChecked();
    await user.click(toggle);

    await waitFor(() => expect(apiMocks.replace).toHaveBeenCalledWith(
      'skill_setting', 'builtin_goal_followups', 0, { enabled: true }, expect.any(String), currentUser.id, 'service-personal-test',
    ));
    await waitFor(() => expect(toggle).toBeChecked());
    expect(apiMocks.submitTask).not.toHaveBeenCalled();
  });

  it('persists disabling automatic Goal follow-ups', async () => {
    const user = userEvent.setup();
    apiMocks.snapshot.mockResolvedValue({
      watermark: 1,
      items: [{ object_type: 'skill_setting', object_id: 'builtin_goal_followups', revision: 4, body: { enabled: true } }],
      next_cursor: null,
    });
    renderPage();
    await screen.findByText(/保存后自动更新云端/);
    await user.click(screen.getByRole('link', { name: '灵感' }));

    const toggle = screen.getByRole('checkbox', { name: '自动分析新受阻的目标' });
    expect(toggle).toBeChecked();
    await user.click(toggle);

    await waitFor(() => expect(apiMocks.replace).toHaveBeenCalledWith(
      'skill_setting', 'builtin_goal_followups', 4, { enabled: false }, expect.any(String), currentUser.id, 'service-personal-test',
    ));
    await waitFor(() => expect(toggle).not.toBeChecked());
  });

  it('does not suggest completed, paused, or fully completed goals', async () => {
    const user = userEvent.setup();
    apiMocks.snapshot.mockResolvedValue({
      watermark: 1,
      items: [
        { object_type: 'goal', object_id: 'g_done', revision: 1, body: { title: '已完成目标', status: 'done', steps: [{ title: 'done', status: 'done' }] } },
        { object_type: 'goal', object_id: 'g_paused', revision: 1, body: { title: '已暂停目标', status: 'paused', steps: [{ title: 'pending', status: 'pending' }] } },
        { object_type: 'goal', object_id: 'g_steps_done', revision: 1, body: { title: '步骤已完成', status: 'active', steps: [{ title: 'done', status: 'done' }] } },
      ],
      next_cursor: null,
    });
    renderPage();
    await screen.findByText(/保存后自动更新云端/);
    await user.click(screen.getByRole('link', { name: '灵感' }));

    expect(screen.queryByRole('heading', { name: /已完成目标|已暂停目标|步骤已完成/ })).not.toBeInTheDocument();
    expect(screen.queryByRole('link', { name: '继续目标' })).not.toBeInTheDocument();
  });

});
