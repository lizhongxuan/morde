import { useCallback, useEffect, useRef, useState, type FormEvent } from "react";
import { ApiError, morde, newKey, validateHostTicketUrl, type Conversation, type Host, type Message, type Model, type Notification, type SharedHost, type Workspace } from "./api";

type Tab = "chat" | "hosts" | "workspaces" | "notices";
const hostStateText: Record<string, string> = { creating: "创建中", running: "运行中", stopping: "停止中", stopped: "已停止", failed: "启动失败", deleting: "删除中" };
const workspaceStateText: Record<string, string> = { ready: "可用", deleting: "删除中" };

export default function MordeApp() {
  const [user, setUser] = useState<{ id: string; name: string } | null>(null);
  const [tab, setTab] = useState<Tab>("chat");
  const [hosts, setHosts] = useState<Host[]>([]);
  const [shared, setShared] = useState<SharedHost[]>([]);
  const [workspaces, setWorkspaces] = useState<Workspace[]>([]);
  const [models, setModels] = useState<Model[]>([]);
  const [conversations, setConversations] = useState<Conversation[]>([]);
  const [notices, setNotices] = useState<Notification[]>([]);
  const [messages, setMessages] = useState<Message[]>([]);
  const [selectedConversation, setSelectedConversation] = useState<string | null>(null);
  const [selectedModel, setSelectedModel] = useState("");
  const [draft, setDraft] = useState("");
  const [busy, setBusy] = useState(false);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [signedOut, setSignedOut] = useState(false);
  const pendingSend = useRef<{ fingerprint: string; key: string } | null>(null);
  const account = useRef<string | null>(null);
  const refreshGeneration = useRef(0);
  const conversationSelectionGeneration = useRef(0);
  const conversation = conversations.find((item) => item.id === selectedConversation);
  const activeModel = selectedConversation ? conversation?.model_id ?? "" : selectedModel;

  const fail = useCallback((reason: unknown) => {
    if (reason instanceof ApiError && reason.status === 401) {
      account.current = null;
      setUser(null); setHosts([]); setShared([]); setWorkspaces([]); setModels([]);
      setConversations([]); setNotices([]); setMessages([]);
      setSelectedConversation(null); setSelectedModel(""); setBusy(false);
      setDraft(""); pendingSend.current = null;
      setSignedOut(true);
    }
    setError(reason instanceof Error ? reason.message : "请求失败，请重试");
  }, []);

  const refresh = useCallback(async () => {
    const generation = ++refreshGeneration.current;
    try {
      const me = await morde.me();
      if (generation !== refreshGeneration.current) return;
      if (account.current && account.current !== me.id) {
        setHosts([]); setShared([]); setWorkspaces([]); setModels([]);
        setConversations([]); setNotices([]); setMessages([]);
        setSelectedConversation(null); setSelectedModel(""); setDraft(""); setBusy(false);
        pendingSend.current = null;
      }
      account.current = me.id;
      setUser(me); setSignedOut(false);
      const [nextHosts, nextShared, nextWorkspaces, nextModels, nextConversations, nextNotices] = await Promise.allSettled([
        morde.hosts(), morde.sharedHosts(), morde.workspaces(), morde.models(), morde.conversations(), morde.notifications(),
      ]);
      if (generation !== refreshGeneration.current || account.current !== me.id) return;
      const failed = [nextHosts, nextShared, nextWorkspaces, nextModels, nextConversations, nextNotices]
        .filter((item) => item.status === "rejected");
      const unauthorized = failed.find((item) => item.reason instanceof ApiError && item.reason.status === 401);
      if (unauthorized) { fail(unauthorized.reason); return; }
      if (nextHosts.status === "fulfilled") setHosts(nextHosts.value);
      if (nextShared.status === "fulfilled") setShared(nextShared.value);
      if (nextWorkspaces.status === "fulfilled") setWorkspaces(nextWorkspaces.value);
      if (nextModels.status === "fulfilled") {
        setModels(nextModels.value);
        setSelectedModel((current) => nextModels.value.some((model) => model.id === current && model.capabilities.text) ? current : (nextModels.value.find((model) => model.capabilities.text)?.id ?? ""));
      }
      if (nextConversations.status === "fulfilled") setConversations(nextConversations.value);
      if (nextNotices.status === "fulfilled") setNotices(nextNotices.value);
      if (failed.length) fail(failed[0].reason);
      else setError("");
    } catch (reason) { if (generation === refreshGeneration.current) fail(reason); }
    finally { if (generation === refreshGeneration.current) setLoading(false); }
  }, [fail]);

  useEffect(() => { void refresh(); }, [refresh]);
  useEffect(() => {
    const onFocus = () => { void refresh(); };
    window.addEventListener("focus", onFocus);
    return () => window.removeEventListener("focus", onFocus);
  }, [refresh]);
  useEffect(() => {
    let cancelled = false;
    const owner = account.current;
    if (!selectedConversation) { setMessages([]); return; }
    void morde.messages(selectedConversation).then((items) => { if (!cancelled && account.current === owner) setMessages(items); }).catch((reason) => { if (!cancelled && account.current === owner) fail(reason); });
    return () => { cancelled = true; };
  }, [selectedConversation, fail]);
  const chooseConversation = (id: string) => {
    conversationSelectionGeneration.current++;
    setSelectedConversation(id); setMessages([]); setDraft(""); pendingSend.current = null;
  };

  const send = async (event: FormEvent) => {
    event.preventDefault();
    const content = draft.trim();
    if (!content || !activeModel || !models.some((model) => model.id === activeModel && model.capabilities.text) || busy) return;
    const owner = account.current;
    const generation = conversationSelectionGeneration.current;
    const fingerprint = JSON.stringify([content, activeModel, selectedConversation]);
    if (pendingSend.current?.fingerprint !== fingerprint) pendingSend.current = { fingerprint, key: newKey() };
    setBusy(true); setError("");
    try {
      const result = await morde.send(activeModel, content, selectedConversation, pendingSend.current.key);
      if (account.current !== owner) return;
      pendingSend.current = null;
      if (conversationSelectionGeneration.current !== generation) { await refresh(); return; }
      setDraft("");
      const nextGeneration = ++conversationSelectionGeneration.current;
      setSelectedConversation(result.conversation.id);
      await refresh();
      const items = await morde.messages(result.conversation.id);
      if (account.current === owner && conversationSelectionGeneration.current === nextGeneration) setMessages(items);
    } catch (reason) { if (account.current === owner) fail(reason); }
    finally { if (account.current === owner) setBusy(false); }
  };

  const openHost = async (id: string, isShared: boolean) => {
    const owner = account.current;
    setError("");
    try {
      const host = (isShared ? shared : hosts).find((item) => item.id === id);
      if (!host) throw new Error("电脑已不可用");
      const ticket = await morde.ticket(id, isShared);
      if (account.current !== owner) return;
      const url = validateHostTicketUrl(ticket.url, host.url, location.href);
      window.location.assign(url.href);
    } catch (reason) { if (account.current === owner) fail(reason); }
  };

  const openNotification = async (notice: Notification) => {
    const owner = account.current;
    try {
      await morde.markRead(notice.id);
      if (account.current !== owner) return;
      await refresh();
    } catch (reason) { if (account.current === owner) fail(reason); }
  };

  const nav: { id: Tab; text: string }[] = [
    { id: "chat", text: "对话" }, { id: "hosts", text: "电脑" },
    { id: "workspaces", text: "项目" }, { id: "notices", text: "通知" },
  ];
  if (loading) return <main className="mx-auto flex h-dvh max-w-xl items-center justify-center px-5" role="status">正在连接 Morde…</main>;
  if (signedOut || !user) return <main className="mx-auto flex h-dvh max-w-xl flex-col items-center justify-center gap-4 px-5 text-center"><h1 className="text-3xl font-bold">Morde</h1><p>请先登录 Morde 用户端。</p><a className="rounded-full bg-ink px-5 py-3 text-ink-fg" href="/login?next=%2Fapp%2F">前往登录</a><button onClick={() => void refresh()}>已登录，重试</button></main>;

  const readyModels = models.filter((model) => model.capabilities.text);
  const effectiveModel = readyModels.some((model) => model.id === activeModel) ? activeModel : "";

  return <div className="morde-mobile mx-auto flex h-dvh max-w-2xl flex-col bg-bg sm:border-x sm:border-border">
    <header className="safe-top flex shrink-0 items-center justify-between border-b border-border px-4 py-3"><div><h1 className="text-xl font-bold">Morde</h1><p className="text-xs text-muted">{user.name} · 云端电脑</p></div><a href="/" className="rounded-full border border-border px-3 py-2 text-sm">PC 用户端</a></header>
    {error && <div role="alert" className="flex items-start justify-between gap-3 bg-rose-50 px-4 py-2 text-sm text-rose-800"><span>{error}</span><button type="button" onClick={() => void refresh()} className="underline">重试</button></div>}
    <main className="min-h-0 flex-1 overflow-y-auto px-4 pb-4">
      {tab === "chat" && <div className="flex min-h-full flex-col gap-3">
        <div className="flex items-center justify-between gap-2 pt-4"><h2 className="text-lg font-semibold">AI 助手</h2><button type="button" disabled={busy} className="rounded-full border border-border px-3 py-1.5 text-sm" onClick={() => { conversationSelectionGeneration.current++; setSelectedConversation(null); setMessages([]); setDraft(""); pendingSend.current = null; }}>新对话</button></div>
        <div className="flex gap-2 overflow-x-auto pb-1" aria-label="最近对话">{conversations.map((item) => <button type="button" key={item.id} disabled={busy} onClick={() => chooseConversation(item.id)} aria-current={item.id === selectedConversation ? "true" : undefined} className={`max-w-40 shrink-0 truncate rounded-full px-3 py-2 text-sm ${item.id === selectedConversation ? "bg-ink text-ink-fg" : "bg-surface-2"}`}>{item.title || "新对话"}</button>)}</div>
        <div className="flex-1 space-y-3" aria-live="polite">{!selectedConversation && <div className="rounded-2xl bg-surface-2 p-5"><p className="font-medium">今天想聊些什么？</p><p className="mt-1 text-sm text-muted">直接与 AI 对话，回复会保存在这里。对话不会操作云端电脑或文件。</p></div>}
          {messages.map((item) => <article key={item.id} className={`max-w-[90%] rounded-2xl p-3 text-sm whitespace-pre-wrap ${item.role === "user" ? "ml-auto bg-ink text-ink-fg" : "bg-surface-2"}`}>{item.content}</article>)}
        </div>
        <form onSubmit={send} className="sticky bottom-0 -mx-4 border-t border-border bg-bg px-4 py-3 safe-bottom"><div className="mb-2"><select aria-label="选择模型" className="w-full rounded-xl bg-surface-2 p-2 text-sm" value={effectiveModel} onChange={(event) => setSelectedModel(event.target.value)} disabled={Boolean(selectedConversation) || busy}><option value="">选择可用模型</option>{readyModels.map((model) => <option key={model.id} value={model.id}>{model.name} · {model.model_name}</option>)}</select></div><div className="flex items-end gap-2"><textarea aria-label="消息内容" value={draft} onChange={(event) => setDraft(event.target.value)} rows={2} maxLength={32768} placeholder="输入你想问的问题…" disabled={busy} className="min-h-12 flex-1 resize-none rounded-2xl border border-border bg-bg p-3" /><button type="submit" disabled={busy || !draft.trim() || !effectiveModel} className="rounded-full bg-ink px-4 py-3 text-ink-fg disabled:opacity-40">发送</button></div>{!readyModels.length && <p className="mt-2 text-xs text-muted">还没有可用模型，请到 PC 用户端配置模型连接。</p>}</form>
      </div>}
      {tab === "hosts" && <section className="space-y-3 pt-4"><h2 className="text-lg font-semibold">云端电脑</h2>{hosts.map((host) => <div key={host.id} className="rounded-2xl border border-border p-4"><div className="flex justify-between gap-3"><strong>{host.name}</strong><span>{hostStateText[host.state] ?? host.state}</span></div><p className="mt-1 text-xs text-muted">{host.workspaces?.map((item) => item.workspace.name).join("、") || "未绑定项目"}</p><button type="button" disabled={host.state !== "running"} className="mt-3 rounded-full bg-ink px-4 py-2 text-sm text-ink-fg disabled:opacity-40" onClick={() => void openHost(host.id, false)}>进入电脑</button></div>)}{shared.map((host) => <div key={host.id} className="rounded-2xl border border-border p-4"><strong>{host.name}</strong><p className="text-sm text-muted">共享电脑 · {hostStateText[host.state] ?? host.state}</p><button type="button" disabled={host.state !== "running"} className="mt-3 rounded-full border border-border px-4 py-2 text-sm disabled:opacity-40" onClick={() => void openHost(host.id, true)}>观看电脑</button></div>)}{!hosts.length && !shared.length && <p className="text-muted">还没有可用电脑，请到 PC 用户端创建。</p>}<div className="rounded-2xl bg-surface-2 p-4 text-sm"><strong>客户端安装</strong><p className="mt-1 text-muted">已登录的电脑与手机在用户端管理。</p><a className="text-accent underline" href="/clients">查看客户端</a></div><div className="rounded-2xl bg-surface-2 p-4 text-sm"><strong>同步设备</strong><p className="mt-1 text-muted">设备进度按真实 Workspace 会话显示。</p><a className="text-accent underline" href="/workspaces">查看项目设备进度</a></div></section>}
      {tab === "workspaces" && <section className="space-y-3 pt-4"><h2 className="text-lg font-semibold">项目</h2>{workspaces.map((item) => <div key={item.id} className="rounded-2xl border border-border p-4"><strong>{item.name}</strong><p className="mt-1 text-sm text-muted">版本 {item.revision} · {workspaceStateText[item.state] ?? item.state}</p></div>)}{!workspaces.length && <p className="text-muted">还没有项目，请到 PC 用户端创建。</p>}</section>}
      {tab === "notices" && <section className="space-y-3 pt-4"><h2 className="text-lg font-semibold">通知</h2>{notices.map((item) => <button type="button" key={item.id} className={`block w-full rounded-2xl border border-border p-4 text-left ${item.read_at ? "opacity-70" : ""}`} onClick={() => void openNotification(item)}><strong>通知</strong><p className="mt-1 text-sm">{item.summary ?? "暂无详情"}</p></button>)}{!notices.length && <p className="text-muted">暂无通知</p>}</section>}
    </main>
    <nav className="safe-bottom flex shrink-0 border-t border-border bg-bg" aria-label="主导航">{nav.map((item) => <button key={item.id} type="button" onClick={() => setTab(item.id)} aria-current={tab === item.id ? "page" : undefined} className={`flex-1 py-3 text-sm ${tab === item.id ? "font-semibold text-accent" : "text-muted"}`}>{item.text}{item.id === "notices" && notices.some((notice) => !notice.read_at) ? " ·" : ""}</button>)}</nav>
  </div>;
}
