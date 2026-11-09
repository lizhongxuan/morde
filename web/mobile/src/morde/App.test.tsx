import { act, create, type ReactTestRenderer } from "react-test-renderer";
import { afterEach, expect, it, vi } from "vitest";
import MordeApp from "./App";
import { morde } from "./api";

afterEach(() => { vi.restoreAllMocks(); vi.unstubAllGlobals(); });

function mockCommon() {
  vi.stubGlobal("crypto", { randomUUID: () => "send-key" });
  vi.stubGlobal("window", { addEventListener() {}, removeEventListener() {} });
  vi.spyOn(morde, "me").mockResolvedValue({ id: "user-1", name: "测试账号" });
  vi.spyOn(morde, "hosts").mockResolvedValue([]);
  vi.spyOn(morde, "sharedHosts").mockResolvedValue([]);
  vi.spyOn(morde, "workspaces").mockResolvedValue([]);
  vi.spyOn(morde, "models").mockResolvedValue([{ id: "model-1", name: "模型", model_name: "model-1", capabilities: { text: true, tools: false, vision: false } }]);
  vi.spyOn(morde, "conversations").mockResolvedValue([]);
  vi.spyOn(morde, "notifications").mockResolvedValue([]);
  vi.spyOn(morde, "messages").mockResolvedValue([]);
}

it("sends an instant message and displays the reply when the computer service is unavailable", async () => {
  mockCommon();
  vi.spyOn(morde, "hosts").mockRejectedValue(new Error("电脑服务暂不可用"));
  const conversation = { id: "c1111111", title: "对话", model_id: "model-1", updated_at: "2026-10-27T00:00:00Z" };
  const exchange = { conversation, message: { id: "m1", role: "user" as const, content: "你好" }, reply: { id: "m2", role: "assistant" as const, content: "你好！" } };
  const send = vi.spyOn(morde, "send").mockResolvedValue(exchange);
  vi.spyOn(morde, "messages").mockResolvedValue([exchange.message, exchange.reply]);
  let renderer!: ReactTestRenderer;
  await act(async () => { renderer = create(<MordeApp />); await new Promise((resolve) => setTimeout(resolve, 0)); });
  await act(async () => { renderer.root.findByType("textarea").props.onChange({ target: { value: "你好" } }); });
  await act(async () => { await renderer.root.findByType("form").props.onSubmit({ preventDefault() {} }); });
  expect(send).toHaveBeenCalledWith("model-1", "你好", null, "send-key");
  expect(renderer.root.findAllByType("article").map((item) => item.children.join(""))).toContain("你好！");
  expect(renderer.root.findAllByType("button").some((item) => item.children.join("") === "新任务")).toBe(false);
  renderer.unmount();
});

it("uses the existing conversation's model and never offers task actions", async () => {
  mockCommon();
  const conversation = { id: "c1111111", title: "已有对话", model_id: "model-2", updated_at: "2026-10-27T00:00:00Z" };
  vi.spyOn(morde, "models").mockResolvedValue([
    { id: "model-1", name: "模型一", model_name: "model-1", capabilities: { text: true, tools: false, vision: false } },
    { id: "model-2", name: "模型二", model_name: "model-2", capabilities: { text: true, tools: false, vision: false } },
  ]);
  vi.spyOn(morde, "conversations").mockResolvedValue([conversation]);
  const send = vi.spyOn(morde, "send").mockResolvedValue({
    conversation, message: { id: "m1", role: "user", content: "继续聊" }, reply: { id: "m2", role: "assistant", content: "好的" },
  });
  let renderer!: ReactTestRenderer;
  await act(async () => { renderer = create(<MordeApp />); await new Promise((resolve) => setTimeout(resolve, 0)); });
  await act(async () => { renderer.root.findAllByType("button").find((button) => button.children.join("") === "已有对话")?.props.onClick(); });
  expect(renderer.root.findByType("select").props.value).toBe("model-2");
  expect(renderer.root.findByType("select").props.disabled).toBe(true);
  await act(async () => { renderer.root.findByType("textarea").props.onChange({ target: { value: "继续聊" } }); });
  await act(async () => { await renderer.root.findByType("form").props.onSubmit({ preventDefault() {} }); });
  expect(send).toHaveBeenCalledWith("model-2", "继续聊", "c1111111", "send-key");
  expect(JSON.stringify(renderer.toJSON())).not.toMatch(/任务|批准|定时计划/);
  renderer.unmount();
});

it("clears the previous account's computer list while the next account loads", async () => {
  let focus: (() => void) | undefined;
  vi.stubGlobal("window", {
    addEventListener: (name: string, listener: () => void) => { if (name === "focus") focus = listener; },
    removeEventListener: () => {},
  });
  const oldHost = { id: "host-a", name: "旧账号电脑", state: "running", owner_id: "user-a", url: "https://a.test", workspaces: [] };
  let resolveHosts!: (hosts: typeof oldHost[]) => void;
  const nextHosts = new Promise<typeof oldHost[]>((resolve) => { resolveHosts = resolve; });
  vi.spyOn(morde, "me").mockResolvedValueOnce({ id: "user-a", name: "账号 A" }).mockResolvedValueOnce({ id: "user-b", name: "账号 B" });
  vi.spyOn(morde, "hosts").mockResolvedValueOnce([oldHost]).mockReturnValueOnce(nextHosts);
  vi.spyOn(morde, "sharedHosts").mockResolvedValue([]);
  vi.spyOn(morde, "workspaces").mockResolvedValue([]);
  vi.spyOn(morde, "models").mockResolvedValue([]);
  vi.spyOn(morde, "conversations").mockResolvedValue([]);
  vi.spyOn(morde, "notifications").mockResolvedValue([]);

  let renderer!: ReactTestRenderer;
  await act(async () => { renderer = create(<MordeApp />); await new Promise((resolve) => setTimeout(resolve, 0)); });
  await act(async () => { renderer.root.findAllByType("button").find((button) => button.children.join("") === "电脑")?.props.onClick(); });
  expect(renderer.root.findAllByType("strong").some((item) => item.children.join("") === "旧账号电脑")).toBe(true);
  await act(async () => { focus?.(); await vi.waitFor(() => expect(renderer.root.findAllByType("p").some((item) => item.children.join("").includes("账号 B"))).toBe(true)); });
  expect(renderer.root.findAllByType("strong").some((item) => item.children.join("") === "旧账号电脑")).toBe(false);
  await act(async () => { resolveHosts([]); await nextHosts; });
  renderer.unmount();
});
