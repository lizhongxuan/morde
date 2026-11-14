const api = window.morde;
const $ = (id) => document.getElementById(id);
const SCOPE_OPTIONS = [
  ['read', '读取历史', '同步此配置下全部会话的可见历史'],
  ['live', '实时更新', '同步新消息、进度和工具输出'],
  ['send', '发送消息', '允许从 iOS 向原会话发送消息'],
  ['interrupt', '中断任务', '允许从 iOS 停止原会话的运行任务'],
  ['approval', '处理审批', '允许从 iOS 处理 Codex 操作审批'],
];
const DEFAULT_SCOPES = ['read', 'live', 'send'];
let state = null;
let threads = [];
let nextCursor = null;
let loading = false;
let loadingProfile = null;
let threadRequest = 0;
let profileActionBusy = false;
let activeProfile = null;

function element(tag, className, text) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text !== undefined) node.textContent = text;
  return node;
}
function errorText(error) {
  return (error?.message || String(error)).replace(/^Error invoking remote method '[^']+': Error: /, '');
}
function message(text = '', error = false) {
  $('message').textContent = text;
  $('message').className = error ? 'error' : '';
}
function selectedProfile() {
  return state?.codex?.profiles.find((profile) => profile.profileKey === state.codex.selectedProfileKey) ?? null;
}
function sessionFor(thread) {
  return state?.codex?.sessions.find((session) => session.profileKey === state.codex.selectedProfileKey && session.threadId === thread.id);
}
function scopeInputs(profile) {
  const list = $('connection-scopes');
  list.replaceChildren();
  const granted = new Set(profile?.connectionAuthorized ? profile.scopes : DEFAULT_SCOPES);
  for (const [scope, title, detail] of SCOPE_OPTIONS) {
    const label = element('label', 'scope-option');
    const input = document.createElement('input');
    input.type = 'checkbox';
    input.value = scope;
    input.checked = granted.has(scope);
    const words = element('span');
    words.append(element('strong', '', title), element('small', '', detail));
    label.append(input, words);
    list.append(label);
  }
}
function syncLabel(profile) {
  if (!profile?.connectionAuthorized) return '尚未连接';
  if (profile.syncState === 'syncing') return `正在同步会话 ${profile.syncDone}/${profile.syncTotal}`;
  if (profile.syncState === 'partial') return `会话发现部分完成：${profile.syncDone}/${profile.syncTotal}`;
  if (profile.syncState === 'synced') return `会话发现完成：${profile.syncDone}/${profile.syncTotal}；历史按各会话状态继续补齐`;
  return '已连接，等待会话发现';
}
function renderThreads() {
  const root = $('threads');
  root.replaceChildren();
  if (!threads.length) {
    if (!loading && state?.codex?.selectedProfileKey) root.append(element('div', 'empty', '没有找到 Codex 会话。可以刷新，或确认所选配置下已有 Codex 会话。'));
    $('load-more').hidden = true;
    return;
  }
  for (const thread of threads) {
    const session = sessionFor(thread);
    const profile = selectedProfile();
    const card = element('article', 'thread-card');
    card.dataset.threadId = thread.id;
    const heading = element('div', 'thread-title');
    heading.append(element('h3', '', thread.title || '未命名会话'));
    const badge = session
      ? session.snapshotState === 'unknown' ? '同步异常' : session.historyComplete ? '已同步' : '补齐历史中'
      : profile?.connectionAuthorized ? profile.syncState === 'syncing' ? '等待发现' : '尚未同步' : '未连接';
    heading.append(element('span', `badge${session ? '' : ' muted-badge'}`, `${thread.archived ? '归档 · ' : ''}${badge}`));
    card.append(heading, element('p', 'subtle thread-meta', [thread.cwdLabel, thread.updatedAt, thread.status].filter(Boolean).join(' · ') || '本机 Codex 会话'));
    if (session?.snapshotMessage) card.append(element('p', 'sync-warning', session.snapshotMessage));
    root.append(card);
  }
  $('load-more').hidden = !nextCursor || loading || profileActionBusy;
}
function renderState(value) {
  const previousProfile = state?.codex?.selectedProfileKey ?? null;
  state = value;
  const codex = value?.codex;
  $('account-status').textContent = value?.authenticated ? 'Morde 已登录；Codex 凭据保存在本机。' : '请先登录 Morde，再连接 Codex。';
  $('add-profile').disabled = !value?.authenticated || loading || profileActionBusy;
  $('profile-select').disabled = !value?.authenticated || !codex?.profiles.length || loading || profileActionBusy;
  $('refresh').disabled = !value?.authenticated || !codex?.selectedProfileKey || loading || profileActionBusy;
  if (!value?.authenticated) {
    $('profile-status').textContent = '请先登录 Morde，随后添加 Codex 的 CODEX_HOME 和命令文件。';
    $('connect-profile').disabled = true;
    $('revoke-profile').hidden = true;
    $('connection-scopes').replaceChildren();
    $('sync-status').textContent = '';
    threadRequest++;
    loading = false;
    loadingProfile = null;
    threads = [];
    activeProfile = null;
    renderThreads();
    return;
  }
  const select = $('profile-select');
  select.replaceChildren();
  for (const profile of codex.profiles) {
    const option = element('option', '', `${profile.label} · ${profile.executableLabel} · ${profile.state}`);
    option.value = profile.profileKey;
    select.append(option);
  }
  const profile = selectedProfile();
  if (!codex.profiles.length) {
    select.append(element('option', '', '尚未添加配置'));
    $('profile-status').textContent = '点击“添加 Codex 配置”，选择本机 Codex 的 CODEX_HOME 文件夹和可执行命令。';
  } else {
    select.value = codex.selectedProfileKey || '';
    $('profile-status').textContent = profile?.error || codex.error || (profile ? `Codex 状态：${profile.state}` : '选择一个 Codex 配置。');
  }
  scopeInputs(profile);
  $('connect-profile').disabled = !profile || !value.authenticated || loading || profileActionBusy;
  $('connect-profile').textContent = profile?.connectionAuthorized ? '更新授权并同步全部会话' : '授权并同步全部会话';
  $('revoke-profile').hidden = !profile?.connectionAuthorized;
  $('revoke-profile').disabled = loading || profileActionBusy;
  $('sync-status').textContent = profile ? [syncLabel(profile), profile.syncError].filter(Boolean).join(' · ') : '';
  if (previousProfile !== codex.selectedProfileKey || activeProfile !== codex.selectedProfileKey) {
    activeProfile = codex.selectedProfileKey;
    threadRequest++;
    loadingProfile = null;
    loading = false;
    threads = [];
    nextCursor = null;
    if (codex.selectedProfileKey) void loadThreads(null);
    else renderThreads();
  } else renderThreads();
}
async function refreshState() {
  try { renderState(await api.state()); }
  catch (error) { message(errorText(error), true); }
}
async function loadThreads(cursor) {
  const profileKey = state?.codex?.selectedProfileKey;
  if (!state?.authenticated || !profileKey || profileActionBusy || loadingProfile === profileKey) return;
  const request = ++threadRequest;
  loadingProfile = profileKey;
  loading = true;
  $('add-profile').disabled = true;
  $('connect-profile').disabled = true;
  $('refresh').disabled = true;
  $('load-more').hidden = true;
  message(cursor ? '正在加载更多会话…' : '正在读取本机 Codex 会话…');
  try {
    const page = await api.listThreads(cursor);
    if (request !== threadRequest || state?.codex?.selectedProfileKey !== profileKey) return;
    const next = cursor ? [...threads, ...page.items] : page.items;
    threads = [...new Map(next.map((thread) => [thread.id, thread])).values()];
    nextCursor = page.nextCursor;
    message(threads.length ? `已读取 ${threads.length} 个会话。${selectedProfile()?.connectionAuthorized ? ` · ${syncLabel(selectedProfile())}` : ''}` : '没有找到 Codex 会话。');
  } catch (error) {
    if (request === threadRequest && state?.codex?.selectedProfileKey === profileKey) {
      message(errorText(error), true);
      if (!cursor) threads = [];
    }
  } finally {
    if (request === threadRequest) {
      loading = false;
      loadingProfile = null;
      renderThreads();
      if (state) renderState(state);
    }
  }
}
function selectedScopes() {
  return [...$('connection-scopes').querySelectorAll('input[type="checkbox"]:checked')].map((input) => input.value);
}

$('add-profile').onclick = async () => {
  message('选择 Codex 配置文件夹和命令…');
  $('add-profile').disabled = true;
  try { await api.addProfile(); await refreshState(); }
  catch (error) { message(errorText(error), true); }
  finally { $('add-profile').disabled = false; }
};
$('profile-select').onchange = async (event) => {
  const profileKey = event.target.value;
  if (!profileKey || profileKey === state?.codex?.selectedProfileKey) return;
  try { await api.selectProfile(profileKey); await refreshState(); }
  catch (error) { message(errorText(error), true); }
};
$('connect-profile').onclick = async () => {
  const profile = selectedProfile();
  if (!profile || profileActionBusy) return;
  const scopes = selectedScopes();
  if (!scopes.includes('read') || !scopes.includes('live')) { message('连接 Codex 至少需要选择“读取历史”和“实时更新”。', true); return; }
  profileActionBusy = true;
  if (state) renderState(state);
  message('等待确认 Codex 全配置连接…');
  try {
    await api.authorizeProfile(profile.profileKey, scopes);
    await refreshState();
    message('配置连接已提交，正在发现和同步全部普通及归档会话。');
  } catch (error) { message(errorText(error), true); }
  finally { profileActionBusy = false; if (state) renderState(state); }
};
$('revoke-profile').onclick = async () => {
  const profile = selectedProfile();
  if (!profile || profileActionBusy) return;
  profileActionBusy = true;
  if (state) renderState(state);
  try {
    await api.revokeProfile(profile.profileKey);
    await refreshState();
    message('已撤销该 Codex 配置的连接；已同步的云端历史保留。');
  } catch (error) { message(errorText(error), true); }
  finally { profileActionBusy = false; if (state) renderState(state); }
};
$('refresh').onclick = () => void loadThreads(null);
$('load-more').onclick = () => { if (nextCursor) void loadThreads(nextCursor); };
api.onState(renderState);
void api.state().then(renderState).catch((error) => message(errorText(error), true));
