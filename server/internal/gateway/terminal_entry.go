package gateway

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

const terminalEntryScript = `
const status = document.querySelector('#status');
const connectButton = document.querySelector('#connect');
const replaceButton = document.querySelector('#replace');
const frame = document.querySelector('#terminal');
let busy = false;
function showStatus(message, error = false) {
  status.textContent = message;
  status.dataset.error = String(error);
}
function applySession(value) {
  if (!value || typeof value.state !== 'string') throw new Error('终端响应无效。');
  if (value.state === 'ready') {
    const url = new URL(value.url, location.origin);
    if (url.origin !== location.origin || url.pathname !== '/terminal' || url.searchParams.size !== 1 || !url.searchParams.get('session_id')) throw new Error('终端地址无效，已阻止打开。');
    frame.src = url.pathname + url.search;
    frame.hidden = false;
    replaceButton.hidden = true;
    showStatus('终端页面已打开。连接状态请以终端内提示为准；切换设备前请先断开当前终端窗口。');
    return true;
  }
  frame.hidden = true;
  frame.removeAttribute('src');
  if (value.state === 'creating') {
    showStatus('正在准备终端…');
    return false;
  }
  replaceButton.hidden = !['unknown', 'interrupted'].includes(value.state);
  showStatus(value.state === 'unknown'
    ? '上一个终端的状态暂时无法确认。创建新终端前，请确认旧会话中的命令可以继续运行。'
    : value.state === 'interrupted'
      ? '上一个终端已中断，旧命令的状态需要在主机中核实。'
      : '终端尚未就绪，请重试。', value.state !== 'unknown' && value.state !== 'interrupted');
  return true;
}
async function requestSession(replace) {
  const response = await fetch('/__ch/api/terminal-session', {
    method: replace ? 'POST' : 'PUT',
    credentials: 'same-origin',
    cache: 'no-store',
    headers: replace ? { 'Content-Type': 'application/json' } : undefined,
    body: replace ? JSON.stringify({ confirm_new: true }) : undefined,
  });
  const value = await response.json().catch(() => null);
  if (!response.ok) throw new Error(value?.error?.code === 'invalid_state'
    ? '主机当前由 AI 控制。请主机所有者先切换到“用户控制”，然后重试连接终端。'
    : value?.error?.message || '连接失败（HTTP ' + response.status + '）');
  return value;
}
async function connect(replace = false) {
  if (busy) return;
  busy = true;
  connectButton.disabled = true;
  replaceButton.disabled = true;
  try {
    for (let attempt = 0; attempt < 15; attempt += 1) {
      const done = applySession(await requestSession(replace));
      replace = false;
      if (done) break;
      await new Promise((resolve) => setTimeout(resolve, 1000));
    }
  } catch (error) {
    showStatus(error instanceof Error ? error.message : '连接失败，请稍后重试。', true);
  } finally {
    busy = false;
    connectButton.disabled = false;
    replaceButton.disabled = false;
  }
}
connectButton.addEventListener('click', () => void connect());
replaceButton.addEventListener('click', () => {
  if (window.confirm('旧终端中的命令可能仍在运行。确认后创建新终端？')) void connect(true);
});
void connect();
`

const terminalEntryPage = `<!doctype html>
<html lang="zh-CN">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Morde 终端</title>
  <style>
    :root { color-scheme: light; font: 16px/1.5 system-ui, sans-serif; background: #f3f6fb; color: #1b2839; }
    * { box-sizing: border-box; }
    body { margin: 0; min-height: 100vh; padding: 24px; }
    main { display: flex; flex-direction: column; gap: 16px; max-width: 1200px; min-height: calc(100vh - 48px); margin: 0 auto; }
    header { display: flex; flex-wrap: wrap; align-items: center; justify-content: space-between; gap: 16px; padding: 16px 20px; border: 1px solid #d4deea; border-radius: 12px; background: white; }
    h1 { margin: 0; font-size: 20px; }
    p { margin: 4px 0 0; color: #52657d; }
    .actions { display: flex; gap: 8px; }
    button { min-height: 40px; padding: 8px 14px; border: 1px solid #c5d2e2; border-radius: 8px; background: white; color: #18304f; font: inherit; font-weight: 600; cursor: pointer; }
    button:disabled { cursor: wait; opacity: .6; }
    #replace { border-color: #d78965; color: #8b351a; }
    #status { padding: 10px 14px; border-radius: 8px; background: #e8eff9; }
    #status[data-error="true"] { background: #fff0ed; color: #a32c18; }
    iframe { flex: 1; width: 100%; min-height: 460px; border: 1px solid #d4deea; border-radius: 12px; background: #111827; }
    @media (max-width: 640px) { body { padding: 12px; } main { min-height: calc(100vh - 24px); } header { align-items: flex-start; flex-direction: column; } iframe { min-height: 58vh; } }
  </style>
</head>
<body>
  <main>
    <header>
      <div><h1>云端电脑终端</h1><p>此入口只开放终端会话，不包含桌面、文件或主机管理操作。</p></div>
      <div class="actions"><button id="connect" type="button">重新连接</button><button id="replace" type="button" hidden>确认后创建新终端</button></div>
    </header>
    <p id="status" role="status" aria-live="polite">正在连接…</p>
    <iframe id="terminal" title="云端电脑终端" hidden></iframe>
  </main>
  <script>${terminalEntryScript}</script>
</body>
</html>`

func (g *gateway) terminalEntry(c *gin.Context) {
	if parsedHostFrom(c).PreviewPort != nil {
		writeNotFound(c)
		return
	}
	hash := sha256.Sum256([]byte(terminalEntryScript))
	c.Header("Cache-Control", "no-store")
	c.Header("Pragma", "no-cache")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; script-src 'sha256-"+base64.StdEncoding.EncodeToString(hash[:])+"'; connect-src 'self'; frame-src 'self'; frame-ancestors 'self'; base-uri 'none'; form-action 'none'")
	c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(strings.Replace(terminalEntryPage, "${terminalEntryScript}", terminalEntryScript, 1)))
}
