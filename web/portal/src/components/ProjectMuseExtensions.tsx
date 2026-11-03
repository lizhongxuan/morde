import { useRef, useState } from 'react';

type Access = 'editable' | 'read_only' | 'none';
type Priority = 'urgent' | 'todo' | 'normal';
type Collection = 'feed' | 'ideas' | 'library' | 'brief';
type Project = { id: string; name: string; access: Access };
type Notification = { id: string; priority: Priority; title: string; read: boolean };
type SearchRecord = { title: string; kind: Collection };
type DemoState = {
  notifications: Notification[];
  skillGranted: boolean;
  skillRevoked: boolean;
  connectorGranted: boolean;
  connectorRevoked: boolean;
  templatePreview: boolean;
  comments: string[];
  collections: Record<Collection, string[]>;
  searchRecords: SearchRecord[];
};

const projects: Project[] = [
  { id: 'project-alpha', name: 'Morde 实施协作', access: 'editable' },
  { id: 'project-beta', name: '客户端体验改进', access: 'read_only' },
  { id: 'project-hidden', name: '', access: 'none' },
];

const initialState: Record<string, DemoState> = {
  'project-alpha': {
    notifications: [
      { id: 'alpha-urgent', priority: 'urgent', title: 'Workspace 同步发生冲突，需要关注', read: false },
      { id: 'alpha-todo', priority: 'todo', title: '待补充工作项的项目授权', read: false },
      { id: 'alpha-normal', priority: 'normal', title: '项目文档已更新', read: true },
    ],
    skillGranted: true, skillRevoked: false, connectorGranted: false, connectorRevoked: false,
    templatePreview: false, comments: [],
    collections: {
      feed: ['本周：项目 Workspace 同步完成'],
      ideas: ['为验收项增加负责人和截止时间'],
      library: ['项目实施手册'],
      brief: ['简报：入口检查已完成，权限复核待办'],
    },
    searchRecords: [
      { title: '项目 Workspace 同步完成', kind: 'feed' },
      { title: '负责人和截止时间建议', kind: 'ideas' },
      { title: '项目实施手册', kind: 'library' },
      { title: '入口检查完成摘要', kind: 'brief' },
      { title: '交付验收 revision 4', kind: 'brief' },
    ],
  },
  'project-beta': {
    notifications: [
      { id: 'beta-todo', priority: 'todo', title: '待复核键盘导航顺序', read: false },
      { id: 'beta-normal', priority: 'normal', title: '设计讨论记录已归档', read: true },
    ],
    skillGranted: false, skillRevoked: false, connectorGranted: true, connectorRevoked: false,
    templatePreview: false, comments: [],
    collections: {
      feed: ['客户端键盘导航已进入只读评审'],
      ideas: ['补充焦点状态说明'],
      library: ['无障碍检查清单'],
      brief: ['简报：体验评审已完成'],
    },
    searchRecords: [
      { title: '客户端键盘导航评审', kind: 'feed' },
      { title: '焦点状态说明', kind: 'ideas' },
      { title: '无障碍检查清单', kind: 'library' },
      { title: '体验评审摘要', kind: 'brief' },
    ],
  },
  'project-hidden': {
    notifications: [{ id: 'hidden-urgent', priority: 'urgent', title: 'HIDDEN_PRIVATE_NOTIFICATION', read: false }],
    skillGranted: false, skillRevoked: false, connectorGranted: false, connectorRevoked: false,
    templatePreview: false, comments: [],
    collections: {
      feed: ['HIDDEN_PRIVATE_FEED'], ideas: ['HIDDEN_PRIVATE_IDEA'],
      library: ['HIDDEN_PRIVATE_LIBRARY'], brief: ['HIDDEN_PRIVATE_BRIEF'],
    },
    searchRecords: [{ title: 'HIDDEN_PRIVATE_SEARCH_CANARY_9381', kind: 'brief' }],
  },
};

const collectionTabs: { id: Collection; label: string }[] = [
  { id: 'feed', label: 'Feed' }, { id: 'ideas', label: 'Ideas' },
  { id: 'library', label: 'Library' }, { id: 'brief', label: '简报' },
];

const priorityCopy: Record<Priority, { label: string; className: string }> = {
  urgent: { label: '紧急', className: 'status-failed' },
  todo: { label: '待办', className: 'status-waiting_user' },
  normal: { label: '普通', className: 'status-disabled' },
};

const collectionLabel: Record<Collection, string> = { feed: 'Feed', ideas: 'Ideas', library: 'Library', brief: '简报' };

function accessLabel(access: Access) {
  return ({ editable: '可编辑', read_only: '只读', none: '无权' })[access];
}

export function ProjectMuseExtensions() {
  const nextCommentID = useRef(0);
  const [selectedProjectID, setSelectedProjectID] = useState('project-alpha');
  const [demoState, setDemoState] = useState(initialState);
  const [selectedCollection, setSelectedCollection] = useState<Collection>('feed');
  const [commentText, setCommentText] = useState('');
  const [searchText, setSearchText] = useState('');
  const [speechText, setSpeechText] = useState('');
  const [deviceAuthorized, setDeviceAuthorized] = useState(false);
  const [deviceActionUsed, setDeviceActionUsed] = useState(false);

  const project = projects.find((item) => item.id === selectedProjectID) ?? projects[0]!;
  const state = demoState[project.id]!;
  const canEdit = project.access === 'editable';
  const accessibleProjects = projects.filter((item) => item.access !== 'none');
  const query = searchText.trim().toLocaleLowerCase();
  const searchResults = query
    ? accessibleProjects.flatMap((item) => demoState[item.id]!.searchRecords
      .filter((record) => record.title.toLocaleLowerCase().includes(query))
      .map((record) => ({ ...record, project: item.name })))
    : [];

  const updateProject = (update: (current: DemoState) => DemoState) => {
    setDemoState((current) => ({ ...current, [project.id]: update(current[project.id]!) }));
  };

  const markNotificationRead = (notificationID: string) => {
    updateProject((current) => ({
      ...current,
      notifications: current.notifications.map((item) => item.id === notificationID ? { ...item, read: true } : item),
    }));
  };

  const setSkillGranted = (granted: boolean) => updateProject((current) => ({ ...current, skillGranted: granted, skillRevoked: !granted }));
  const setConnectorGranted = (granted: boolean) => updateProject((current) => ({ ...current, connectorGranted: granted, connectorRevoked: !granted }));

  const addComment = (event: React.FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const text = commentText.trim();
    if (!canEdit || !text) return;
    const id = ++nextCommentID.current;
    updateProject((current) => ({ ...current, comments: [...current.comments, `演示批注 ${id}：${text}`] }));
    setCommentText('');
  };

  return <section aria-labelledby="project-muse-title">
    <div className="connection-section-heading"><div><p className="eyebrow">PC11 / PC12 · 客户端扩展</p><h2 id="project-muse-title">Muse 项目能力演示</h2><p className="muted">用于核对权限、空态和跨项目隔离状态。</p></div></div>
    <div className="notice notice-warning" role="note"><strong>本地演示</strong>：刷新即丢；不连接服务、不调用外部服务、不生成或上传文件。下方所有交互只改变当前页面组件状态。</div>

    <section className="connection-section" aria-labelledby="demo-project-title">
      <div className="connection-section-heading"><div><h3 id="demo-project-title">演示项目与权限</h3><p className="muted">当前项目：{project.name} · {accessLabel(project.access)}。只读项目可浏览，不能更改授权或添加批注。</p></div></div>
      <label htmlFor="muse-demo-project">当前演示项目</label>
      <select id="muse-demo-project" value={selectedProjectID} onChange={(event) => setSelectedProjectID(event.target.value)}>
        {accessibleProjects.map((item) => <option key={item.id} value={item.id}>{item.name}（{accessLabel(item.access)}）</option>)}
      </select>
      <p className="muted">本地样例另含一个无权项目；它的名称、内容和搜索结果均不会显示。</p>
    </section>

    <section className="connection-section" aria-labelledby="demo-notifications-title">
      <div className="connection-section-heading"><div><h3 id="demo-notifications-title">通知分级</h3><p className="muted">紧急、待办和普通通知分别标识；标记已读仅更新本地演示状态。</p></div></div>
      {!state.notifications.length ? <div className="empty-card"><h4>当前项目暂无演示通知</h4></div> : <div className="schedule-list">{state.notifications.map((item) => <article className="schedule-card" key={item.id}>
        <div className="schedule-card-heading"><div><h4>{item.title}</h4><span className={`status ${priorityCopy[item.priority].className}`}>{priorityCopy[item.priority].label}</span></div><span className="muted">{item.read ? '已读' : '未读'}</span></div>
        {!item.read && <button type="button" className="button button-quiet button-small" onClick={() => markNotificationRead(item.id)}>标记已读（本地演示）</button>}
      </article>)}</div>}
    </section>

    <section className="connection-section" aria-labelledby="demo-device-title">
      <div className="connection-section-heading"><div><h3 id="demo-device-title">设备接力与单次动作</h3><p className="muted">只模拟使用已获准电脑执行一次动作；不会连接电脑、打开桌面或传输文件。</p></div></div>
      <button type="button" className="button button-secondary" disabled={deviceAuthorized} onClick={() => setDeviceAuthorized(true)}>{deviceAuthorized ? '演示电脑已获准' : '模拟授权已获准电脑（本地演示）'}</button>
      <button type="button" className="button button-secondary" disabled={!deviceAuthorized || deviceActionUsed} onClick={() => setDeviceActionUsed(true)}>{deviceActionUsed ? '单次动作已使用' : '执行一次电脑动作（本地演示）'}</button>
      {deviceActionUsed && <p className="notice notice-info" role="status">单次动作已在本地演示完成；没有连接电脑或传输文件。</p>}
    </section>

    <section className="connection-section" aria-labelledby="demo-project-tools-title">
      <div className="connection-section-heading"><div><h3 id="demo-project-tools-title">项目技能与连接器授权</h3><p className="muted">授权状态按当前项目隔离；撤销后此项目的对应能力显示为不可用。</p></div></div>
      <div className="schedule-list">
        <article className="schedule-card">
          <div className="schedule-card-heading"><div><h4>项目技能：文档整理</h4><p className="muted">只在当前项目的演示范围内启用。</p></div><span className={`status ${state.skillGranted ? 'status-enabled' : 'status-disabled'}`}>{state.skillGranted ? '已授权' : state.skillRevoked ? '已撤销' : '未授权'}</span></div>
          {state.skillRevoked && <p className="notice notice-warning" role="status">技能授权已撤销；本地演示不会再把该技能用于此项目。</p>}
          <button type="button" className="button button-secondary" disabled={!canEdit} onClick={() => setSkillGranted(!state.skillGranted)}>{state.skillGranted ? '撤销技能授权（本地演示）' : '授权项目技能（本地演示）'}</button>
        </article>
        <article className="schedule-card">
          <div className="schedule-card-heading"><div><h4>项目连接器：代码仓库</h4><p className="muted">连接器授权不会自动延伸到其他项目。</p></div><span className={`status ${state.connectorGranted ? 'status-enabled' : 'status-disabled'}`}>{state.connectorGranted ? '已授权' : state.connectorRevoked ? '已撤销' : '未授权'}</span></div>
          {state.connectorRevoked && <p className="notice notice-warning" role="status">连接器授权已撤销；本地演示不会派发新的连接器操作。</p>}
          <button type="button" className="button button-secondary" disabled={!canEdit} onClick={() => setConnectorGranted(!state.connectorGranted)}>{state.connectorGranted ? '撤销连接器授权（本地演示）' : '授权项目连接器（本地演示）'}</button>
        </article>
      </div>
      {!canEdit && <p className="notice notice-info">当前项目为只读；项目技能和连接器授权按钮已禁用。</p>}
    </section>

    <section className="connection-section" aria-labelledby="demo-template-title">
      <div className="connection-section-heading"><div><h3 id="demo-template-title">文档模板</h3><p className="muted">模板列表为空；本地预演只展示结构，不创建或保存文件。</p></div></div>
      <div className="empty-card"><h4>当前项目没有已发布的文档模板</h4><p>可以预览一个本地结构样例，预览结果不会保存。</p></div>
      <button type="button" className="button button-secondary" disabled={!canEdit} onClick={() => updateProject((current) => ({ ...current, templatePreview: true }))}>预演文档模板（本地演示）</button>
      {state.templatePreview && <div className="notice notice-info" role="status"><strong>本地预演结果：</strong>项目目标 → 工作项 → 交付记录。未创建、未保存文件。</div>}
      {!canEdit && <p className="muted">只读项目不能启动本地模板预演。</p>}
    </section>

    <section className="connection-section" aria-labelledby="demo-comments-title">
      <div className="connection-section-heading"><div><h3 id="demo-comments-title">行级批注</h3><p className="muted">示例行：项目目标与验收范围。</p></div></div>
      <article className="schedule-card"><h4>项目目标与验收范围</h4><p className="muted">本地演示中的项目文档行，不对应真实 Workspace 文件。</p>
        {state.comments.length > 0 && <ul aria-label="本地演示批注">{state.comments.map((comment) => <li key={comment}>{comment}</li>)}</ul>}
        {canEdit ? <form onSubmit={addComment}>
          <label htmlFor="demo-row-comment">添加行级批注（本地演示）</label>
          <textarea id="demo-row-comment" value={commentText} onChange={(event) => setCommentText(event.target.value)} maxLength={500} placeholder="写一条本地演示批注" />
          <button type="submit" className="button button-secondary" disabled={!commentText.trim()}>添加批注（本地演示）</button>
        </form> : <p className="notice notice-info" role="status">只读项目不能添加行级批注；没有可写权限。</p>}
      </article>
    </section>

    <section className="connection-section" aria-labelledby="demo-search-title">
      <div className="connection-section-heading"><div><h3 id="demo-search-title">跨项目搜索</h3><p className="muted">只搜索当前账号有权读取的项目；无权项目的内容不会进入结果。</p></div></div>
      <label htmlFor="demo-project-search">搜索可访问项目（本地演示）</label>
      <input id="demo-project-search" value={searchText} onChange={(event) => setSearchText(event.target.value)} placeholder="输入项目内容关键词" />
      {!query ? <p className="muted">输入关键词搜索两个可访问的演示项目。</p>
        : searchResults.length ? <div className="schedule-list" aria-label="跨项目搜索结果">{searchResults.map((result, index) => <article className="schedule-card" key={`${result.project}-${result.kind}-${index}`}>
          <p className="muted">{result.project} · {collectionLabel[result.kind]}</p><strong>{result.title}</strong>
        </article>)}</div>
          : <div className="empty-card"><h4>没有可访问的匹配结果</h4><p>无权项目的内容已从搜索范围排除。</p></div>}
    </section>

    <section className="connection-section" aria-labelledby="demo-collections-title">
      <div className="connection-section-heading"><div><h3 id="demo-collections-title">Muse 内容</h3><p className="muted">Feed、Ideas、Library 和简报只展示当前选择项目的数据。</p></div></div>
      <div role="tablist" aria-label="项目内容分类" className="notification-filters">
        {collectionTabs.map((tab) => <button type="button" role="tab" aria-selected={selectedCollection === tab.id} className={`button button-small ${selectedCollection === tab.id ? 'button-primary' : 'button-secondary'}`} key={tab.id} onClick={() => setSelectedCollection(tab.id)}>{tab.label}（本地演示）</button>)}
      </div>
      <div role="tabpanel" aria-label={`${collectionLabel[selectedCollection]} 当前项目内容`}>
        {state.collections[selectedCollection].length ? <ul>{state.collections[selectedCollection].map((entry) => <li key={entry}>{entry}</li>)}</ul> : <div className="empty-card"><h4>当前项目没有{collectionLabel[selectedCollection]}演示内容</h4></div>}
      </div>
    </section>

    <section className="connection-section" aria-labelledby="demo-speech-title">
      <div className="connection-section-heading"><div><h3 id="demo-speech-title">语音输入</h3><p className="muted">模拟转写仅更新页面文本，不会请求麦克风权限。</p></div></div>
      <button type="button" className="button button-secondary" onClick={() => setSpeechText('模拟语音文本：请检查本周项目交付状态。')}>模拟语音输入（本地演示）</button>
      {speechText && <p className="notice notice-info" role="status">{speechText}</p>}
      <p className="muted">此控件不会调用 SpeechRecognition、getUserMedia 或其他语音服务。</p>
    </section>

    <section className="connection-section" aria-labelledby="demo-media-title">
      <div className="connection-section-heading"><div><h3 id="demo-media-title">媒体入口</h3><p className="muted">只展示服务就绪状态，不会选择模型或提交生成请求。</p></div><span className="status status-disabled">服务未就绪</span></div>
      <div className="empty-card"><h4>图片与视频服务尚未就绪</h4><p>服务未就绪期间，此入口不可用，也不会调用 AI 模型。</p><button type="button" className="button button-secondary" disabled>媒体生成未就绪（本地演示）</button></div>
    </section>
  </section>;
}
