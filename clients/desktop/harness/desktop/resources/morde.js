const api = window.morde;
const $ = (id) => document.getElementById(id);
let state = null;
const syncModes = { upload: '上传到 Morde', download: '下载云端缺失文件', bidirectional: '双向补齐缺失文件', live_sync: '旧版实时同步恢复' };
const syncSettings = new Map();
const liveSyncSettings = new Map();
const liveSyncPending = new Set();
const liveSyncStates = new Set(['idle', 'stopped', 'starting', 'running', 'watching', 'scanning', 'transferring', 'reconnecting', 'syncing', 'stopping', 'active', 'persisted_uncertain', 'conflict', 'failed', 'error', 'unknown', 'unavailable', 'disabled', 'closed', 'not_supported']);
let syncOperation = null;
let syncStatusPanel = null;

function message(value, error = false) {
  $('message').textContent = value;
  $('message').className = error ? 'error' : '';
}
function errorMessage(error) { return (error?.message || String(error)).replace(/^Error invoking remote method '[^']+': Error: /, ''); }
async function run(action) {
  try { message(''); return await action(); }
  catch (error) { message(errorMessage(error), true); return null; }
}
function text(tag, value, className) {
  const element = document.createElement(tag);
  element.textContent = value;
  if (className) element.className = className;
  return element;
}
function button(label, action) {
  const element = text('button', label);
  element.type = 'button';
  element.onclick = () => void run(action);
  return element;
}
function items(value) { return Array.isArray(value?.items) ? value.items : Array.isArray(value) ? value : []; }
function formatBytes(size) {
  if (size < 1024) return `${size} B`;
  if (size < 1024 * 1024) return `${(size / 1024).toFixed(1)} KB`;
  if (size < 1024 * 1024 * 1024) return `${(size / (1024 * 1024)).toFixed(1)} MB`;
  return `${(size / (1024 * 1024 * 1024)).toFixed(1)} GB`;
}
function displayTime(value) {
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? '时间未知' : date.toLocaleString();
}
function settingsFor(workspaceId) {
  if (!syncSettings.has(workspaceId)) syncSettings.set(workspaceId, { mode: 'bidirectional', scope: '', quiesced: false, handoffTarget: 'cloud_workspace' });
  return syncSettings.get(workspaceId);
}
function liveSettingsFor(workspaceId) {
  if (!liveSyncSettings.has(workspaceId)) liveSyncSettings.set(workspaceId, { scope: '', rescanSeconds: 30, confirmRecovery: false });
  return liveSyncSettings.get(workspaceId);
}
function parseScope(value) { return value.split(/[\n,]/).map((part) => part.trim()).filter(Boolean); }
function workspaceSyncEnabled(workspaceId) { return Boolean(state.workspaceSync?.bindings?.[workspaceId]?.enabled); }
function liveSyncFor(workspaceId) { return state?.liveSyncs?.[workspaceId] || null; }
function liveSyncRunning(value) { return ['starting', 'running', 'watching', 'scanning', 'transferring', 'reconnecting', 'syncing', 'stopping', 'active'].includes(value?.state); }
function hasLiveSyncCapability() {
  return typeof api.startLiveSync === 'function' && typeof api.stopLiveSync === 'function' && typeof api.onLiveSync === 'function';
}
function operationActive() { return state?.syncState === 'running' || syncOperation?.status === 'running'; }
function recoveryPending() { return Boolean(state?.syncCheckpoint); }
function syncProgressText(progress) {
  if (!progress) return '';
  const confirmed = Number(progress.confirmed_bytes) || 0;
  const planned = Number(progress.planned_bytes) || 0;
  const stage = progress.stage || progress.state || '同步中';
  const stageLabels = { inventory: '正在统计文件', hash: '正在校验文件', planning: '正在准备传输', transferring: '正在传输文件' };
  return `${stageLabels[stage] || stage}${planned ? ` · ${formatBytes(confirmed)} / ${formatBytes(planned)}` : ''}`;
}
function liveProgressText(progress) {
  if (typeof progress === 'string') return progress;
  if (!progress || typeof progress !== 'object') return '';
  const stage = progress.stage || progress.phase || progress.state;
  const completed = Number(progress.completed_bytes ?? progress.confirmed_bytes ?? progress.transferred_bytes) || 0;
  const total = Number(progress.total_bytes ?? progress.planned_bytes) || 0;
  if (!stage && !total && !completed) return '';
  return `${stage || '本机实时同步'}${total ? ` · ${formatBytes(completed)} / ${formatBytes(total)}` : ''}`;
}
function normalizedRelativePath(path) {
  if (typeof path !== 'string' || !path.trim()) return null;
  const relative = path.trim().replace(/\\/g, '/');
  if (relative.includes('\0') || relative.startsWith('/') || relative.startsWith('~/')
    || /^[a-z][a-z0-9+.-]*:/i.test(relative) || relative.split('/').includes('..')) return null;
  return relative;
}
function relativeConflictPath(conflict) {
  const path = typeof conflict === 'string' ? conflict : conflict?.path ?? conflict?.relativePath ?? conflict?.relative_path;
  return normalizedRelativePath(path) || '未知相对路径';
}
function liveSyncConflictCount(value) { return Array.isArray(value?.conflicts) ? value.conflicts.length : 0; }
function liveSyncBlockedReasons(workspace, mapping, value) {
  const reasons = [];
  const closedMessage = typeof value?.message === 'string' && /未开放|不可用|\bclosed\b|\bdisabled\b|not[_ -]?supported/i.test(value.message);
  const capabilityClosed = !hasLiveSyncCapability() || !value || value.capability !== 'enabled' || ['unavailable', 'disabled', 'closed', 'not_supported'].includes(value.state) || closedMessage;
  if (capabilityClosed) reasons.push('此版本未开放旧版实时同步能力；相关操作失败关闭。');
  if (workspace.state !== 'ready') reasons.push('Workspace 尚未就绪。');
  if (!mapping) reasons.push('尚未选择本机 Workspace 目录。');
  const binding = state.workspaceSync?.bindings?.[workspace.id];
  if (binding && binding.state !== 'revoked') reasons.push('此 Workspace 存在 Syncthing 绑定。');
  if (state.syncCheckpoint) reasons.push('存在待处理的单次同步恢复点。');
  if (state.syncRecovery?.state === 'unknown' || syncOperation?.status === 'unknown') reasons.push('当前同步恢复状态未确认。');
  if (syncOperation?.status === 'conflict' || state.syncRecovery?.state === 'conflict'
    || (Array.isArray(state.syncRecovery?.conflicts) && state.syncRecovery.conflicts.length > 0)
    || liveSyncConflictCount(value) > 0 || value?.state === 'conflict') reasons.push('存在尚未处理的同步冲突。');
  if (operationActive()) reasons.push('当前有单次同步操作正在运行。');
  if (liveSyncPending.has(workspace.id)) reasons.push('本机实时同步操作正在处理。');
  return reasons;
}
function renderLiveSync(workspace, mapping, parent) {
  const value = liveSyncFor(workspace.id);
  const settings = liveSettingsFor(workspace.id);
  if (value && !settings.initializedFromRecord) {
    if (Array.isArray(value.scope)) settings.scope = value.scope.join('\n');
    if (Number.isInteger(value.rescanSeconds)) settings.rescanSeconds = value.rescanSeconds;
    settings.initializedFromRecord = true;
  }
  const running = liveSyncRunning(value);
  const recoveryRequired = value?.state === 'persisted_uncertain' || value?.recoveryRequired === true;
  const supportedStates = ['idle', 'stopped', 'persisted_uncertain'];
  const startable = supportedStates.includes(value?.state);
  const blockedReasons = liveSyncBlockedReasons(workspace, mapping, value);
  const section = text('section', '', 'card sync-card');
  section.append(text('strong', '旧版实时同步（本机）'));

  const stateLabels = {
    idle: '本机报告已停止',
    stopped: '本机报告已停止',
    starting: '本机正在启动',
    running: '本机报告正在运行',
    scanning: '本机正在扫描',
    syncing: '本机正在同步',
    stopping: '本机正在停止',
    active: '本机报告正在运行',
    persisted_uncertain: '上次运行状态未确定，需要人工确认后才能恢复',
    conflict: '本机报告存在同步冲突',
    failed: '本机报告运行失败',
    error: '本机报告运行异常',
    unknown: '本机实时同步状态未知',
    unavailable: '此版本未开放旧版实时同步能力',
    disabled: '旧版实时同步能力已关闭',
    closed: '旧版实时同步能力已关闭',
    not_supported: '此版本未开放旧版实时同步能力',
  };
  const statusLabel = value?.state ? (stateLabels[value.state] || `本机状态：${value.state}`) : '本机状态未知';
  section.append(text('p', statusLabel, value?.state === 'conflict' || value?.state === 'failed' || value?.state === 'error' || value?.state === 'unknown' || value?.state === 'persisted_uncertain' ? 'error' : 'muted'));
  if (value?.message) section.append(text('p', String(value.message), value.state === 'conflict' || value.state === 'failed' || value.state === 'error' ? 'error' : 'muted'));
  if (value?.sessionId) section.append(text('small', `本机 session：${value.sessionId}`));
  if (Number.isSafeInteger(value?.lastConfirmedRevision)) section.append(text('small', `最近核验 revision：${value.lastConfirmedRevision}`));
  if (value?.progress) {
    const progressPanel = text('div', '', 'sync-status');
    const progress = value.progress;
    const total = Number(progress?.total_bytes ?? progress?.planned_bytes) || 0;
    const completed = Number(progress?.completed_bytes ?? progress?.confirmed_bytes ?? progress?.transferred_bytes) || 0;
    if (total > 0) {
      const bar = document.createElement('progress');
      bar.max = total;
      bar.value = Math.min(completed, total);
      progressPanel.append(bar);
    }
    const progressText = liveProgressText(progress);
    if (progressText) progressPanel.append(text('p', progressText, 'muted'));
    section.append(progressPanel);
  }
  const conflicts = Array.isArray(value?.conflicts) ? value.conflicts : [];
  if (conflicts.length) {
    const list = text('ul', '');
    for (const conflict of conflicts.slice(0, 8)) list.append(text('li', relativeConflictPath(conflict)));
    section.append(text('strong', `发现冲突 ${conflicts.length} 项（仅显示 Workspace 相对路径）`), list);
    if (conflicts.length > 8) section.append(text('small', `另有 ${conflicts.length - 8} 项`));
  }

  const scopeLabel = text('label', '限定相对路径（可选，每行一项）');
  const scope = document.createElement('textarea');
  scope.value = settings.scope;
  scope.rows = 2;
  scope.placeholder = '例如：src/notes\nREADME.md';
  scope.disabled = blockedReasons.length > 0 || running || Array.isArray(value?.scope);
  scope.oninput = () => { settings.scope = scope.value; };
  scopeLabel.append(scope);
  section.append(scopeLabel);
  const rescanLabel = text('label', '本机扫描间隔（秒）');
  const rescan = document.createElement('input');
  rescan.type = 'number';
  rescan.min = '5';
  rescan.max = '3600';
  rescan.step = '1';
  rescan.value = String(settings.rescanSeconds);
  rescan.disabled = blockedReasons.length > 0 || running;
  let startButton = null;
  const updateStartDisabled = () => {
    if (!startButton) return;
    const validInterval = Number.isInteger(settings.rescanSeconds) && settings.rescanSeconds >= 5 && settings.rescanSeconds <= 3600;
    startButton.disabled = !startable || blockedReasons.length > 0 || !validInterval || (recoveryRequired && settings.confirmRecovery !== true);
  };
  rescan.oninput = () => { settings.rescanSeconds = Number(rescan.value); updateStartDisabled(); };
  rescanLabel.append(rescan);
  section.append(rescanLabel);
  if (recoveryRequired) {
    const confirmLabel = text('label', '');
    const confirm = document.createElement('input');
    confirm.type = 'checkbox';
    confirm.checked = settings.confirmRecovery;
    confirm.disabled = blockedReasons.length > 0 || running;
    confirm.onchange = () => { settings.confirmRecovery = confirm.checked; updateStartDisabled(); };
    confirmLabel.append(confirm, text('span', '我确认上次本机运行状态未确定，并同意人工检查后尝试恢复'));
    section.append(confirmLabel);
  }

  if (running) {
    const stop = button('停止旧版实时同步', () => stopLiveSync(workspace.id));
    stop.disabled = !hasLiveSyncCapability() || liveSyncPending.has(workspace.id);
    section.append(stop);
  } else {
    const canStart = startable && blockedReasons.length === 0 && (!recoveryRequired || settings.confirmRecovery === true);
    startButton = button(recoveryRequired ? '确认并恢复旧版实时同步' : '启动旧版实时同步', () => startLiveSync(workspace.id, workspace, mapping));
    const validInterval = Number.isInteger(settings.rescanSeconds) && settings.rescanSeconds >= 5 && settings.rescanSeconds <= 3600;
    startButton.disabled = !canStart || !validInterval;
    section.append(startButton);
  }
  if (!running && !startable && value && !blockedReasons.some((reason) => reason.startsWith('此版本未开放'))) {
    section.append(text('p', '当前本机状态不支持启动；请先检查状态并完成必要恢复。', 'error'));
  }
  if (blockedReasons.length) section.append(text('p', blockedReasons.join(' '), 'error'));
  section.append(text('p', '状态和进度来自本机；即使显示正在运行，也不代表服务端已确认文件传输完成。启动与停止仅在你明确点击后执行，不会自动重放。', 'muted'));
  parent.append(section);
}
async function refreshLiveSyncState() {
  try { renderState(await api.state()); }
  catch { if (state?.view === 'sync') void renderWorkspaces(); }
}
async function startLiveSync(workspaceId, workspace, mapping) {
  const value = liveSyncFor(workspaceId);
  const settings = liveSettingsFor(workspaceId);
  if (!workspace || !value || !['idle', 'stopped', 'persisted_uncertain'].includes(value.state)) {
    message('本机实时同步能力或状态未确认；操作失败关闭。', true);
    return;
  }
  const reasons = liveSyncBlockedReasons(workspace, mapping, value);
  if (reasons.length) { message(reasons.join(' '), true); return; }
  const recoveryRequired = value.state === 'persisted_uncertain' || value.recoveryRequired === true;
  if (recoveryRequired && settings.confirmRecovery !== true) {
    message('请先勾选人工恢复确认，再尝试恢复本机实时同步。', true);
    return;
  }
  if (!Number.isInteger(settings.rescanSeconds) || settings.rescanSeconds < 5 || settings.rescanSeconds > 3600) {
    message('扫描间隔需为 5 到 3600 秒的整数。', true);
    return;
  }
  const scope = parseScope(settings.scope);
  if (scope.some((path) => !normalizedRelativePath(path))) {
    message('同步范围只能使用 Workspace 内的相对路径。', true);
    return;
  }
  if (!hasLiveSyncCapability()) { message('此版本未开放旧版实时同步能力；操作失败关闭。', true); return; }
  liveSyncPending.add(workspaceId);
  const confirmedRecovery = recoveryRequired && settings.confirmRecovery === true;
  settings.confirmRecovery = false;
  void renderWorkspaces();
  let failure = null;
  try { await api.startLiveSync(workspaceId, scope, settings.rescanSeconds, confirmedRecovery); }
  catch (error) { failure = error; }
  finally {
    liveSyncPending.delete(workspaceId);
    await refreshLiveSyncState();
  }
  if (failure) throw failure;
  message('启动操作已返回；请查看本机状态，服务端传输结果尚未确认。');
}
async function stopLiveSync(workspaceId) {
  if (!hasLiveSyncCapability()) { message('此版本未开放旧版实时同步能力；操作失败关闭。', true); return; }
  if (liveSyncPending.has(workspaceId) || !liveSyncRunning(liveSyncFor(workspaceId))) return;
  liveSyncPending.add(workspaceId);
  void renderWorkspaces();
  let failure = null;
  try { await api.stopLiveSync(workspaceId); }
  catch (error) { failure = error; }
  finally {
    liveSyncPending.delete(workspaceId);
    await refreshLiveSyncState();
  }
  if (failure) throw failure;
  message('停止操作已返回；请查看本机状态确认停止，服务端传输结果尚未确认。');
}
function renderSyncStatus(panel) {
  if (!panel) return;
  const operation = syncOperation;
  if (!operation || !panel.workspaceId || operation.workspaceId !== panel.workspaceId) {
    panel.replaceChildren();
    return;
  }
  const title = operation.status === 'running' ? '正在执行 Workspace 同步…'
    : operation.status === 'completed' ? operation.message
      : operation.status === 'conflict' ? '发现冲突，结果未完成'
        : operation.status === 'cancelled' ? '同步已停止，恢复点已保留'
          : operation.status === 'unknown' ? '同步结果未知，恢复点已保留'
            : operation.message || '同步尚未确认完成';
  const body = text('div', '', `sync-status ${operation.status === 'completed' ? 'success' : operation.status === 'running' ? 'muted' : 'error'}`);
  body.append(text('strong', title));
  const progress = operation.progress;
  if (progress) {
    const planned = Number(progress.planned_bytes) || 0;
    const confirmed = Number(progress.confirmed_bytes) || 0;
    if (planned > 0) {
      const bar = document.createElement('progress');
      bar.max = planned;
      bar.value = Math.min(confirmed, planned);
      body.append(bar);
    }
    body.append(text('p', syncProgressText(progress), 'muted'));
  }
  const conflicts = Array.isArray(operation.conflicts) ? operation.conflicts : [];
  if (conflicts.length) {
    const list = text('ul', '');
    for (const conflict of conflicts.slice(0, 8)) list.append(text('li', typeof conflict === 'string' ? conflict : String(conflict?.path || '未知路径')));
    body.append(text('strong', `冲突 ${conflicts.length} 项；请检查两侧文件后再继续`), list);
    if (conflicts.length > 8) body.append(text('small', `另有 ${conflicts.length - 8} 项`));
  }
  panel.replaceChildren(body);
}
function renderWorkspaceSync(workspace, mapping, parent, running, checkpoint) {
  const workspaceId = workspace.id;
  const settings = settingsFor(workspaceId);
  if (checkpoint?.workspaceId === workspaceId) {
    if (Array.isArray(checkpoint.scope)) settings.scope = checkpoint.scope.join('\n');
    if (checkpoint.mode === 'handoff_checkpoint') settings.handoffTarget = checkpoint.handoffTarget || 'cloud_workspace';
  }
  const bindingEnabled = workspaceSyncEnabled(workspaceId);
  const nodeUnknown = state.workspaceSync?.nodeState === 'unknown';
  const live = liveSyncFor(workspaceId);
  const liveBlocked = liveSyncRunning(live) || live?.state === 'persisted_uncertain' || liveSyncConflictCount(live) > 0 || live?.state === 'conflict';
  const blocked = running || Boolean(checkpoint) || nodeUnknown || bindingEnabled || liveBlocked || workspace.state !== 'ready' || !mapping;
  const section = text('section', '', 'card sync-card');
  section.append(text('strong', '单次同步与交接检查点'));

  const modeLabel = text('label', '同步方向');
  const mode = document.createElement('select');
  let confirmed = null, handoff = null, resumeHandoff = null;
  const clearQuiesced = () => {
    settings.quiesced = false;
    if (confirmed) confirmed.checked = false;
    if (handoff) handoff.disabled = running || recoveryPending() || nodeUnknown || bindingEnabled || !mapping || workspace.state !== 'ready' || !settings.quiesced;
    if (resumeHandoff) resumeHandoff.disabled = running || nodeUnknown || bindingEnabled || !settings.quiesced || !mapping;
  };
  for (const [value, label] of Object.entries(syncModes).filter(([value]) => value !== 'live_sync' || checkpoint?.workspaceId === workspaceId && checkpoint.mode === value)) {
    const option = text('option', label);
    option.value = value;
    mode.append(option);
  }
  mode.value = checkpoint?.workspaceId === workspaceId && Object.hasOwn(syncModes, checkpoint.mode) ? checkpoint.mode : settings.mode;
  mode.disabled = blocked;
  mode.onchange = () => { settings.mode = mode.value; clearQuiesced(); };
  modeLabel.append(mode);
  section.append(modeLabel);

  const scopeLabel = text('label', '指定相对路径（可选，每行一项）');
  const scope = document.createElement('textarea');
  scope.value = settings.scope;
  scope.rows = 2;
  scope.placeholder = '例如：src/notes\nREADME.md';
  scope.disabled = blocked;
  scope.oninput = () => { settings.scope = scope.value; clearQuiesced(); };
  scopeLabel.append(scope);
  section.append(scopeLabel, text('p', '留空表示整个 Workspace；下载和双向同步只补齐缺失文件，不传播删除。', 'muted'));

  const start = button('开始同步', () => runWorkspaceSync(workspaceId, settings.mode, parseScope(settings.scope), false));
  start.disabled = blocked;
  section.append(start);

  if (checkpoint?.workspaceId === workspaceId) {
    if (Object.hasOwn(syncModes, checkpoint.mode)) {
      const resume = button(`按原模式“${syncModes[checkpoint.mode]}”从断点恢复`, () => runWorkspaceSync(workspaceId, checkpoint.mode, parseScope(settings.scope), false));
      resume.disabled = running || nodeUnknown || bindingEnabled || !mapping;
      section.append(resume);
    } else if (checkpoint.mode === 'handoff_checkpoint') {
      resumeHandoff = button('按原交接检查点恢复', () => runWorkspaceSync(workspaceId, checkpoint.mode, parseScope(settings.scope), true));
      resumeHandoff.disabled = running || nodeUnknown || bindingEnabled || !settings.quiesced || !mapping;
      section.append(resumeHandoff);
    }
  }

  const handoffResumeAllowed = checkpoint?.workspaceId === workspaceId && checkpoint.mode === 'handoff_checkpoint';
  const handoffLabel = text('label', '');
  confirmed = document.createElement('input');
  confirmed.type = 'checkbox';
  confirmed.checked = settings.quiesced;
  confirmed.disabled = running || nodeUnknown || bindingEnabled || !mapping || workspace.state !== 'ready' || Boolean(checkpoint && !handoffResumeAllowed);
  confirmed.onchange = () => {
    settings.quiesced = confirmed.checked;
    if (handoff) handoff.disabled = running || recoveryPending() || nodeUnknown || bindingEnabled || !mapping || workspace.state !== 'ready' || !settings.quiesced;
    if (resumeHandoff) resumeHandoff.disabled = running || nodeUnknown || bindingEnabled || !settings.quiesced || !mapping;
  };
  handoffLabel.append(confirmed, text('span', '我已人工暂停此 Workspace 的 AI、IDE 和构建写入，并确认其实时同步已暂停'));
  section.append(handoffLabel);
  const handoffTargetLabel = text('label', '交接后的继续位置（本机记录）');
  const handoffTarget = document.createElement('select');
  for (const [value, label] of [['cloud_workspace', '云端 Workspace'], ['desktop', '此电脑']]) {
    const option = text('option', label);
    option.value = value;
    handoffTarget.append(option);
  }
  handoffTarget.value = settings.handoffTarget;
  handoffTarget.disabled = blocked;
  handoffTarget.onchange = () => { settings.handoffTarget = handoffTarget.value; };
  handoffTargetLabel.append(handoffTarget);
  section.append(handoffTargetLabel);
  if (!handoffResumeAllowed) {
    handoff = button('双向同步并核验交接检查点', () => runWorkspaceSync(workspaceId, 'handoff_checkpoint', parseScope(settings.scope), true));
    handoff.disabled = running || recoveryPending() || nodeUnknown || bindingEnabled || !mapping || workspace.state !== 'ready' || !settings.quiesced;
    section.append(handoff);
  }
  section.append(text('p', '勾选只记录人工确认，应用无法检测写入进程。检查点结果未知或有冲突时不会显示为成功，恢复点会保留。', 'muted'));
  const handoffRecord = state.handoff?.workspaceId === workspaceId ? state.handoff : null;
  if (handoffRecord) {
    const phases = { checking: '核验中', needs_review: '需要检查', verified: '已核验' };
    const targets = { desktop: '此电脑', cloud_workspace: '云端 Workspace' };
    section.append(text('p', `本机交接记录：${targets[handoffRecord.source]} → ${targets[handoffRecord.target]} · ${phases[handoffRecord.phase] || handoffRecord.phase}${Number.isSafeInteger(handoffRecord.revision) ? ` · 版本 ${handoffRecord.revision}` : ''}。目标仅作记录，不会启动远端工作进程或绑定 Host。`, 'muted'));
  }
  if (state.syncRecovery?.workspaceId === workspaceId && state.syncRecovery.state === 'unknown') {
    section.append(text('p', state.syncRecovery.message || '同步恢复状态未知；请先核对本机状态。', 'error'));
  }

  if (syncOperation?.workspaceId === workspaceId && syncOperation.status === 'running') {
    const cancel = button('取消并保留恢复点', stopWorkspaceSync);
    section.append(cancel);
  }
  const panel = text('div', '', 'sync-operation-panel');
  panel.workspaceId = workspaceId;
  section.append(panel);
  if (syncOperation?.workspaceId === workspaceId) syncStatusPanel = panel;
  renderSyncStatus(panel);
  if (bindingEnabled) section.append(text('p', '请先暂停此 Workspace 的实时同步，再运行单次同步或交接核验。', 'error'));
  if (nodeUnknown) section.append(text('p', '固定同步节点状态未知；同步操作已禁用。', 'error'));
  parent.append(section);
}
async function runWorkspaceSync(workspaceId, mode, scope, confirmedQuiesced) {
  const checkpoint = state?.syncCheckpoint;
  const live = liveSyncFor(workspaceId);
  const sameCheckpoint = checkpoint?.workspaceId === workspaceId && checkpoint?.mode === mode;
  if (operationActive() || (checkpoint && !sameCheckpoint) || workspaceSyncEnabled(workspaceId) || state?.workspaceSync?.nodeState === 'unknown'
    || liveSyncRunning(live) || live?.state === 'persisted_uncertain' || live?.state === 'conflict' || liveSyncConflictCount(live) > 0) {
    message('当前同步状态不允许启动此操作；请先暂停实时同步或处理原恢复点。', true);
    return;
  }
  const settings = settingsFor(workspaceId);
  const handoffTarget = mode === 'handoff_checkpoint' && sameCheckpoint
    ? checkpoint.handoffTarget || 'cloud_workspace'
    : settings.handoffTarget;
  if (mode === 'handoff_checkpoint' && (confirmedQuiesced !== true || !settings.quiesced || workspaceSyncEnabled(workspaceId))) {
    message('交接核验需要勾选人工停写确认，并先暂停此 Workspace 的实时同步。', true);
    return;
  }
  syncOperation = { workspaceId, mode, status: 'running', progress: null, conflicts: [], message: '' };
  if (mode === 'handoff_checkpoint') settings.quiesced = false;
  renderSyncStatus(syncStatusPanel);
  void renderWorkspaces();
  try {
    const result = mode === 'handoff_checkpoint'
      ? await api.sync(workspaceId, mode, scope, confirmedQuiesced, handoffTarget)
      : await api.sync(workspaceId, mode, scope, confirmedQuiesced);
    if (mode === 'handoff_checkpoint') {
      if (Number.isSafeInteger(result?.revision) && result.revision >= 0) {
        syncOperation = { ...syncOperation, status: 'completed', message: `交接检查点已核验（版本 ${result.revision}）` };
      } else {
        syncOperation = { ...syncOperation, status: 'unknown', message: '交接核验结果未获确认；检查点恢复点已保留。' };
      }
    } else {
      const session = result?.session;
      const valid = session && typeof session.id === 'string' && /^p[a-z0-9]{7}$/.test(session.id) && session.workspace_id === workspaceId && Array.isArray(result?.conflicts);
      if (!valid) syncOperation = { ...syncOperation, status: 'unknown', message: '同步结果未获确认；检查点恢复点已保留。' };
      else if (session.state === 'completed' && result.conflicts.length === 0) syncOperation = { ...syncOperation, status: 'completed', message: `${syncModes[mode]}已完成` };
      else syncOperation = { ...syncOperation, status: result.conflicts.length ? 'conflict' : 'unknown', conflicts: result.conflicts, message: `同步状态：${session.state}；检查点恢复点已保留。` };
    }
  } catch (error) {
    syncOperation = { ...syncOperation, status: 'unknown', message: `无法确认同步结果：${errorMessage(error)}；检查点恢复点已保留。` };
  } finally {
    settings.quiesced = false;
    try { renderState(await api.state()); }
    catch { renderSyncStatus(syncStatusPanel); }
  }
  const success = syncOperation.status === 'completed';
  message(syncOperation.message, !success);
  renderSyncStatus(syncStatusPanel);
}
async function stopWorkspaceSync() {
  const stopped = await api.stopSync();
  if (syncOperation) syncOperation = { ...syncOperation, status: stopped ? 'cancelled' : 'unknown', message: stopped ? '同步已停止；检查点恢复点已保留，可按原操作恢复。' : '同步停止未获确认；检查点恢复点已保留。' };
  renderState(await api.state());
  message(syncOperation?.message || (stopped ? '同步已停止；恢复点已保留。' : '同步停止未确认，恢复点已保留'), !stopped);
}
async function discardSyncCheckpoint() {
  if (!window.confirm('明确放弃此 Workspace 同步恢复点？放弃后不能从该检查点继续。')) return;
  await api.discardSync();
  const updated = await api.state();
  renderState(updated);
  message(updated.syncCheckpoint ? '恢复点状态仍未确认，请检查本机同步状态。' : '已明确放弃同步恢复点。', Boolean(updated.syncCheckpoint));
}
function renderPreservedSection(mapping, parent, expectedState) {
  const panel = text('section', '', 'card');
  panel.append(text('strong', '本机安全副本'), text('p', '正在读取本机安全副本…', 'muted'));
  parent.append(panel);
  void refreshPreservedSection(mapping, panel, expectedState);
}
async function refreshPreservedSection(mapping, panel, expectedState) {
  const heading = text('strong', '本机安全副本');
  if (expectedState.syncState !== 'idle' || expectedState.syncCheckpoint || (expectedState.workspaceSync?.nodeState ?? 'stopped') !== 'stopped') {
    panel.replaceChildren(heading, text('p', '同步节点正在运行、仍可能持有目录或存在待处理恢复点；请先暂停并确认停止后再查看、恢复或删除本机副本。', 'muted'));
    return;
  }
  panel.replaceChildren(heading, text('p', '正在读取本机安全副本…', 'muted'));
  let files;
  try { files = await api.preserved(mapping.workspaceId); }
  catch {
    if (state !== expectedState) return;
    panel.replaceChildren(heading, text('p', '无法安全读取本机副本。请先解决同步状态或检查本机目录。', 'error'));
    return;
  }
  if (state !== expectedState || !state.authenticated) return;
  const copies = Array.isArray(files) ? files : [];
  const total = copies.reduce((sum, file) => sum + (Number.isSafeInteger(file?.size) && file.size >= 0 ? file.size : 0), 0);
  panel.replaceChildren(heading, text('p', `共 ${copies.length} 份 · 额外占用 ${formatBytes(total)}`, 'muted'));
  if (!copies.length) {
    panel.append(text('p', '此 Workspace 暂无本机安全副本。', 'muted'));
    return;
  }
  for (const file of copies) {
    const row = text('div', '', 'card');
    row.append(text('strong', file.path));
    row.append(text('p', `${displayTime(file.created_at)} · ${formatBytes(file.size)}`, 'muted'));
    row.append(button('恢复到本机', async () => {
      const prompt = `将“${file.path}”恢复到 Workspace。目标路径必须为空；恢复后归档副本仍会保留。确认继续？`;
      if (!window.confirm(prompt)) return;
      await api.restorePreserved(mapping.workspaceId, file.id);
      message(`已恢复本机文件：${file.path}`);
      await refreshPreservedSection(mapping, panel, expectedState);
    }));
    row.append(button('删除此副本', async () => {
      const prompt = `确定删除本机安全副本“${file.path}”（${formatBytes(file.size)}）？此操作仅删除这份本机副本，无法恢复，不影响云端 Workspace。`;
      if (!window.confirm(prompt)) return;
      await api.deletePreserved(mapping.workspaceId, file.id);
      message(`已删除本机安全副本：${file.path}`);
      await refreshPreservedSection(mapping, panel, expectedState);
    }));
    panel.append(row);
  }
}
function renderOfflinePreserved(root, current) {
  root.replaceChildren(text('p', '云端 Workspace 当前不可读取。下面的安全副本来自本机已保存的 Workspace mapping；查看和恢复只使用本机数据。', 'error'));
  if (current.syncState === 'running') root.append(text('p', '本机同步正在运行。', 'muted'), stopSyncButton());
  const localMappings = Array.isArray(current.mappings) ? current.mappings : [];
  if (!localMappings.length) root.append(text('p', '此账号尚未保存本机 Workspace 目录。'));
  for (const mapping of localMappings) {
    const card = text('div', '', 'card');
    card.append(text('strong', `Workspace ${mapping.workspaceId}`));
    renderPreservedSection(mapping, card, current);
    root.append(card);
  }
}
function stopSyncButton() {
  const stop = button('停止同步', stopWorkspaceSync);
  stop.disabled = state.syncState !== 'running';
  return stop;
}

function renderState(value) {
  state = value;
  $('login').hidden = value.authenticated;
  $('navigation').hidden = !value.authenticated;
  $('sync-page').hidden = !value.authenticated || value.view !== 'sync';
  $('account').textContent = value.authenticated ? `${value.accountName || ''} · ${value.origin}` : '';
  $('expiry-warning').hidden = !value.expiresSoon;
  if (!value.authenticated) {
    if (value.origin && !$('origin').value) $('origin').value = value.origin;
    if (!value.storageReady) message('系统安全存储不可用，请解锁 Keychain、GNOME Keyring 或 KDE Wallet。', true);
  } else if (value.view === 'sync') void renderWorkspaces();
}

async function renderWorkspaces() {
  const root = $('workspaces');
  const current = state;
  if (state.profileError) { root.replaceChildren(text('p', state.profileError, 'error')); return; }
  const nodeState = state.workspaceSync?.nodeState || 'stopped';
  const status = text('p', '正在读取 Workspace…', 'muted');
  root.replaceChildren(status);
  if (state.syncState === 'running') root.append(text('p', '本机同步正在运行。', 'muted'), stopSyncButton());
  let data;
  try { message(''); data = await api.workspaces(); }
  catch (error) {
    if (state !== current) return;
    message(errorMessage(error), true);
    renderOfflinePreserved(root, current);
    return;
  }
  if (state !== current || !state.authenticated || state.view !== 'sync') return;
  if (data === null) { renderOfflinePreserved(root, current); return; }
  root.replaceChildren();
  if (state.workspaceSync?.error) root.append(text('p', state.workspaceSync.error, 'error'));
  if (nodeState === 'running') root.append(text('p', '本机固定同步节点正在运行。此状态不表示云端已确认同步完成。', 'muted'));
  else if (nodeState === 'unknown') root.append(text('p', '本机同步节点停止状态未确认；相关目录仍受保护。', 'error'));
  const running = state.syncState === 'running';
  const recoveryPending = Boolean(state.syncCheckpoint);
  if (running) root.append(text('p', '旧版同步进程仍在运行。', 'muted'), stopSyncButton());
  else if (state.syncCheckpoint) {
    const warning = text('div', '', 'card');
    const recoveryMessage = state.syncRecovery?.state === 'unknown'
      ? `${state.syncRecovery.message || '同步恢复状态未知。'} 请检查本机状态后再处理恢复点。`
      : `上次 ${state.syncCheckpoint.workspaceId} 的 ${state.syncCheckpoint.mode} 尚待恢复。只能在原 Workspace、模式和目录身份下继续；也可以明确放弃恢复点。`;
    warning.append(text('p', recoveryMessage, 'error'));
    warning.append(button('明确放弃恢复点', discardSyncCheckpoint));
    root.append(warning);
  } else if (state.syncRecovery && state.syncRecovery.state !== 'pending') {
    root.append(text('p', state.syncRecovery.message || '本机同步恢复状态未知；同步未启动或已暂停。', 'error'));
  }
  const workspaces = items(data);
  for (const workspace of workspaces) {
    const card = text('div', '', 'card');
    const mapping = state.mappings.find((item) => item.workspaceId === workspace.id);
    const live = liveSyncFor(workspace.id);
    const liveBusy = liveSyncRunning(live);
    const liveRecoveryUncertain = live?.state === 'persisted_uncertain' || live?.recoveryRequired === true;
    const liveConflict = live?.state === 'conflict' || liveSyncConflictCount(live) > 0;
    card.append(text('strong', `${workspace.name || workspace.id} · ${workspace.state || ''}`));
    card.append(text('p', mapping?.directoryName || '尚未选择本机目录', 'muted'));
    if (workspace.state === 'ready') {
      const choose = button('选择目录', async () => { await api.chooseDirectory(workspace.id); renderState(await api.state()); });
      choose.disabled = running || recoveryPending || nodeState !== 'stopped' || liveBusy || liveRecoveryUncertain || liveConflict || Boolean(state.workspaceSync?.bindings?.[workspace.id]?.enabled);
      card.append(choose);
      if (mapping) {
        const binding = state.workspaceSync?.bindings?.[workspace.id];
        const toggle = button(binding?.enabled ? '暂停本机同步' : '启用本机同步', async () => {
          renderState(await api.setWorkspaceSyncEnabled(workspace.id, !binding?.enabled));
        });
        toggle.disabled = running || recoveryPending || nodeState === 'unknown' || liveBusy || liveRecoveryUncertain || liveConflict || binding?.state === 'revoking' || binding?.state === 'revoked';
        card.append(toggle);
        if (binding?.state === 'pending_node') card.append(text('p', '设备已登记，正在等待云端节点就绪。', 'muted'));
        else if (binding?.state === 'revoking' || binding?.state === 'revoked') card.append(text('p', '此设备绑定已撤销或正在撤销。', 'error'));
        else if (binding?.enabled) card.append(text('p', '本机开关已启用；请以 Workspace 状态确认同步进度。', 'muted'));
        if (binding?.enabled && binding.state === 'active' && nodeState === 'running') {
          const status = text('div', '', 'sync-status');
          const refreshStatus = button('刷新本机同步状态', async () => {
            refreshStatus.disabled = true;
            status.replaceChildren(text('p', '正在读取本机同步快照…', 'muted'));
            try {
              const value = await api.workspaceSyncStatus(workspace.id);
              const states = { idle: '空闲', scanning: '扫描中', syncing: '同步中', error: '错误' };
              status.replaceChildren(
                text('p', `本机状态：${states[value.state] || value.state} · 待同步 ${value.needFiles} 个文件 / ${formatBytes(value.needBytes)} · 上次同步错误 ${value.pullErrors} 项`, 'muted'),
                text('small', `本机快照 ${displayTime(value.observedAt)}；不代表服务端已确认。`),
              );
            } catch (error) {
              status.replaceChildren(text('p', errorMessage(error), 'error'));
            } finally { refreshStatus.disabled = false; }
          });
          card.append(refreshStatus, status);
        }
        renderWorkspaceSync(workspace, mapping, card, running || operationActive() || liveBusy || liveRecoveryUncertain || liveConflict, state.syncCheckpoint);
      }
    } else card.append(text('p', '此 Workspace 当前不可同步。', 'muted'));
    renderLiveSync(workspace, mapping, card);
    if (mapping) renderPreservedSection(mapping, card, current);
    root.append(card);
  }
  if (!workspaces.length) root.append(text('p', '暂无 Workspace。'));
  root.append(text('p', '本机安全副本和固定同步节点仍按各自控制项管理；单次同步只在你明确启动后运行。启用文件同步不会自动授权 Task 读取这些文件。', 'muted'));
}

$('login-form').onsubmit = (event) => {
  event.preventDefault();
  void run(async () => {
    const origin = $('origin').value.trim(), name = $('name').value.trim(), password = $('password').value;
    $('submit').disabled = true;
    try {
      renderState(await api.login(origin, name, password));
      $('password').value = '';
    } finally { $('submit').disabled = false; }
  });
};
$('register').onclick = () => void run(() => api.publicPage('register', $('origin').value.trim()));
$('download').onclick = () => void run(() => api.publicPage('download', $('origin').value.trim()));
$('web').onclick = () => void run(async () => { await api.showWeb(); renderState(await api.state()); });
$('account-settings').onclick = () => void run(async () => { await api.showAccount(); renderState(await api.state()); });
$('local-sync').onclick = () => void run(async () => { await api.showSync(); renderState(await api.state()); });
$('logout').onclick = () => void run(async () => { const warning = await api.logout(); renderState(await api.state()); if (warning) message(warning, true); });
api.onState(renderState);
if (typeof api.onLiveSync === 'function') api.onLiveSync((value) => {
  const workspaceId = value?.workspaceId;
  if (!workspaceId || !state) return;
  const event = value.event && typeof value.event === 'object' ? value.event : {};
  const previous = liveSyncFor(workspaceId) || {};
  const next = { ...previous, ...event };
  if (typeof event.state !== 'string' && liveSyncStates.has(event.status)) next.state = event.status;
  state = { ...state, liveSyncs: { ...(state.liveSyncs || {}), [workspaceId]: next } };
  if (state.view === 'sync') void renderWorkspaces();
});
api.onSync((value) => {
  const workspaceId = value.workspaceId || state?.syncCheckpoint?.workspaceId;
  if (syncOperation && (!workspaceId || workspaceId === syncOperation.workspaceId)) {
    syncOperation = { ...syncOperation, progress: value.progress || syncOperation.progress, eventState: value.state };
    if (value.state === 'shutdown_unknown' || value.state === 'offline') {
      syncOperation = { ...syncOperation, status: 'unknown', message: value.message || '同步停止或连接状态未知；检查点恢复点已保留。' };
    }
  } else if (workspaceId) {
    syncOperation = { workspaceId, mode: value.mode || state?.syncCheckpoint?.mode, status: value.state === 'running' ? 'running' : 'unknown', progress: value.progress || null, conflicts: [], message: value.message || '' };
  }
  message(value.message || `同步：${value.state}${value.progress?.stage ? ` · ${value.progress.stage}` : ''}`, value.state === 'offline' || value.state === 'shutdown_unknown');
  if (value.state === 'running' && state?.view === 'sync' && state.syncState !== 'running') {
    state.syncState = 'running';
    void renderWorkspaces();
  } else renderSyncStatus(syncStatusPanel);
});
void run(async () => renderState(await api.state()));
