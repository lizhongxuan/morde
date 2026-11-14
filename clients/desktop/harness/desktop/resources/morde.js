const api = window.morde;
const $ = (id) => document.getElementById(id);
const offlineText = 'Morde 云端页面暂时无法连接。网络恢复后，可从 PC 端「云端电脑」下的「文件同步」入口继续操作。';
let state = null;

function showMessage(target, value, error = false) {
  target.textContent = value;
  target.className = error ? 'error' : '';
}
function renderState(value) {
  state = value;
  $('login').hidden = value.authenticated;
  $('offline-page').hidden = !value.authenticated || value.view !== 'offline';
  if (value.origin && !$('origin').value) $('origin').value = value.origin;
  if (!value.authenticated && !value.storageReady) showMessage($('message'), '系统安全存储不可用，请解锁 Keychain、GNOME Keyring 或 KDE Wallet。', true);
  if (value.authenticated && value.view === 'offline' && !$('offline-message').textContent) showMessage($('offline-message'), offlineText, true);
}
function showOffline(error = offlineText) {
  $('login').hidden = true;
  $('offline-page').hidden = false;
  showMessage($('offline-message'), error, true);
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
$('sign-out').onclick = () => void api.logout().then(renderState).catch((error) => showOffline(errorMessage(error)));
api.onState(renderState);
api.onSync((value) => { if (value?.state === 'offline') showOffline(value.message || offlineText); });
void api.state().then(renderState).catch(() => showMessage($('message'), '无法读取 Morde 客户端状态。', true));
