"use strict";
Object.defineProperty(exports, "__esModule", { value: true });
exports.isSystemDirectory = isSystemDirectory;
const electron_1 = require("electron");
const node_crypto_1 = require("node:crypto");
const node_fs_1 = require("node:fs");
const node_path_1 = require("node:path");
const morde_core_1 = require("./morde-core");
const morde_sync_1 = require("./morde-sync");
electron_1.app.setName("Morde");
const userData = (0, node_path_1.join)(electron_1.app.getPath("appData"), "Morde Client");
(0, node_fs_1.mkdirSync)(userData, { recursive: true, mode: 0o700 });
electron_1.app.setPath("userData", userData);
const zh = (electron_1.app.getLocale() || "").toLowerCase().startsWith("zh");
let mainWindow = null;
let tray = null;
let credentials = null;
let lastOrigin = null;
let lastInstallation = null;
let activeProfile = null;
let mappings = [];
let syncCheckpoint = null;
let profileError = null;
let syncChildPid;
let webView = null;
let webPartition = null;
let webAttached = false;
let view = "web";
let webRefreshTimer = null;
let credentialExpiryTimer = null;
let authorizationCheckTimer = null;
let logoutInProgress = false;
let exitReady = false;
const hostWindows = new Set();
const api = new morde_core_1.MordeAPI(() => credentials);
const sync = new morde_sync_1.MordeSync(() => electron_1.app.isPackaged ? (0, node_path_1.join)(process.resourcesPath, "bin", process.platform === "win32" ? "morde-sync.exe" : "morde-sync") : process.env.MORDE_SYNC_BINARY || "", (state, progress) => {
    if (syncCheckpoint && progress?.session_id && /^p[a-z0-9]{7}$/.test(progress.session_id) && progress.session_id !== syncCheckpoint.sessionId) {
        syncCheckpoint.sessionId = progress.session_id;
        try {
            savePrivate(profileFile("sync-checkpoint"), JSON.stringify(syncCheckpoint));
        }
        catch {
            void sync.stop();
            mainWindow?.webContents.send("morde:sync-state", { state: "shutdown_unknown", message: "Could not save the sync recovery point" });
        }
    }
    mainWindow?.webContents.send("morde:sync-state", { state, progress });
}, (pid) => {
    if (!syncCheckpoint)
        throw new Error("Sync recovery point is missing");
    syncChildPid = pid;
    syncCheckpoint.childPid = pid;
    savePrivate(profileFile("sync-checkpoint"), JSON.stringify(syncCheckpoint));
});
function isSystemDirectory(candidate, root) {
    const path = root.includes("\\") || /^[A-Za-z]:/.test(root) ? node_path_1.win32 : node_path_1.posix;
    if (candidate === root)
        return true;
    if (path.normalize(root) === path.parse(root).root)
        return false;
    const relativePath = path.relative(root, candidate);
    return relativePath !== ".." && !relativePath.startsWith(`..${path.sep}`) && !path.isAbsolute(relativePath);
}
function childMayStillRun(checkpoint) { return sync.checkpointMayOwnDirectory(checkpoint); }
function priorSyncChildMayStillRun() { return Boolean(syncCheckpoint?.childPid && syncCheckpoint.childPid !== syncChildPid && childMayStillRun(syncCheckpoint)); }
function clearConfirmedChild() {
    if (!syncCheckpoint?.childPid)
        return true;
    if (childMayStillRun(syncCheckpoint))
        return false;
    const childPid = syncCheckpoint.childPid;
    const { childPid: _childPid, ...confirmed } = syncCheckpoint;
    savePrivate(profileFile("sync-checkpoint"), JSON.stringify(confirmed));
    syncCheckpoint = confirmed;
    if (syncChildPid === childPid)
        syncChildPid = undefined;
    return true;
}
function settingsPath(name) { return (0, node_path_1.join)(electron_1.app.getPath("userData"), name); }
function profileID(auth) { return (0, node_crypto_1.createHash)("sha256").update(`${auth.origin}\0${auth.accountName}`).digest("hex"); }
function profileFile(name) {
    if (!activeProfile)
        throw new Error("Log in before changing local Workspace state");
    return `${name}-${activeProfile}.json`;
}
function savePrivate(name, value) {
    const target = settingsPath(name);
    (0, node_fs_1.mkdirSync)(electron_1.app.getPath("userData"), { recursive: true, mode: 0o700 });
    const pending = `${target}.${(0, node_crypto_1.randomUUID)()}.tmp`;
    (0, node_fs_1.writeFileSync)(pending, value, { mode: 0o600 });
    (0, node_fs_1.renameSync)(pending, target);
}
function readOptional(name) {
    try {
        return (0, node_fs_1.readFileSync)(settingsPath(name), "utf8");
    }
    catch (error) {
        if (error.code === "ENOENT")
            return null;
        throw error;
    }
}
function safeStorageReady() {
    if (!electron_1.safeStorage.isEncryptionAvailable())
        return false;
    return process.platform !== "linux" || ["gnome_libsecret", "kwallet", "kwallet5", "kwallet6"].includes(electron_1.safeStorage.getSelectedStorageBackend());
}
function loadProfile(auth) {
    activeProfile = profileID(auth);
    mappings = [];
    syncCheckpoint = null;
    profileError = null;
    // shortcut: Unscoped legacy files need matching installation metadata; otherwise the user must select the folder again.
    const legacy = lastInstallation?.origin === auth.origin && lastInstallation.name === auth.accountName && lastInstallation.id === auth.installationId;
    const mappingFile = profileFile("mappings");
    const checkpointFile = profileFile("sync-checkpoint");
    let source = mappingFile;
    try {
        let raw = readOptional(source);
        if (raw === null && legacy) {
            source = "mappings.json";
            raw = readOptional(source);
        }
        if (raw !== null) {
            const saved = JSON.parse(raw);
            if (!Array.isArray(saved) || !saved.every((m) => m && /^w[a-z0-9]{7}$/.test(m.workspaceId) && typeof m.directory === "string" && typeof m.dev === "string" && typeof m.ino === "string"))
                throw new Error("Invalid Workspace mapping");
            mappings = saved;
            if (source !== mappingFile)
                savePrivate(mappingFile, raw);
        }
    }
    catch {
        profileError = `本机同步配置无法读取：${settingsPath(source)}。同步已暂停，请备份并修复该文件。`;
    }
    source = checkpointFile;
    try {
        let raw = readOptional(source);
        if (raw === null && legacy) {
            source = "sync-checkpoint.json";
            raw = readOptional(source);
        }
        if (raw !== null) {
            const saved = JSON.parse(raw);
            if (!saved || typeof saved.installationId !== "string" || !/^w[a-z0-9]{7}$/.test(saved.workspaceId) || typeof saved.directory !== "string" || typeof saved.dev !== "string" || typeof saved.ino !== "string" || !/^[A-Za-z0-9._~-]{1,128}$/.test(saved.idempotencyKey) || !["upload", "download", "bidirectional", "live_sync", "handoff_checkpoint"].includes(saved.mode) || (saved.sessionId && !/^p[a-z0-9]{7}$/.test(saved.sessionId)) || (saved.childPid !== undefined && (!Number.isSafeInteger(saved.childPid) || saved.childPid <= 0)))
                throw new Error("Invalid sync recovery point");
            syncCheckpoint = saved;
            if (source !== checkpointFile)
                savePrivate(checkpointFile, raw);
        }
    }
    catch {
        profileError ??= `本机同步恢复点无法读取：${settingsPath(source)}。同步已暂停，请备份并修复该文件。`;
    }
}
function loadState() {
    try {
        lastOrigin = (0, morde_core_1.portalOrigin)((0, node_fs_1.readFileSync)(settingsPath("service-origin.txt"), "utf8").trim());
    }
    catch {
        lastOrigin = null;
    }
    try {
        const saved = JSON.parse((0, node_fs_1.readFileSync)(settingsPath("installation.json"), "utf8"));
        if (saved && saved.origin === (0, morde_core_1.portalOrigin)(saved.origin) && typeof saved.name === "string" && saved.name && /^i[a-z0-9]{7}$/.test(saved.id))
            lastInstallation = saved;
    }
    catch {
        lastInstallation = null;
    }
    try {
        if (safeStorageReady() && (0, node_fs_1.existsSync)(settingsPath("credentials.bin"))) {
            const saved = JSON.parse(electron_1.safeStorage.decryptString((0, node_fs_1.readFileSync)(settingsPath("credentials.bin"))));
            const auth = saved.version === 2 ? saved.credentials : null;
            if (auth && /^i[a-z0-9]{7}$/.test(auth.installationId) && typeof auth.clientCredential === "string" && auth.clientCredential && !(0, morde_core_1.credentialExpired)(auth.clientExpiresAt)) {
                auth.origin = (0, morde_core_1.portalOrigin)(auth.origin);
                if (!auth.accountName && lastInstallation?.origin === auth.origin && lastInstallation.id === auth.installationId)
                    auth.accountName = lastInstallation.name;
                if (typeof auth.accountName !== "string" || !auth.accountName)
                    throw new Error("Local account identity is missing");
                credentials = auth;
                lastOrigin = auth.origin;
            }
            else
                (0, node_fs_1.rmSync)(settingsPath("credentials.bin"), { force: true });
        }
    }
    catch {
        credentials = null;
    }
    if (credentials)
        loadProfile(credentials);
}
function publicState() {
    if (credentials && (0, morde_core_1.credentialExpired)(credentials.clientExpiresAt))
        clearAuthorization();
    return { authenticated: Boolean(credentials), origin: credentials?.origin ?? lastOrigin, installationId: credentials?.installationId ?? null, expiresAt: credentials?.clientExpiresAt ?? null, expiresSoon: credentials ? (0, morde_core_1.credentialExpiresSoon)(credentials.clientExpiresAt) : false, mappings: credentials ? mappings : [], syncState: sync.snapshot(), syncCheckpoint: credentials && syncCheckpoint ? { workspaceId: syncCheckpoint.workspaceId, mode: syncCheckpoint.mode, installationId: syncCheckpoint.installationId } : null, profileError: credentials ? profileError : null, storageReady: safeStorageReady(), view };
}
function notifyState() { mainWindow?.webContents.send("morde:state", publicState()); }
function clearTimer(timer) { if (timer)
    clearTimeout(timer); return null; }
function closeHostWindows() { for (const window of hostWindows)
    window.close(); }
function detachWeb() {
    closeHostWindows();
    if (webView) {
        if (mainWindow && !mainWindow.isDestroyed() && webAttached)
            mainWindow.contentView.removeChildView(webView);
        webAttached = false;
        if (!webView.webContents.isDestroyed())
            webView.webContents.close();
        webView = null;
    }
    if (webPartition)
        void webPartition.clearStorageData({ storages: ["cookies", "localstorage"] });
    webPartition = null;
    webRefreshTimer = clearTimer(webRefreshTimer);
}
function clearAuthorization() {
    if (!credentials)
        return;
    credentials = null;
    detachWeb();
    credentialExpiryTimer = clearTimer(credentialExpiryTimer);
    authorizationCheckTimer = clearTimer(authorizationCheckTimer);
    (0, node_fs_1.rmSync)(settingsPath("credentials.bin"), { force: true });
    void sync.stop().then((stopped) => { if (stopped)
        clearConfirmedChild(); }).catch(() => undefined);
    notifyState();
}
function scheduleCredentialExpiry() {
    credentialExpiryTimer = clearTimer(credentialExpiryTimer);
    const auth = credentials;
    if (!auth)
        return;
    const expiry = Date.parse(auth.clientExpiresAt);
    if (!Number.isFinite(expiry) || expiry <= Date.now()) {
        clearAuthorization();
        return;
    }
    credentialExpiryTimer = setTimeout(() => {
        if (credentials !== auth)
            return;
        if ((0, morde_core_1.credentialExpired)(auth.clientExpiresAt))
            clearAuthorization();
        else
            scheduleCredentialExpiry();
    }, Math.min(expiry - Date.now(), 2_147_483_647));
    credentialExpiryTimer.unref();
}
function scheduleAuthorizationCheck() {
    authorizationCheckTimer = clearTimer(authorizationCheckTimer);
    const auth = credentials;
    if (!auth)
        return;
    authorizationCheckTimer = setTimeout(() => {
        void api.me().catch((error) => { if (credentials === auth && String(error).includes("Authorization expired or revoked"))
            clearAuthorization(); })
            .finally(() => { if (credentials === auth)
            scheduleAuthorizationCheck(); });
    }, 60_000);
    authorizationCheckTimer.unref();
}
function sameOriginURL(value, origin) {
    try {
        const url = new URL(value);
        return url.origin === origin && !url.username && !url.password ? url : null;
    }
    catch {
        return null;
    }
}
function hostEntryURL(value, origin) {
    const url = sameOriginURL(value, origin);
    if (!url || url.hash || !/^\/host-entry\/h[a-z0-9]{7}$/.test(url.pathname) || [...url.searchParams.keys()].some((key) => key !== "target") || url.searchParams.getAll("target").length > 1)
        return null;
    const target = url.searchParams.get("target");
    if (target && target !== "desktop" && target !== "terminal")
        return null;
    return { id: url.pathname.slice("/host-entry/".length), target };
}
async function hostOrigin(id, shared, serviceOrigin) {
    const value = shared ? (await api.sharedHosts()).items?.find((item) => item.id === id) : await api.host(id);
    if (!value?.url)
        throw new Error("Host is unavailable");
    const url = new URL(value.url);
    if ((url.protocol !== "https:" && !(0, morde_core_1.localGatewayOrigin)(url, serviceOrigin)) || url.username || url.password)
        throw new Error("Invalid Host origin");
    return url.origin;
}
function guardHostWindow(win, entry, auth) {
    hostWindows.add(win);
    win.on("closed", () => hostWindows.delete(win));
    let allowedGateway = null;
    const returnToMain = () => {
        if (!win.isDestroyed())
            win.close();
        if (credentials === auth && mainWindow && !mainWindow.isDestroyed()) {
            showView("web");
            mainWindow.focus();
        }
    };
    win.webContents.setWindowOpenHandler(() => ({ action: "deny" }));
    win.webContents.on("will-navigate", (event, value) => {
        const same = sameOriginURL(value, auth.origin);
        if (same?.pathname === `/host-entry/${entry.id}`)
            return;
        if (same?.pathname === "/hosts") {
            event.preventDefault();
            returnToMain();
            return;
        }
        try {
            if (allowedGateway && new URL(value).origin === allowedGateway)
                return;
        }
        catch { /* Reject malformed navigation. */ }
        let target;
        try {
            target = (0, morde_core_1.gatewayURL)(value, auth.origin);
        }
        catch {
            event.preventDefault();
            return;
        }
        const origin = new URL(target).origin;
        if (allowedGateway === origin && credentials === auth)
            return;
        event.preventDefault();
        void hostOrigin(entry.id, Boolean(entry.target), auth.origin).then((expected) => {
            if (expected !== origin || credentials !== auth || win.isDestroyed())
                throw new Error("Host ticket target changed");
            allowedGateway = origin;
            return win.loadURL(target);
        }).catch((error) => { if (!win.isDestroyed())
            void electron_1.dialog.showMessageBox(win, { type: "error", message: String(error) }); });
    });
    win.webContents.on("will-redirect", (event, value) => {
        const url = sameOriginURL(value, auth.origin);
        if (url?.pathname === "/hosts") {
            event.preventDefault();
            returnToMain();
            return;
        }
        const portalEntry = url?.pathname === `/host-entry/${entry.id}`;
        try {
            if (!portalEntry && new URL(value).origin !== allowedGateway)
                event.preventDefault();
        }
        catch {
            event.preventDefault();
        }
    });
    win.webContents.on("did-navigate-in-page", (_event, value) => {
        if (sameOriginURL(value, auth.origin)?.pathname === "/hosts")
            returnToMain();
    });
}
function installDownloadAndClipboard(partition, auth) {
    partition.on("will-download", (_event, item, contents) => {
        const source = contents.getURL();
        if (!sameOriginURL(source, auth.origin) && ![...hostWindows].some((win) => win.webContents === contents)) {
            item.cancel();
            return;
        }
        item.setSaveDialogOptions({ title: "保存 Morde 文件" });
    });
    partition.setPermissionRequestHandler((contents, permission, callback) => {
        const trusted = Boolean(sameOriginURL(contents.getURL(), auth.origin)) || [...hostWindows].some((win) => win.webContents === contents);
        callback(trusted && permission === "clipboard-sanitized-write");
    });
}
function positionWeb() {
    if (!mainWindow || !webView)
        return;
    const bounds = mainWindow.getContentBounds();
    webView.setBounds({ x: 0, y: 64, width: bounds.width, height: Math.max(0, bounds.height - 64) });
}
function showView(next) {
    view = next;
    if (mainWindow && webView) {
        if (next === "web" && !webAttached) {
            mainWindow.contentView.addChildView(webView);
            webAttached = true;
            positionWeb();
            webView.webContents.focus();
        }
        if (next === "sync" && webAttached) {
            mainWindow.contentView.removeChildView(webView);
            webAttached = false;
        }
    }
    notifyState();
}
async function refreshWebSession(auth, initial) {
    if (credentials !== auth || !webPartition)
        return;
    try {
        const next = initial ?? await api.webSession();
        if (credentials !== auth || !webPartition)
            return;
        await webPartition.cookies.set({ url: auth.origin, name: "ch_portal", value: next.credential, path: "/", httpOnly: true, secure: auth.origin.startsWith("https:"), sameSite: "strict", expirationDate: Date.parse(next.expiresAt) / 1000 });
        webRefreshTimer = clearTimer(webRefreshTimer);
        webRefreshTimer = setTimeout(() => { void refreshWebSession(auth).catch(() => undefined); }, Math.max(1000, Math.min(Date.parse(next.expiresAt) - Date.now() - 60_000, 2_147_483_647)));
        webRefreshTimer.unref();
    }
    catch (error) {
        if (credentials !== auth)
            return;
        if (String(error).includes("Authorization expired or revoked")) {
            clearAuthorization();
            return;
        }
        webRefreshTimer = clearTimer(webRefreshTimer);
        webRefreshTimer = setTimeout(() => { void refreshWebSession(auth).catch(() => undefined); }, 30_000);
        webRefreshTimer.unref();
        throw error;
    }
}
async function openWeb(initial) {
    const auth = credentials;
    if (!auth)
        throw new Error("Log in to Morde first");
    if (!webPartition) {
        webPartition = electron_1.session.fromPartition(`morde-user-${(0, node_crypto_1.randomUUID)()}`);
        installDownloadAndClipboard(webPartition, auth);
        webPartition.webRequest.onCompleted({ urls: [`${auth.origin}/api/auth/logout`] }, (details) => {
            if (credentials === auth && details.statusCode === 204 && details.method === "POST")
                clearAuthorization();
        });
    }
    await refreshWebSession(auth, initial);
    if (credentials !== auth)
        return;
    if (!webView) {
        const content = new electron_1.WebContentsView({ webPreferences: { session: webPartition, contextIsolation: true, nodeIntegration: false, sandbox: true, webSecurity: true } });
        webView = content;
        const restoreFromLogin = () => {
            void refreshWebSession(auth).then(() => {
                if (credentials === auth && !content.webContents.isDestroyed())
                    return content.webContents.loadURL(auth.origin);
            }).catch(() => { if (credentials === auth)
                showView("sync"); });
        };
        content.webContents.setWindowOpenHandler((details) => {
            const entry = hostEntryURL(details.url, auth.origin);
            if (!entry || credentials !== auth) {
                const target = sameOriginURL(details.url, auth.origin);
                if (target?.pathname.startsWith("/admin"))
                    void electron_1.shell.openExternal(target.href);
                return { action: "deny" };
            }
            return { action: "allow", createWindow: (options) => {
                    const win = new electron_1.BrowserWindow({ ...options, width: 1280, height: 800, title: "Morde Host", webPreferences: { ...options.webPreferences, session: webPartition, contextIsolation: true, nodeIntegration: false, sandbox: true, webSecurity: true } });
                    guardHostWindow(win, entry, auth);
                    return win.webContents;
                } };
        });
        const guardNavigation = (event, value) => {
            const target = sameOriginURL(value, auth.origin);
            if (!target || target.pathname.startsWith("/admin") || target.pathname === "/login") {
                event.preventDefault();
                if (target?.pathname.startsWith("/admin"))
                    void electron_1.shell.openExternal(target.href);
                else if (target?.pathname === "/login")
                    restoreFromLogin();
            }
        };
        content.webContents.on("will-navigate", guardNavigation);
        content.webContents.on("will-redirect", guardNavigation);
        content.webContents.on("did-navigate-in-page", (_event, value) => {
            const target = sameOriginURL(value, auth.origin);
            if (credentials !== auth)
                return;
            if (target?.pathname === "/login")
                restoreFromLogin();
            if (target?.pathname.startsWith("/admin")) {
                void electron_1.shell.openExternal(target.href);
                void content.webContents.loadURL(auth.origin);
            }
        });
        await content.webContents.loadURL(auth.origin);
    }
    showView("web");
}
async function login(origin, name, password, key) {
    if (credentials)
        throw new Error("Sign out before changing accounts");
    if (sync.snapshot() !== "idle" || childMayStillRun(syncCheckpoint))
        throw new Error("Finish the previous Workspace sync before changing accounts");
    if (!safeStorageReady())
        throw new Error("Secure operating system storage is unavailable");
    if (typeof key !== "string")
        throw new Error("Invalid login request");
    const normalizedOrigin = (0, morde_core_1.portalOrigin)(origin);
    const normalizedName = typeof name === "string" ? name.trim() : "";
    const previousID = lastInstallation?.origin === normalizedOrigin && lastInstallation.name === normalizedName ? lastInstallation.id : undefined;
    const result = await api.login(normalizedOrigin, name, password, key, previousID);
    const previousInstallation = lastInstallation;
    const previousProfile = activeProfile, previousMappings = mappings, previousCheckpoint = syncCheckpoint, previousProfileError = profileError, previousChildPid = syncChildPid;
    credentials = result.credentials;
    try {
        loadProfile(result.credentials);
        if (activeProfile !== previousProfile)
            syncChildPid = undefined;
        savePrivate("credentials.bin", electron_1.safeStorage.encryptString(JSON.stringify({ version: 2, credentials: result.credentials })));
        savePrivate("service-origin.txt", result.credentials.origin);
        savePrivate("installation.json", JSON.stringify({ origin: result.credentials.origin, name: normalizedName, id: result.credentials.installationId }));
        lastOrigin = credentials.origin;
        lastInstallation = { origin: credentials.origin, name: normalizedName, id: credentials.installationId };
        scheduleCredentialExpiry();
        scheduleAuthorizationCheck();
        await openWeb(result.webSession);
        return publicState();
    }
    catch (error) {
        void new morde_core_1.MordeAPI(() => result.credentials).logout().catch(() => undefined);
        clearAuthorization();
        activeProfile = previousProfile;
        mappings = previousMappings;
        syncCheckpoint = previousCheckpoint;
        profileError = previousProfileError;
        syncChildPid = previousChildPid;
        lastInstallation = previousInstallation;
        try {
            if (previousInstallation)
                savePrivate("installation.json", JSON.stringify(previousInstallation));
            else
                (0, node_fs_1.rmSync)(settingsPath("installation.json"), { force: true });
        }
        catch { /* Login still fails closed when local metadata cannot be restored. */ }
        throw error;
    }
}
async function logout() {
    if (!credentials)
        return null;
    if (logoutInProgress)
        throw new Error("Sign-out is already in progress");
    logoutInProgress = true;
    let warning = null;
    try {
        try {
            await api.logout();
        }
        catch {
            warning = "本机已退出。网络请求未完成，请在 Portal 的客户端列表中撤销此设备。";
        }
        const stopped = priorSyncChildMayStillRun() ? false : await sync.stop();
        if (stopped) {
            try {
                if (!clearConfirmedChild())
                    warning = `${warning ?? ""} 同步进程仍可能持有目录；恢复点已保留。`.trim();
            }
            catch {
                warning = `${warning ?? ""} 同步恢复点保存失败，请检查本机目录。`.trim();
            }
        }
        else
            warning = `${warning ?? ""} 同步进程停止状态未确认；恢复点已保留。`.trim();
        clearAuthorization();
        lastInstallation = null;
        (0, node_fs_1.rmSync)(settingsPath("installation.json"), { force: true });
        return warning;
    }
    finally {
        logoutInProgress = false;
    }
}
function sendError(error) {
    const message = error instanceof Error ? error.message : "Morde request failed";
    if (message.includes("Authorization expired or revoked"))
        clearAuthorization();
    else if (message.includes("HTTP 403"))
        void sync.stop();
    throw new Error(message);
}
function handler(channel, fn) {
    electron_1.ipcMain.handle(channel, async (event, ...args) => {
        if (!mainWindow || event.sender !== mainWindow.webContents)
            throw new Error("Invalid desktop window");
        if (logoutInProgress && channel !== "morde:logout")
            throw new Error("Sign-out is in progress");
        if (credentials && (0, morde_core_1.credentialExpired)(credentials.clientExpiresAt))
            clearAuthorization();
        try {
            return await fn(...args);
        }
        catch (error) {
            return sendError(error);
        }
    });
}
function register() {
    handler("morde:state", () => publicState());
    handler("morde:login", login);
    handler("morde:logout", logout);
    handler("morde:show-web", async () => { try {
        await openWeb();
    }
    catch (error) {
        showView("sync");
        throw error;
    } });
    handler("morde:show-sync", () => { if (!credentials)
        throw new Error("Log in to Morde first"); showView("sync"); });
    handler("morde:public-page", (path, requestedOrigin) => {
        if (path !== "register" && path !== "download")
            throw new Error("Invalid public page");
        const origin = credentials?.origin ?? (requestedOrigin ? (0, morde_core_1.portalOrigin)(requestedOrigin) : lastOrigin);
        if (!origin)
            throw new Error("Enter a service address and log in first");
        const partition = electron_1.session.fromPartition(`morde-public-${(0, node_crypto_1.randomUUID)()}`);
        partition.on("will-download", (_event, item) => item.setSaveDialogOptions({ title: "保存 Morde 安装包" }));
        partition.setPermissionRequestHandler((_contents, _permission, callback) => callback(false));
        const window = new electron_1.BrowserWindow({ width: 960, height: 720, title: "Morde", webPreferences: { session: partition, contextIsolation: true, nodeIntegration: false, sandbox: true, webSecurity: true } });
        window.webContents.setWindowOpenHandler(() => ({ action: "deny" }));
        window.webContents.on("will-navigate", (event, value) => {
            const target = sameOriginURL(value, origin);
            if (target?.pathname === "/login") {
                event.preventDefault();
                window.close();
                mainWindow?.focus();
                return;
            }
            if (!target || (target.pathname !== "/register" && target.pathname !== "/download")) {
                event.preventDefault();
                if (value.startsWith("https://"))
                    void electron_1.shell.openExternal(value);
            }
        });
        window.webContents.on("will-redirect", (event, value) => {
            const target = sameOriginURL(value, origin);
            if (!target || (target.pathname !== "/register" && target.pathname !== "/download"))
                event.preventDefault();
            if (target?.pathname === "/login") {
                window.close();
                mainWindow?.focus();
            }
        });
        window.webContents.on("did-navigate-in-page", (_event, value) => {
            if (sameOriginURL(value, origin)?.pathname === "/login") {
                window.close();
                mainWindow?.focus();
            }
        });
        void window.loadURL(`${origin}/${path}`);
    });
    handler("morde:workspaces", () => api.workspaces());
    handler("morde:choose-directory", async (workspaceId) => {
        const auth = credentials;
        if (!auth)
            throw new Error("Log in to Morde first");
        if (profileError)
            throw new Error(profileError);
        const id = (0, morde_core_1.checkedId)("workspace", workspaceId);
        if (sync.snapshot() !== "idle" || syncCheckpoint)
            throw new Error("Finish or discard the prior sync recovery point before changing folders");
        const workspace = await api.workspace(id);
        if (workspace.state !== "ready")
            throw new Error("Workspace is not ready for sync");
        const picked = await electron_1.dialog.showOpenDialog(mainWindow, { properties: ["openDirectory"] });
        if (picked.canceled || !picked.filePaths[0])
            return null;
        const appData = electron_1.app.getPath("userData");
        const systemRoots = process.platform === "win32" ? [process.env.SystemRoot, process.env.ProgramFiles].filter((value) => Boolean(value)) : ["/", "/System", "/Applications", "/Library", "/private", "/usr", "/etc", "/var", "/bin", "/sbin", "/dev", "/proc", "/sys", "/run"];
        const candidate = await (0, morde_sync_1.checkedDirectory)(picked.filePaths[0], [...mappings.filter((item) => item.workspaceId !== id), { workspaceId: "", directory: appData, dev: "", ino: "" }]);
        if (systemRoots.some((root) => root && isSystemDirectory(candidate.directory, root)))
            throw new Error("Choose a project folder outside system directories");
        if (credentials !== auth)
            throw new Error("Authorization changed while selecting a folder");
        const mapping = { ...candidate, workspaceId: id };
        const nextMappings = [...mappings.filter((item) => item.workspaceId !== id), mapping];
        savePrivate(profileFile("mappings"), JSON.stringify(nextMappings));
        mappings = nextMappings;
        notifyState();
        return mapping;
    });
    handler("morde:sync", async (workspaceId, mode) => {
        if (profileError)
            throw new Error(profileError);
        if (!["upload", "download", "bidirectional", "live_sync", "handoff_checkpoint"].includes(mode))
            throw new Error("Unsupported sync mode");
        const id = (0, morde_core_1.checkedId)("workspace", workspaceId);
        const auth = credentials;
        const mapping = mappings.find((item) => item.workspaceId === id);
        if (!auth || !mapping)
            throw new Error("Log in and choose a folder first");
        const workspace = await api.workspace(id);
        if (credentials !== auth || workspace.state !== "ready")
            throw new Error("Workspace is not ready for sync");
        if (childMayStillRun(syncCheckpoint))
            throw new Error("A prior sync process may still hold this folder. Stop it before recovery");
        if (syncCheckpoint && (syncCheckpoint.installationId !== auth.installationId || syncCheckpoint.workspaceId !== id || syncCheckpoint.mode !== mode || syncCheckpoint.directory !== mapping.directory || syncCheckpoint.dev !== mapping.dev || syncCheckpoint.ino !== mapping.ino))
            throw new Error("An earlier sync needs recovery or explicit discard before starting another");
        if (!syncCheckpoint) {
            syncCheckpoint = { installationId: auth.installationId, workspaceId: id, directory: mapping.directory, dev: mapping.dev, ino: mapping.ino, mode, idempotencyKey: (0, node_crypto_1.randomUUID)() };
            try {
                savePrivate(profileFile("sync-checkpoint"), JSON.stringify(syncCheckpoint));
            }
            catch (error) {
                syncCheckpoint = null;
                throw error;
            }
        }
        let result;
        try {
            result = await sync.run(auth, mapping, mode, [], syncCheckpoint, () => credentials === auth && !logoutInProgress);
        }
        finally {
            if (sync.snapshot() === "idle" && !childMayStillRun(syncCheckpoint))
                clearConfirmedChild();
        }
        const outcome = result;
        if (!outcome?.session || (outcome.session.state === "completed" && !outcome.conflicts?.length)) {
            syncCheckpoint = null;
            (0, node_fs_1.rmSync)(settingsPath(profileFile("sync-checkpoint")), { force: true });
        }
        return result;
    });
    handler("morde:sync-stop", async () => {
        if (priorSyncChildMayStillRun())
            throw new Error("A prior sync process may still hold this folder. Its recovery point was kept");
        const stopped = await sync.stop();
        if (stopped && !clearConfirmedChild())
            throw new Error("A prior sync process may still hold this folder. Its recovery point was kept");
        return stopped;
    });
    handler("morde:sync-discard", () => {
        if (profileError)
            throw new Error(profileError);
        if (sync.snapshot() === "running")
            throw new Error("Stop sync before discarding its recovery point");
        if (childMayStillRun(syncCheckpoint))
            throw new Error("A prior sync process may still hold this folder");
        if (!sync.discardCheckpoint(syncCheckpoint))
            throw new Error("A prior sync process may still hold this folder");
        syncCheckpoint = null;
        (0, node_fs_1.rmSync)(settingsPath(profileFile("sync-checkpoint")), { force: true });
        return publicState();
    });
}
function makeWindow() {
    const win = new electron_1.BrowserWindow({ width: 1120, height: 760, minWidth: 720, minHeight: 520, title: "Morde", show: false, webPreferences: { contextIsolation: true, nodeIntegration: false, sandbox: true, webSecurity: true, preload: (0, node_path_1.join)(__dirname, "morde-preload.js") } });
    win.webContents.setWindowOpenHandler(() => ({ action: "deny" }));
    win.webContents.on("will-navigate", (event) => event.preventDefault());
    win.on("resize", positionWeb);
    void win.loadFile((0, node_path_1.join)(__dirname, "../resources/morde.html"));
    win.once("ready-to-show", () => win.show());
    win.on("closed", () => { detachWeb(); mainWindow = null; });
    return win;
}
function showWindow() {
    if (!mainWindow) {
        mainWindow = makeWindow();
        if (credentials)
            void openWeb().catch((error) => { mainWindow?.webContents.send("morde:sync-state", { state: "offline", message: String(error) }); showView("sync"); });
    }
    else
        mainWindow.show();
    mainWindow.focus();
}
if (!electron_1.app.requestSingleInstanceLock())
    electron_1.app.quit();
else {
    electron_1.app.on("second-instance", showWindow);
    electron_1.app.whenReady().then(() => {
        loadState();
        scheduleCredentialExpiry();
        scheduleAuthorizationCheck();
        register();
        electron_1.Menu.setApplicationMenu(electron_1.Menu.buildFromTemplate([{ label: "Morde", submenu: [{ label: zh ? "打开" : "Open", click: showWindow }, { label: zh ? "退出" : "Quit", click: () => electron_1.app.quit() }] }]));
        showWindow();
        try {
            tray = new electron_1.Tray((0, node_path_1.join)(__dirname, "../resources/morde-icon.png"));
            tray.setToolTip("Morde");
            tray.setContextMenu(electron_1.Menu.buildFromTemplate([{ label: zh ? "打开 Morde" : "Open Morde", click: showWindow }, { label: zh ? "退出" : "Quit", click: () => electron_1.app.quit() }]));
            tray.on("double-click", showWindow);
        }
        catch {
            tray = null;
        }
    });
    electron_1.app.on("activate", showWindow);
    electron_1.app.on("window-all-closed", () => { if (process.platform !== "darwin" && !tray)
        electron_1.app.quit(); });
    electron_1.app.on("before-quit", (event) => {
        if (exitReady)
            return;
        event.preventDefault();
        const stopping = priorSyncChildMayStillRun() ? Promise.resolve(false) : sync.stop();
        void stopping.then((stopped) => {
            let cleared = false;
            try {
                cleared = stopped && clearConfirmedChild();
            }
            catch { /* Keep the recovery point when it cannot be saved. */ }
            if (!cleared)
                void electron_1.dialog.showMessageBox({ type: "warning", message: "Sync shutdown was not confirmed, or a prior sync process may still be running. The recovery point was kept." });
            exitReady = true;
            electron_1.app.quit();
        });
    });
}
