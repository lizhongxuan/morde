import { createElement } from "react";
import { act, create, type ReactTestRenderer } from "react-test-renderer";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { api } from "../api";
import { LoadError } from "../components/LoadError";
import type { CalendarData, FeedItem, FeedPostsData, UpcomingData } from "../types";
import { FeedScreen } from "./FeedScreen";

const store = vi.hoisted(() => ({
  state: {
    profile: null, status: { state: "idle", detail: "", thread: "main" }, settings: null,
    feedVersion: 0, remindersVersion: 0, calendarVersion: 0, connectionsVersion: 0,
    feedSeenAt: "", pendingApprovals: [], loaded: true, connected: true,
    mishapAt: 0, finishedAt: 0, holds: [], working: {},
  },
  markFeedSeen: () => {}, toast: () => {}, setTab: () => {}, setDrawer: () => {},
  send: async () => {}, openThread: () => {}, openFile: () => {},
}));
vi.mock("../store", () => ({ useStore: () => store }));

const activity: FeedItem[] = [{
  id: "activity", ts: "2026-10-14T08:00:00Z", kind: "background",
  title: "Retained activity", text: "Completed work", thread: "main", thread_title: "Main chat",
}];
const posts: FeedPostsData = {
  instructions: "", daily: false, time: "08:00", generated_at: null,
  posts: [{ id: "post", ts: "2026-10-14T08:00:00Z", title: "Retained post", body: "A useful update", area: "other", prompt: "" }],
};
const upcoming: UpcomingData = {
  proactive: false, proactivity: "off", interval_minutes: 60, effective_interval_minutes: 60,
  quiet_hours: "", quiet_until: null, next_pass_at: null, queue: [], check_ins: [], reminders: [], busy: false,
  triggers: { items: [], available: { mail: false, event: false, hook: true, device: false }, mail_checked_at: null, mail_error: "", mail_poll_minutes: 5 },
};
const calendar: CalendarData = {
  enabled: true, configured: false, today: "2026-10-14", events: [], feeds: [], fetched_at: null, stale: false,
};
const owners = ["feed", "feedPosts", "upcoming", "calendar"] as const;
let screen: ReactTestRenderer;

beforeEach(() => {
  store.state.feedVersion = 0;
  store.state.calendarVersion = 0;
  vi.spyOn(api, "feed").mockResolvedValue(activity);
  vi.spyOn(api, "feedPosts").mockResolvedValue(posts);
  vi.spyOn(api, "upcoming").mockResolvedValue(upcoming);
  vi.spyOn(api, "calendar").mockResolvedValue(calendar);
});

afterEach(() => {
  if (screen) act(() => screen.unmount());
  vi.restoreAllMocks();
});

async function mount() {
  await act(async () => { screen = create(createElement(FeedScreen)); });
}

function failure(message: string) {
  return screen.root.findAllByType(LoadError).find((node) => node.props.message === message);
}

describe("Feed request recovery", () => {
  it.each(owners)("shows a failed %s request and retries only that request", async (owner) => {
    vi.mocked(api[owner]).mockRejectedValueOnce(new Error(`${owner} unavailable`));
    await mount();

    const error = failure(`${owner} unavailable`);
    expect(error, "the failed section must offer a retry").toBeDefined();
    const before = owners.map((name) => vi.mocked(api[name]).mock.calls.length);
    await act(async () => { error!.findByType("button").props.onClick(); });

    expect(failure(`${owner} unavailable`)).toBeUndefined();
    expect(JSON.stringify(screen.toJSON())).toContain("Retained activity");
    expect(JSON.stringify(screen.toJSON())).toContain("Retained post");
    owners.forEach((name, index) => {
      expect(vi.mocked(api[name]).mock.calls.length, name).toBe(before[index] + (name === owner ? 1 : 0));
    });
  });

  it("shows refresh failures alongside the last successful posts and activity", async () => {
    await mount();
    vi.mocked(api.feedPosts).mockRejectedValueOnce(new Error("posts refresh failed"));
    vi.mocked(api.upcoming).mockRejectedValueOnce(new Error("upcoming refresh failed"));
    vi.mocked(api.calendar).mockRejectedValueOnce(new Error("calendar refresh failed"));

    await act(async () => {
      store.state.feedVersion += 1;
      store.state.calendarVersion += 1;
      screen.update(createElement(FeedScreen));
    });

    for (const message of ["posts refresh failed", "upcoming refresh failed", "calendar refresh failed"]) {
      expect(failure(message), message).toBeDefined();
    }
    expect(JSON.stringify(screen.toJSON())).toContain("Retained post");
    expect(JSON.stringify(screen.toJSON())).toContain("Retained activity");
  });

  it.each(["resolve", "reject"] as const)("ignores an older request that finishes with %s after a newer version", async (outcome) => {
    let resolveOld!: (value: FeedPostsData) => void;
    let rejectOld!: (reason: Error) => void;
    vi.mocked(api.feedPosts).mockImplementationOnce(() => new Promise((resolve, reject) => {
      resolveOld = resolve;
      rejectOld = reject;
    }));
    await mount();
    await act(async () => {
      store.state.feedVersion += 1;
      screen.update(createElement(FeedScreen));
    });
    expect(JSON.stringify(screen.toJSON())).toContain("Retained post");

    await act(async () => {
      if (outcome === "resolve") resolveOld({ ...posts, posts: [{ ...posts.posts[0], title: "Obsolete post" }] });
      else rejectOld(new Error("obsolete request failed"));
    });
    expect(JSON.stringify(screen.toJSON())).toContain("Retained post");
    expect(JSON.stringify(screen.toJSON())).not.toContain("Obsolete post");
    expect(failure("obsolete request failed")).toBeUndefined();
  });
});
