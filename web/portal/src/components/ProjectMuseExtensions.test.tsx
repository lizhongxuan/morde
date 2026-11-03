import { cleanup, render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { ProjectMuseExtensions } from './ProjectMuseExtensions';

function renderPanel() {
  return render(<ProjectMuseExtensions />);
}

describe('ProjectMuseExtensions local demo', () => {
  afterEach(() => { cleanup(); vi.restoreAllMocks(); });

  it('clearly discloses local-only state and excludes private project details from the picker', () => {
    renderPanel();

    expect(screen.getByRole('note')).toHaveTextContent('本地演示');
    expect(screen.getByRole('note')).toHaveTextContent('刷新即丢');
    expect(screen.getByRole('note')).toHaveTextContent('不连接服务');
    expect(screen.getByRole('note')).toHaveTextContent('不调用外部服务');
    expect(screen.getByRole('note')).toHaveTextContent('不生成或上传文件');
    expect(screen.getAllByRole('option')).toHaveLength(2);
    expect(screen.getByText(/无权项目.*名称、内容和搜索结果均不会显示/)).toBeInTheDocument();
    expect(screen.queryByText('HIDDEN_PRIVATE_FEED')).not.toBeInTheDocument();
  });

  it('keeps Feed, Ideas, Library, and brief content isolated by selected project', async () => {
    const user = userEvent.setup();
    renderPanel();

    const collections = [
      { tab: 'Feed（本地演示）', alpha: '本周：项目 Workspace 同步完成', beta: '客户端键盘导航已进入只读评审' },
      { tab: 'Ideas（本地演示）', alpha: '为验收项增加负责人和截止时间', beta: '补充焦点状态说明' },
      { tab: 'Library（本地演示）', alpha: '项目实施手册', beta: '无障碍检查清单' },
      { tab: '简报（本地演示）', alpha: '简报：入口检查已完成，权限复核待办', beta: '简报：体验评审已完成' },
    ];
    for (const collection of collections) {
      await user.click(screen.getByRole('tab', { name: collection.tab }));
      expect(screen.getByText(collection.alpha)).toBeInTheDocument();
      expect(screen.queryByText(collection.beta)).not.toBeInTheDocument();
    }

    await user.selectOptions(screen.getByLabelText('当前演示项目'), 'project-beta');
    for (const collection of collections) {
      await user.click(screen.getByRole('tab', { name: collection.tab }));
      expect(screen.getByText(collection.beta)).toBeInTheDocument();
      expect(screen.queryByText(collection.alpha)).not.toBeInTheDocument();
    }
  });

  it('allows row comments only in an editable project and keeps comments project-scoped', async () => {
    const user = userEvent.setup();
    renderPanel();

    await user.type(screen.getByLabelText('添加行级批注（本地演示）'), '确认验收范围');
    await user.click(screen.getByRole('button', { name: '添加批注（本地演示）' }));
    expect(screen.getByText('演示批注 1：确认验收范围')).toBeInTheDocument();

    await user.selectOptions(screen.getByLabelText('当前演示项目'), 'project-beta');
    expect(screen.getByText('只读项目不能添加行级批注；没有可写权限。')).toBeInTheDocument();
    expect(screen.queryByLabelText('添加行级批注（本地演示）')).not.toBeInTheDocument();
    expect(screen.queryByText('演示批注 1：确认验收范围')).not.toBeInTheDocument();
  });

  it('searches readable projects and never returns a no-access project record', async () => {
    const user = userEvent.setup();
    renderPanel();
    const search = screen.getByLabelText('搜索可访问项目（本地演示）');

    await user.type(search, '交付验收');
    expect(screen.getByText('交付验收 revision 4')).toBeInTheDocument();
    expect(screen.getByText('Morde 实施协作 · 简报')).toBeInTheDocument();

    await user.clear(search);
    await user.type(search, '键盘导航');
    expect(screen.getByText('客户端键盘导航评审')).toBeInTheDocument();
    expect(screen.getByText('客户端体验改进 · Feed')).toBeInTheDocument();

    await user.clear(search);
    await user.type(search, 'HIDDEN_PRIVATE_SEARCH_CANARY_9381');
    expect(screen.getByText('没有可访问的匹配结果')).toBeInTheDocument();
    expect(screen.queryByText(/HIDDEN_PRIVATE/)).not.toBeInTheDocument();
  });

  it('shows urgency levels and marks a notification read in local state', async () => {
    const user = userEvent.setup();
    renderPanel();

    expect(screen.getByText('紧急')).toBeInTheDocument();
    expect(screen.getByText('待办', { selector: 'span.status' })).toBeInTheDocument();
    expect(screen.getByText('普通')).toBeInTheDocument();
    const urgent = screen.getByText('Workspace 同步发生冲突，需要关注').closest('article');
    expect(urgent).not.toBeNull();
    await user.click(within(urgent!).getByRole('button', { name: '标记已读（本地演示）' }));
    expect(within(urgent!).getByText('已读')).toBeInTheDocument();
  });

  it('allows one in-memory action only after the device is authorized', async () => {
    const user = userEvent.setup();
    renderPanel();

    const action = screen.getByRole('button', { name: '执行一次电脑动作（本地演示）' });
    expect(action).toBeDisabled();
    await user.click(screen.getByRole('button', { name: '模拟授权已获准电脑（本地演示）' }));
    expect(action).toBeEnabled();
    await user.click(action);
    expect(screen.getByRole('status')).toHaveTextContent('单次动作已在本地演示完成');
    expect(screen.getByRole('button', { name: '单次动作已使用' })).toBeDisabled();
  });

  it('authorizes and revokes project skills and connectors without changing another project', async () => {
    const user = userEvent.setup();
    renderPanel();

    const connector = screen.getByRole('heading', { name: '项目连接器：代码仓库' }).closest('article');
    expect(connector).not.toBeNull();
    expect(within(connector!).getByText('未授权')).toBeInTheDocument();
    await user.click(within(connector!).getByRole('button', { name: '授权项目连接器（本地演示）' }));
    expect(within(connector!).getByText('已授权')).toBeInTheDocument();
    await user.click(within(connector!).getByRole('button', { name: '撤销连接器授权（本地演示）' }));
    expect(within(connector!).getByText('已撤销')).toBeInTheDocument();
    expect(within(connector!).getByText(/不会派发新的连接器操作/)).toBeInTheDocument();

    const skill = screen.getByRole('heading', { name: '项目技能：文档整理' }).closest('article');
    expect(skill).not.toBeNull();
    await user.click(within(skill!).getByRole('button', { name: '撤销技能授权（本地演示）' }));
    expect(within(skill!).getByText('已撤销')).toBeInTheDocument();

    await user.selectOptions(screen.getByLabelText('当前演示项目'), 'project-beta');
    const readOnlyConnector = screen.getByRole('heading', { name: '项目连接器：代码仓库' }).closest('article');
    expect(readOnlyConnector).not.toBeNull();
    expect(within(readOnlyConnector!).getByText('已授权')).toBeInTheDocument();
    expect(within(readOnlyConnector!).getByRole('button', { name: '撤销连接器授权（本地演示）' })).toBeDisabled();
    expect(screen.getByText('当前项目为只读；项目技能和连接器授权按钮已禁用。')).toBeInTheDocument();
  });

  it('shows an empty template state and a non-persistent local preview', async () => {
    const user = userEvent.setup();
    renderPanel();

    expect(screen.getByText('当前项目没有已发布的文档模板')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: '预演文档模板（本地演示）' }));
    expect(screen.getByRole('status')).toHaveTextContent('本地预演结果：项目目标 → 工作项 → 交付记录。未创建、未保存文件。');
  });

  it('simulates speech locally and keeps media generation disabled while services are unready', async () => {
    const user = userEvent.setup();
    const fetchSpy = vi.spyOn(globalThis, 'fetch');
    renderPanel();

    await user.click(screen.getByRole('button', { name: '模拟语音输入（本地演示）' }));
    expect(screen.getByText('模拟语音文本：请检查本周项目交付状态。')).toBeInTheDocument();
    expect(screen.getByText(/不会请求麦克风权限/)).toBeInTheDocument();
    expect(screen.getByText(/不会调用 SpeechRecognition、getUserMedia/)).toBeInTheDocument();
    expect(screen.getByText('图片与视频服务尚未就绪')).toBeInTheDocument();
    expect(screen.getByText(/不会调用 AI 模型/)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: '媒体生成未就绪（本地演示）' })).toBeDisabled();
    expect(fetchSpy).not.toHaveBeenCalled();
  });
});
