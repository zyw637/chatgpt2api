import { describe, expect, it, vi } from "vitest";

const stores = vi.hoisted(() => new Map<string, Map<string, unknown>>());
const mocks = vi.hoisted(() => ({ request: vi.fn() }));

vi.mock("@/constants/common-env", () => ({
  default: { apiUrl: "", appVersion: "test" },
}));
vi.mock("localforage", () => ({
  default: {
    createInstance: ({ storeName }: { storeName: string }) => {
      const store = stores.get(storeName) || new Map<string, unknown>();
      stores.set(storeName, store);
      return {
        getItem: async <T>(key: string) => (store.get(key) as T | undefined) ?? null,
        setItem: async <T>(key: string, value: T) => {
          store.set(key, value);
          return value;
        },
      };
    },
  },
}));
vi.mock("@/store/auth", () => ({
  getStoredAuthSession: async () => null,
}));
vi.mock("@/lib/request", () => ({
  httpRequest: mocks.request,
}));

import {
  listImageConversations,
  mergeImageConversationSnapshots,
  preserveInFlightConversations,
  pullImageConversationsRemote,
  saveImageConversations,
  type ImageConversation,
} from "./image-conversations";

function conversation(updatedAt: string, status: "queued" | "generating" | "success", imageID: string): ImageConversation {
  return {
    id: "conversation-1",
    title: "test",
    createdAt: "2026-07-20T10:00:00.000Z",
    updatedAt,
    turns: [
      {
        id: "turn-1",
        prompt: "regenerate",
        model: "gpt-image-2",
        mode: "generate",
        referenceImages: [],
        count: 1,
        size: "1024x1024",
        images: [
          {
            id: imageID,
            status: status === "success" ? "success" : "loading",
            taskStatus: status === "success" ? "success" : "queued",
          },
        ],
        createdAt: "2026-07-20T10:00:00.000Z",
        updatedAt,
        status,
      },
    ],
  };
}

describe("image conversation history merge", () => {
  it("accepts a newer regeneration even when it resets a turn to queued", () => {
    const merged = mergeImageConversationSnapshots(
      conversation("2026-07-20T10:00:00.000Z", "success", "old-image"),
      conversation("2026-07-20T10:01:00.000Z", "queued", "new-image"),
    );

    expect(merged.turns[0].status).toBe("queued");
    expect(merged.turns[0].images[0].id).toBe("new-image");
  });

  it("keeps a newer completed snapshot over an older in-flight snapshot", () => {
    const merged = mergeImageConversationSnapshots(
      conversation("2026-07-20T10:01:00.000Z", "success", "new-image"),
      conversation("2026-07-20T10:00:00.000Z", "generating", "old-image"),
    );

    expect(merged.turns[0].status).toBe("success");
    expect(merged.turns[0].images[0].id).toBe("new-image");
  });

  it("does not let a newer conversation carry a stale copy over a completed turn", () => {
    const completed = conversation("2026-07-20T10:02:00.000Z", "success", "finished-image");
    const staleWithNewTurn = conversation("2026-07-20T10:03:00.000Z", "generating", "stale-image");
    staleWithNewTurn.turns[0].updatedAt = "2026-07-20T10:01:00.000Z";
    staleWithNewTurn.turns.push({
      ...staleWithNewTurn.turns[0],
      id: "turn-2",
      prompt: "new work",
      images: [{ id: "new-image", status: "loading", taskStatus: "queued" }],
      createdAt: "2026-07-20T10:03:00.000Z",
      updatedAt: "2026-07-20T10:03:00.000Z",
      status: "queued",
    });

    const merged = mergeImageConversationSnapshots(completed, staleWithNewTurn);

    expect(merged.turns).toHaveLength(2);
    expect(merged.turns[0].status).toBe("success");
    expect(merged.turns[0].images[0].id).toBe("finished-image");
    expect(merged.turns[1].id).toBe("turn-2");
  });

  it("does not restore a terminal local conversation missing from the cloud", () => {
    const local = conversation("2026-07-20T10:01:00.000Z", "success", "local-image");

    expect(preserveInFlightConversations([], [local])).toEqual([]);
  });

  it("temporarily preserves local in-flight work missing from the cloud", () => {
    const local = conversation("2026-07-20T10:01:00.000Z", "generating", "pending-image");

    expect(preserveInFlightConversations([], [local])).toEqual([local]);
  });

  it("replaces the local cache with a successful cloud pull", async () => {
    const local = conversation("2026-07-20T10:01:00.000Z", "success", "local-image");
    const remote = conversation("2026-07-20T10:02:00.000Z", "success", "cloud-image");
    mocks.request.mockResolvedValueOnce({ items: [remote], deletions: [] });

    await saveImageConversations([local]);
    await pullImageConversationsRemote();

    const stored = await listImageConversations();
    expect(stored.map((item) => item.turns[0].images[0].id)).toEqual(["cloud-image"]);
  });
});
