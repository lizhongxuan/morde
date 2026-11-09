import { act } from "react";
import { create, type ReactTestRenderer } from "react-test-renderer";
import { beforeAll, describe, expect, it, vi } from "vitest";
import { setLocaleSetting } from "../i18n";
import { NamingCard } from "./FirstConversation";

(globalThis as Record<string, unknown>).IS_REACT_ACT_ENVIRONMENT = true;

// The chooser must not depend on the model repeating a typed name in a `nanomuse-naming`
// block: "Something else" opens an input on the card and confirming goes straight to
// onPick, the same path a chip takes.

function byText(r: ReactTestRenderer, text: string) {
  return r.root
    .findAllByProps({ type: "button" })
    .concat(r.root.findAllByProps({ type: "submit" }))
    .find((b) => {
      const c = b.props.children;
      return typeof c === "string" ? c === text : Array.isArray(c) && c.includes(text);
    });
}

describe("NamingCard", () => {
  // the labels looked for are the English ones, whatever the machine running the suite speaks
  beforeAll(() => setLocaleSetting("en"));

  it("picks a chip straight away", () => {
    const onPick = vi.fn();
    let r!: ReactTestRenderer;
    act(() => {
      r = create(<NamingCard chips={["豆苗", "小满"]} chosen={null} onPick={onPick} />);
    });
    act(() => {
      byText(r, "小满")?.props.onClick();
    });
    expect(onPick).toHaveBeenCalledWith("小满");
  });

  it("takes a typed name, trimmed", async () => {
    const onPick = vi.fn();
    let r!: ReactTestRenderer;
    act(() => {
      r = create(<NamingCard chips={["豆苗", "小满"]} chosen={null} onPick={onPick} />);
    });
    act(() => {
      byText(r, "Something else")?.props.onClick();
    });
    const input = r.root.findByProps({ placeholder: "Type a name for me" });
    await act(async () => {
      input.props.onChange({ target: { value: "  小助理  " } });
    });
    await act(async () => {
      r.root.findByType("form").props.onSubmit({ preventDefault: () => undefined });
    });
    expect(onPick).toHaveBeenCalledWith("小助理");
  });

  it("does not submit an empty name", async () => {
    const onPick = vi.fn();
    let r!: ReactTestRenderer;
    act(() => {
      r = create(<NamingCard chips={["豆苗"]} chosen={null} onPick={onPick} />);
    });
    act(() => {
      byText(r, "Something else")?.props.onClick();
    });
    const input = r.root.findByProps({ placeholder: "Type a name for me" });
    await act(async () => {
      input.props.onChange({ target: { value: "   " } });
    });
    expect(byText(r, "Call me this")?.props.disabled).toBe(true);
    await act(async () => {
      r.root.findByType("form").props.onSubmit({ preventDefault: () => undefined });
    });
    expect(onPick).not.toHaveBeenCalled();
  });

  it("Escape returns to the chips", async () => {
    const onPick = vi.fn();
    let r!: ReactTestRenderer;
    act(() => {
      r = create(<NamingCard chips={["豆苗"]} chosen={null} onPick={onPick} />);
    });
    act(() => {
      byText(r, "Something else")?.props.onClick();
    });
    const input = r.root.findByProps({ placeholder: "Type a name for me" });
    await act(async () => {
      input.props.onKeyDown({ key: "Escape" });
    });
    expect(() => r.root.findByProps({ placeholder: "Type a name for me" })).toThrow();
    expect(byText(r, "Something else")).toBeDefined();
  });
});
