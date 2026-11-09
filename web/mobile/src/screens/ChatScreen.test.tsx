import { act } from "react";
import { create, type ReactTestRenderer } from "react-test-renderer";
import { beforeAll, describe, expect, it, vi } from "vitest";
import { setLocaleSetting } from "../i18n";
import { Composer, WorkingLine } from "./ChatScreen";

(globalThis as Record<string, unknown>).IS_REACT_ACT_ENVIRONMENT = true;
// the composer's only window use is the quick-chat focus listener
(globalThis as Record<string, unknown>).window = {
  addEventListener: () => undefined,
  removeEventListener: () => undefined,
};

vi.mock("../store", () => ({
  useStore: () => ({
    state: { draft: null, draftFiles: null, settings: { skills: { enabled: false } }, hub: null },
    draft: () => undefined,
    draftFiles: () => undefined,
    toast: () => undefined,
  }),
}));

function discs(r: ReactTestRenderer) {
  return {
    stop: r.root.findAllByProps({ "aria-label": "Stop" })[0],
    send: r.root.findAllByProps({ "aria-label": "Send" })[0],
  };
}

describe("Composer stop disc", () => {
  // the labels looked for are the English ones, whatever the machine running the suite speaks
  beforeAll(() => setLocaleSetting("en"));

  it("swaps the send disc for a stop disc while busy with an empty box", () => {
    let r!: ReactTestRenderer;
    act(() => {
      r = create(<Composer name="Muse" busy waiting={false} onSend={vi.fn(async () => undefined)} onStop={vi.fn(async () => undefined)} />);
    });
    const d = discs(r);
    expect(d.stop).toBeDefined();
    expect(d.send).toBeUndefined();
  });

  it("keeps no stop disc without an onStop", () => {
    let r!: ReactTestRenderer;
    act(() => {
      r = create(<Composer name="Muse" busy waiting={false} onSend={vi.fn(async () => undefined)} />);
    });
    const d = discs(r);
    expect(d.stop).toBeUndefined();
    expect(d.send).toBeDefined();
    expect(d.send?.props.disabled).toBe(true);
  });

  it("brings the send disc back once there is text", async () => {
    let r!: ReactTestRenderer;
    act(() => {
      r = create(<Composer name="Muse" busy waiting={false} onSend={vi.fn(async () => undefined)} onStop={vi.fn(async () => undefined)} />);
    });
    const box = r.root.findByProps({ "aria-label": "Message" });
    await act(async () => {
      box.props.onChange({ target: { value: "接着来" } });
    });
    const d = discs(r);
    expect(d.stop).toBeUndefined();
    expect(d.send).toBeDefined();
    expect(d.send?.props.disabled).toBe(false);
  });

  it("calls onStop once and holds the disc while the stop is under way", async () => {
    const onStop = vi.fn(() => new Promise<void>(() => undefined));
    let r!: ReactTestRenderer;
    act(() => {
      r = create(<Composer name="Muse" busy waiting={false} onSend={vi.fn(async () => undefined)} onStop={onStop} />);
    });
    const disc = discs(r).stop;
    await act(async () => {
      disc.props.onClick();
    });
    expect(onStop).toHaveBeenCalledTimes(1);
    expect(discs(r).stop?.props.disabled).toBe(true);
  });
});

// The presence frame from another device says what its run is doing — the line
// under the message reads the step, and a run parked on a card reads as waiting.

function line(who: Parameters<typeof WorkingLine>[0]["who"]) {
  let r!: ReactTestRenderer;
  act(() => {
    r = create(<WorkingLine who={who} />);
  });
  return r.root.findByType("div").props.children[1].props.children;
}

describe("WorkingLine", () => {
  beforeAll(() => setLocaleSetting("en"));

  it("reads as plain working without a state", () => {
    expect(line({ thread: "main", cid: "c", device: "phone-1", device_name: "Pixel 8", at: 1 })).toBe(
      "Pixel 8 is working…",
    );
  });

  it("reads the step the run reported", () => {
    expect(
      line({ thread: "main", cid: "c", device: "phone-1", device_name: "Pixel 8", state: "working", detail: "打开携程网站", at: 1 }),
    ).toBe("Pixel 8 is working: 打开携程网站");
  });

  it("reads as waiting when the run is parked on a card", () => {
    expect(
      line({ thread: "main", cid: "c", device: "phone-1", device_name: "", state: "waiting", at: 1 }),
    ).toBe("phone-1 is waiting for you");
  });
});
