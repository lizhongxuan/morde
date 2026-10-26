import { http, HttpResponse } from "msw";

const now = () => new Date().toISOString();
const host = { id: "h1234567", name: "本地测试主机", portal_origin: "http://127.0.0.1:5183", state: "running", url: "http://h1234567.localhost", expires_at: new Date(Date.now() + 8 * 60 * 60 * 1000).toISOString() };
const control = {
  host_id: host.id,
  control_state: "ai" as "ai" | "taking_over" | "user" | "returning",
  control_version: 0,
  active_transition: null as null | { request_id: string; action: "take_over" | "release"; phase: string; failure_reason: string; expected_control_version: number },
};
type MockInstallOperation = { id: string; application_id: string; status: "accepted" | "running" | "succeeded" | "failed"; progress_percent: number; error_code: string; created_at: string; updated_at: string };
type MockLaunchOperation = { id: string; application_id: "wechat"; status: "starting" | "running" | "stopping" | "stopped" | "exited" | "failed" | "unknown"; error_code: string; created_at: string; updated_at: string };
const applications = {
  host_id: host.id,
  runtime_generation: 1,
  runtime_instance_id: "runtime-mock-1",
  control_version: 0,
  checked_at: now(),
  items: [
    { id: "codex", installed: true, version: "0.1.0", login_status: "not_logged_in" as const, installable: false, install_operation: null },
    { id: "wechat", installed: false, version: "", login_status: "not_applicable" as const, installable: true, install_operation: null as MockInstallOperation | null },
  ],
};
const applicationInstallRequests = new Map<string, MockInstallOperation>();
let applicationInstallPolls = 0;
let applicationLaunch: MockLaunchOperation | null = null;
let applicationLaunchPolls = 0;
let controlPolls = 0;
let terminalSessionUrl: string | null = null;
let terminalSessionSerial = 0;
const failedControlRequests = new Set<string>();
const files = new Map<string, Blob>([
  ["/home/gem/workspace/README.md", new Blob(["# 欢迎使用\n\n这是本地 mock 工作目录。\n"])],
]);
const directories = new Set(["/", "/home", "/home/gem", "/home/gem/workspace"]);
const json = (body: object, status = 200) => HttpResponse.json(body, { status });
const aio = (data: unknown, success = true, message = "ok") => json({ success, message, data });
const terminalHtml = `<!doctype html><html lang="zh-CN"><meta charset="utf-8"><title>本地合成终端</title><body><label for="command">命令</label><input id="command" autocomplete="off" autofocus><pre id="output" role="log" aria-live="polite"></pre><script>const input=document.querySelector('#command');const output=document.querySelector('#output');input.addEventListener('keydown',event=>{if(event.key!=='Enter')return;const command=input.value.trim();if(!command)return;output.textContent += '$ '+command+'\\n'+(command==='echo local-terminal-ok'?'local-terminal-ok':'本地演示仅支持 echo local-terminal-ok')+'\\n';input.value='';});</script></body></html>`;
const codeServerMockHtml = `<!doctype html><html lang="zh-CN"><meta charset="utf-8"><title>本地 VSCode 模拟</title><body><h1>VSCode 本地模拟</h1></body></html>`;
const noVncMockHtml = (connected: boolean) => `<!doctype html><html class="${connected ? "noVNC_connected" : "noVNC_connecting"}"><head><title>本地 noVNC 状态模拟</title></head><body><label for="noVNC_setting_resize">Scaling Mode</label><select id="noVNC_setting_resize"><option value="off">None</option><option value="scale">Local Scaling</option></select><label><input id="noVNC_setting_view_only" type="checkbox">View only</label><button id="noVNC_keyboard_button" type="button">Keyboard</button><button id="noVNC_toggle_alt_button" type="button">Alt</button><button id="noVNC_send_tab_button" type="button">Tab</button><script>const viewOnly=document.querySelector('#noVNC_setting_view_only');viewOnly.checked=new URL(location.href).searchParams.get('view_only')==='true';document.body.dataset.viewOnly=String(viewOnly.checked);viewOnly.addEventListener('change',event=>document.body.dataset.viewOnly=String(event.target.checked));document.querySelector('#noVNC_setting_resize').addEventListener('change',event=>document.body.dataset.resize=event.target.value);document.querySelector('#noVNC_keyboard_button').addEventListener('click',()=>document.body.dataset.keyboardOpens=String(Number(document.body.dataset.keyboardOpens||0)+1));document.querySelector('#noVNC_toggle_alt_button').addEventListener('click',event=>event.currentTarget.classList.toggle('noVNC_selected'));document.querySelector('#noVNC_send_tab_button').addEventListener('click',()=>document.body.dataset.tabPresses=String(Number(document.body.dataset.tabPresses||0)+1))</script></body></html>`;

function resetControl() {
  control.control_state = "ai";
  control.control_version = 0;
  control.active_transition = null;
  controlPolls = 0;
  failedControlRequests.clear();
}

function resetApplications() {
  applications.items[1]!.installed = false;
  applications.items[1]!.version = "";
  applications.items[1]!.install_operation = null;
  applicationInstallRequests.clear();
  applicationInstallPolls = 0;
  applicationLaunch = null;
  applicationLaunchPolls = 0;
}

function launchResponse() {
  return {
    host_id: applications.host_id,
    runtime_generation: applications.runtime_generation,
    runtime_instance_id: applications.runtime_instance_id,
    control_version: applications.control_version,
    operation: applicationLaunch,
  };
}

function reconcileLaunch() {
  if (applicationLaunch?.status === "starting") {
    applicationLaunchPolls += 1;
    if (applicationLaunchPolls >= 1) applicationLaunch.status = "running";
  } else if (applicationLaunch?.status === "stopping") {
    applicationLaunchPolls += 1;
    if (applicationLaunchPolls >= 2) applicationLaunch.status = "stopped";
  }
  if (applicationLaunch) applicationLaunch.updated_at = now();
}

function controlResponse() {
  if (control.control_state === "taking_over" || control.control_state === "returning") {
    controlPolls += 1;
    if (controlPolls >= 2 && control.active_transition) {
      control.control_state = control.active_transition.action === "take_over" ? "user" : "ai";
      control.control_version += 1;
      control.active_transition.phase = "completed";
    }
  }
  return { ...control, active_transition: control.active_transition && { ...control.active_transition } };
}

function createMockTerminalSession() {
  terminalSessionSerial += 1;
  terminalSessionUrl = `/terminal?session_id=mock-terminal-${terminalSessionSerial}`;
  return { state: "ready", url: terminalSessionUrl };
}

function entries(path: string, showHidden: boolean) {
  const prefix = `${path.replace(/\/$/, "")}/`;
  const names = new Map<string, { name: string; path: string; is_directory: boolean; size: number | null; modified_time: string; extension: string | null }>();
  for (const dir of directories) {
    if (!dir.startsWith(prefix) || dir === path) continue;
    const name = dir.slice(prefix.length).split("/")[0]!;
    if (!showHidden && name.startsWith(".")) continue;
    names.set(name, { name, path: `${prefix}${name}`, is_directory: true, size: null, modified_time: now(), extension: null });
  }
  for (const [file, content] of files) {
    if (!file.startsWith(prefix)) continue;
    const name = file.slice(prefix.length).split("/")[0]!;
    if (!showHidden && name.startsWith(".")) continue;
    if (!file.slice(prefix.length).includes("/")) names.set(name, { name, path: file, is_directory: false, size: content.size, modified_time: now(), extension: name.includes(".") ? name.slice(name.lastIndexOf(".")) : "" });
  }
  return [...names.values()].sort((a, b) => Number(b.is_directory) - Number(a.is_directory) || a.name.localeCompare(b.name));
}

function parentPath(path: string) {
  const trimmed = path.replace(/\/$/, "");
  const separator = trimmed.lastIndexOf("/");
  return separator <= 0 ? "/" : trimmed.slice(0, separator);
}

function exists(path: string) {
  return directories.has(path) || files.has(path);
}

function shellArgs(command: string) {
  const args: string[] = [];
  let token = "";
  let quote: "single" | "double" | undefined;
  let escaped = false;
  for (let index = 0; index < command.length; index += 1) {
    const char = command[index]!;
    if (!quote && char === "&" && command[index + 1] === "&") break;
    if (escaped) { token += char; escaped = false; continue; }
    if (quote === "single") { if (char === "'") quote = undefined; else token += char; continue; }
    if (quote === "double") { if (char === '"') quote = undefined; else if (char === "\\") escaped = true; else token += char; continue; }
    if (char === "\\") { escaped = true; continue; }
    if (char === "'") { quote = "single"; continue; }
    if (char === '"') { quote = "double"; continue; }
    if (/\s/.test(char)) { if (token) args.push(token); token = ""; } else token += char;
  }
  if (token) args.push(token);
  return args;
}

function commandParts(command: string) {
  const [op, ...tokens] = shellArgs(command);
  const separator = tokens.indexOf("--");
  const beforeSeparator = separator < 0 ? tokens : tokens.slice(0, separator);
  const afterSeparator = separator < 0 ? [] : tokens.slice(separator + 1);
  return {
    op,
    options: beforeSeparator.filter((arg) => arg.startsWith("-")),
    operands: [...beforeSeparator.filter((arg) => !arg.startsWith("-")), ...afterSeparator],
  };
}

// shortcut: only file-manager mkdir, mv, and rm are modeled; add commands when the UI uses them.
function runMockCommand(command: string) {
  const { op, options, operands } = commandParts(command);
  if (op === "mkdir") {
    const recursive = options.includes("-p");
    if (operands.length !== 1) return { exit_code: 2, output: "mkdir: 缺少或多余目录名" };
    const target = operands[0]!;
    if (directories.has(target)) return recursive ? { exit_code: 0, output: "" } : { exit_code: 1, output: "mkdir: 目录已存在" };
    if (files.has(target)) return { exit_code: 1, output: "mkdir: 同名文件已存在" };
    const parent = parentPath(target);
    if (recursive) {
      const missing: string[] = [];
      let current = target;
      while (current !== "/" && !directories.has(current)) {
        if (files.has(current)) return { exit_code: 1, output: "mkdir: 路径被文件占用" };
        missing.push(current);
        current = parentPath(current);
      }
      if (current !== "/" && !directories.has(current)) return { exit_code: 1, output: "mkdir: 父目录不存在" };
      missing.forEach((directory) => directories.add(directory));
      return { exit_code: 0, output: "" };
    }
    if (!directories.has(parent)) return { exit_code: 1, output: "mkdir: 父目录不存在" };
    directories.add(target);
    return { exit_code: 0, output: "" };
  }
  if (op === "rm") {
    if (operands.length !== 1) return { exit_code: 2, output: "rm: 缺少或多余路径" };
    const target = operands[0]!;
    if (!exists(target)) return { exit_code: 1, output: "rm: 路径不存在" };
    if (directories.has(target) && !options.some((option) => /^-[^-]*[rR]/.test(option))) {
      return { exit_code: 1, output: "rm: 目录需要递归删除" };
    }
    for (const dir of [...directories]) if (dir === target || dir.startsWith(`${target}/`)) directories.delete(dir);
    for (const file of [...files.keys()]) if (file === target || file.startsWith(`${target}/`)) files.delete(file);
    return { exit_code: 0, output: "" };
  }
  if (op === "mv") {
    if (operands.length !== 2) return { exit_code: 2, output: "mv: 需要源路径和目标路径" };
    const [source, target] = operands as [string, string];
    const targetParent = parentPath(target);
    if (!exists(source) || !directories.has(targetParent) || exists(target) || target.startsWith(`${source}/`)) {
      return { exit_code: 1, output: "mv: 源或目标路径无效" };
    }
    for (const [file, content] of [...files]) if (file === source || file.startsWith(`${source}/`)) { files.set(`${target}${file.slice(source.length)}`, content); files.delete(file); }
    for (const directory of [...directories]) if (directory === source || directory.startsWith(`${source}/`)) { directories.add(`${target}${directory.slice(source.length)}`); directories.delete(directory); }
    return { exit_code: 0, output: "" };
  }
  return { exit_code: 127, output: `不支持本地 mock 命令：${op ?? "（空命令）"}` };
}

const e2eNoVncHandlers = import.meta.env.VITE_WORKBENCH_E2E === "1" ? [
  http.get("/code-server/", () => new HttpResponse(codeServerMockHtml, {
    headers: { "Content-Type": "text/html; charset=utf-8" },
  })),
  http.get("/vnc/index.html", ({ request }) => {
    const state = new URL(request.url).searchParams.get("morde_mock_state");
    return new HttpResponse(noVncMockHtml(state !== "connecting"), {
      headers: { "Content-Type": "text/html; charset=utf-8" },
    });
  }),
] : [];

export const handlers = [
  ...e2eNoVncHandlers,
  http.get("/__ch/api/host", () => { resetControl(); resetApplications(); return json(host); }),
  http.get("/__ch/api/applications", () => {
    const operation = applications.items[1]!.install_operation;
    if (operation?.status === "accepted" || operation?.status === "running") {
      applicationInstallPolls += 1;
      operation.status = applicationInstallPolls >= 2 ? "succeeded" : "running";
      operation.progress_percent = applicationInstallPolls >= 2 ? 100 : 45;
      operation.updated_at = now();
      if (operation.status === "succeeded") {
        applications.items[1]!.installed = true;
        applications.items[1]!.version = "4.1.13.26";
      }
    }
    return json(applications);
  }),
  http.get("/__ch/api/applications/:app_id/launches/current", ({ params }) =>
    params.app_id === "wechat" ? json(launchResponse()) : json({ error: { code: "not_found", message: "应用不存在" } }, 404)),
  http.get("/__ch/api/applications/:app_id/launches/:launch_id", ({ params }) => {
    if (params.app_id !== "wechat") return json({ error: { code: "not_found", message: "应用不存在" } }, 404);
    if (!applicationLaunch || params.launch_id !== applicationLaunch.id) return json({ error: { code: "not_found", message: "应用启动记录不存在" } }, 404);
    reconcileLaunch();
    return json(launchResponse());
  }),
  http.post("/__ch/api/applications/:app_id/launch", async ({ request, params }) => {
    if (params.app_id !== "wechat") return json({ error: { code: "not_found", message: "应用不存在" } }, 404);
    const body = await request.json().catch(() => null) as { launch_id?: string } | null;
    if (!body || typeof body.launch_id !== "string" || !/^launch_[A-Za-z0-9][A-Za-z0-9._:-]{0,120}$/.test(body.launch_id)) {
      return json({ error: { code: "invalid_argument", message: "请求参数无效" } }, 400);
    }
    if (applicationLaunch?.id === body.launch_id) return json(launchResponse(), 202);
    if (applicationLaunch && !["stopped", "exited", "failed"].includes(applicationLaunch.status)) return json({ error: { code: "conflict", message: "已有应用正在运行" } }, 409);
    applicationLaunch = { id: body.launch_id, application_id: "wechat", status: "starting", error_code: "", created_at: now(), updated_at: now() };
    applicationLaunchPolls = 0;
    return json(launchResponse(), 202);
  }),
  http.post("/__ch/api/applications/:app_id/launches/:launch_id/stop", ({ params }) => {
    if (params.app_id !== "wechat") return json({ error: { code: "not_found", message: "应用不存在" } }, 404);
    if (!applicationLaunch || params.launch_id !== applicationLaunch.id) return json({ error: { code: "not_found", message: "应用启动记录不存在" } }, 404);
    if (applicationLaunch.status === "running") {
      applicationLaunch.status = "stopping";
      applicationLaunchPolls = 0;
    }
    return json(launchResponse(), 202);
  }),
  http.post("/__ch/api/applications/:app_id/install", async ({ request, params }) => {
    if (params.app_id !== "wechat") return json({ error: { code: "not_found", message: "应用不存在" } }, 404);
    const body = await request.json().catch(() => null) as { request_id?: string } | null;
    if (!body || typeof body.request_id !== "string" || body.request_id.length < 1) {
      return json({ error: { code: "invalid_argument", message: "请求参数无效" } }, 400);
    }
    let operation = applicationInstallRequests.get(body.request_id);
    if (!operation) {
      operation = {
        id: `app_${String(applicationInstallRequests.size + 1).padStart(24, "0")}`,
        application_id: "wechat",
        status: "accepted",
        progress_percent: 0,
        error_code: "",
        created_at: now(),
        updated_at: now(),
      };
      applicationInstallRequests.set(body.request_id, operation);
      applications.items[1]!.install_operation = operation;
      applicationInstallPolls = 0;
    }
    return json({ host_id: applications.host_id, runtime_generation: applications.runtime_generation, runtime_instance_id: applications.runtime_instance_id, control_version: applications.control_version, operation }, 202);
  }),
  http.post("/__ch/api/renew", () => { host.expires_at = new Date(Date.now() + 8 * 60 * 60 * 1000).toISOString(); return json(host); }),
  http.get("/__ch/api/control", () => json(controlResponse())),
  http.post("/__ch/api/control/actions", async ({ request }) => {
    const body = await request.json().catch(() => null) as { action?: string; request_id?: string; expected_control_version?: number } | null;
    if (!body || !body.request_id || !["take_over", "release"].includes(body.action ?? "")) {
      return json({ error: { code: "invalid_argument", message: "控制操作参数无效" } }, 400);
    }
    const action = body.action as "take_over" | "release";
    if (action === "take_over" && !failedControlRequests.has(body.request_id)) {
      failedControlRequests.add(body.request_id);
      return json({ error: { code: "runtime_unavailable", message: "运行端暂不可用，请重试" } }, 503);
    }
    if (body.expected_control_version !== control.control_version) {
      return json({ error: { code: "conflict", message: "控制状态已变化，请刷新后重试" } }, 409);
    }
    if ((action === "take_over" && control.control_state !== "ai") || (action === "release" && control.control_state !== "user")) {
      return json({ error: { code: "invalid_state", message: "当前控制状态不支持此操作" } }, 409);
    }
    control.control_state = action === "take_over" ? "taking_over" : "returning";
    control.control_version += 1;
    control.active_transition = {
      request_id: body.request_id,
      action,
      phase: "draining",
      failure_reason: "",
      expected_control_version: body.expected_control_version,
    };
    controlPolls = 0;
    return json({ ...control, active_transition: { ...control.active_transition }, pending_operation_ids: ["mock-operation"] }, 202);
  }),
  http.put("/__ch/api/terminal-session", () => json(terminalSessionUrl
    ? { state: "ready", url: terminalSessionUrl }
    : createMockTerminalSession())),
  http.post("/__ch/api/terminal-session", async ({ request }) => {
    const body = await request.json().catch(() => null) as { confirm_new?: boolean } | null;
    return body?.confirm_new === true
      ? json(createMockTerminalSession())
      : json({ error: { code: "invalid_argument", message: "请确认是否创建新终端" } }, 400);
  }),
  http.get("/terminal", () => new HttpResponse(terminalHtml, { headers: { "Content-Type": "text/html; charset=utf-8" } })),
  http.post("/v1/shell/exec", async ({ request }) => { const body = await request.json().catch(() => ({})) as { command?: string }; const result = runMockCommand(body.command ?? ""); return aio({ ...result, output: result.output ?? "", status: "completed" }); }),
  http.post("/v1/file/list", async ({ request }) => { const body = await request.json().catch(() => ({})) as { path?: string; show_hidden?: boolean }; const path = body.path ?? "/home/gem/workspace"; return directories.has(path) ? aio({ path, files: entries(path, body.show_hidden === true) }) : aio({ error_type: "not_found", errno_name: "ENOENT" }, false, "目录不存在"); }),
  http.post("/v1/file/read", async ({ request }) => { const body = await request.json().catch(() => ({})) as { file?: string }; const file = body.file ?? ""; const content = files.get(file); if (!content) return aio({ path: file, error_type: "not_found", errno_name: "ENOENT" }, false, "文件不存在"); let text: string; try { text = new TextDecoder("utf-8", { fatal: true }).decode(await content.arrayBuffer()); } catch { return aio({ file, error_type: "decode_error" }, false, "文件不是有效的 UTF-8 文本，无法预览"); } return aio({ file, content: text, line_count: text.split("\n").length }); }),
  http.post("/v1/file/write", async ({ request }) => { const body = await request.json().catch(() => ({})) as { file?: string; content?: string }; if (!body.file) return aio({ error_type: "invalid_path" }, false, "缺少文件路径"); if (!directories.has(parentPath(body.file))) return aio({ error_type: "not_found", errno_name: "ENOENT" }, false, "父目录不存在"); const content = new Blob([body.content ?? ""]); files.set(body.file, content); return aio({ file: body.file, bytes_written: content.size }); }),
  http.post("/v1/file/upload", async ({ request }) => { const form = await request.formData(); const file = form.get("file"); const path = String(form.get("path") ?? ""); if (!(file instanceof File)) return aio({ error_type: "invalid_path" }, false, "未选择文件"); const destination = path || `/home/gem/workspace/${file.name}`; if (!directories.has(parentPath(destination))) return aio({ error_type: "not_found", errno_name: "ENOENT" }, false, "父目录不存在"); if (directories.has(destination)) return aio({ error_type: "is_directory" }, false, "目标路径是目录，无法覆盖"); files.set(destination, file); return aio({ path: destination, bytes_written: file.size }); }),
  http.get("/v1/file/download", ({ request }) => { const path = new URL(request.url).searchParams.get("path") ?? ""; const content = files.get(path); return content === undefined ? new HttpResponse("not found", { status: 404 }) : new HttpResponse(content, { headers: { "Content-Type": "application/octet-stream", "Content-Disposition": `attachment; filename*=UTF-8''${encodeURIComponent(path.split("/").at(-1) ?? "download")}` } }); }),
];
