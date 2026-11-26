const api = window.morde;
const $ = (id) => document.getElementById(id);
const offlineMessages = Object.freeze({
  session: '已登录，但云端会话暂时无法续期。请检查网络后重试。',
  proxy: '已登录，但本机对话页面服务暂时不可用。重试连接可重新加载。',
  assistant: '已登录，但对话页面暂时无法加载。重试连接不会重新发送消息。',
  unavailable: 'Morde 云端页面暂时无法连接。请检查网络后重试；本机项目同步状态未改变。',
});
function offlineText(reason) { return offlineMessages[reason] || offlineMessages.unavailable; }
let state = null;

function showMessage(target, value, error = false) {
  target.textContent = value;
  target.className = error ? 'error' : '';
}
function renderState(value) {
  state = value;
  $('login').hidden = value.authenticated;
  $('connecting-page').hidden = !value.authenticated || value.view !== 'connecting';
  $('offline-page').hidden = !value.authenticated || value.view !== 'offline';
  if (value.origin && !$('origin').value) $('origin').value = value.origin;
  if (!value.authenticated && !value.storageReady) showMessage($('message'), '系统安全存储不可用，请解锁 Keychain、GNOME Keyring 或 KDE Wallet。', true);
  if (value.authenticated && value.view === 'offline') showMessage($('offline-message'), offlineText(value.offlineReason), true);
}
function showOffline(reason = 'unavailable') {
  $('login').hidden = true;
  $('offline-page').hidden = false;
  showMessage($('offline-message'), offlineText(reason), true);
}
function errorMessage(error) { return (error?.message || String(error)).replace(/^Error invoking remote method '[^']+': Error: /, ''); }

$('login-form').onsubmit = (event) => {
  event.preventDefault();
  const origin = $('origin').value.trim(), name = $('name').value.trim(), password = $('password').value;
  $('submit').disabled = true;
  showMessage($('message'), '');
  void api.login(origin, name, password).then((next) => {
    renderState(next);
    $('password').value = '';
  }).catch((error) => showMessage($('message'), errorMessage(error), true)).finally(() => { $('submit').disabled = false; });
};
$('register').onclick = () => void api.publicPage('register', $('origin').value.trim()).catch((error) => showMessage($('message'), errorMessage(error), true));
$('download').onclick = () => void api.publicPage('download', $('origin').value.trim()).catch((error) => showMessage($('message'), errorMessage(error), true));
$('retry-assistant').onclick = () => {
  showMessage($('offline-message'), '正在重新连接 Morde…');
  void api.openAssistant().then(() => api.state()).then(renderState).catch(() => showOffline());
};
$('sign-out').onclick = () => void api.logout().then(renderState).catch(() => showOffline());
api.onState(renderState);
api.onSync((value) => { if (value?.state === 'offline') showOffline(value.offlineReason); });
void api.state().then(renderState).catch(() => showMessage($('message'), '无法读取 Morde 客户端状态。', true));
