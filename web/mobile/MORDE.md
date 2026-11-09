# Morde 手机 Web 适配与验收边界

## 实际交付

- `src/main.tsx` 只装载 `src/morde/App.tsx`；Vite 固定 `/app/` base 和 `web/dist` 产物。`publicDir: false`，不会将上游 Service Worker、manifest、Cloud/Hub 静态资源打包。原 nanoMuse Web 源文件仅作本地参考，不进入 Morde 页面入口。
- `/app/` 所有 API 请求使用相对 `/api/*` 与 `credentials: "same-origin"`，只复用 Portal Cookie；页面不保存另一套 token。401 清空内存数据、草稿、待重试键并导向 Portal 登录。`GET /api/me` 的用户 ID 变化会清理上一个用户的会话状态；异步响应按用户和刷新代次隔离。
- 导航读取真实 Host、共享 Host、Workspace、无密钥可用模型、Conversation、Task 与通知。客户端安装入口链接到 Portal `/clients`，同步设备进度链接到 Portal `/workspaces`；不自行维护同步状态。
- 新任务走 `POST /api/tasks`，续聊走 `POST /api/conversations/{id}/messages`；每次逻辑发送保留 `Idempotency-Key` 供超时重试。审批带 `pending_id/version`，澄清带 `clarification_id/version`；409 后重读当前 Task。暂停、继续、取消从服务端再次读取状态。
- Task SSE 监听服务端已有事件类型，原生 EventSource 断线时按 `Last-Event-ID` 重连；收到 `task.snapshot_required` 时按返回 sequence 重新取快照并用 `after` 恢复，快照取回失败时重试。
- 自有 Host 入口取 `POST /api/hosts/{id}/enter`；共享观看取 `POST /api/hosts/{id}/observation-ticket`。均只在取得一次性 URL 后导航到 Gateway，不向 Gateway 注入 API 凭证。

## 本地证据

- 原版基线 `npm ci && npm run check && npm run build` 通过，21 个测试文件、126 项测试。
- 改造后 `npm run check && npm run build` 通过；`web/dist` 仅包含 `index.html`、CSS、JS；`git diff --check` 通过。
- 本地假数据 API 的浏览器检查已看到 Host、Workspace、模型、任务输入和 Host 列表，拒绝写入时 UI 能显示 403。该检查不等同真实 Portal/Host 联调。

## 仍待真实环境验收

- 使用真实 Portal 同源 `/app/` 验证登录、登出、两账号切换与 `/api`/Admin fallback 边界；后一项由服务端静态分发负责。
- 按 TC-09–12 在真实服务端做三轮续聊、超时同键重试、SSE 裁剪快照、旧审批 409、跨端同一 Task、暂停与取消在途回执。
- 按 TC-13–16 使用真实 Gateway 验证一次性 ticket、Host 身份、观看权限和撤权。手机视口、大字体、软键盘与真机触摸仍需手工验收。
- 服务端尚无可发现的已授权应用列表，客户端不展示“应用托管/网站发布已可用”的入口；现有任务可由服务端支持 `application_id`，界面暂不让用户猜测应用 ID。
