import { describe, expect, it, vi } from "vitest";

import { createHistorySync } from "./history-sync";

type TestDocument = { value: number };

describe("createHistorySync", () => {
  it("drains one queued snapshot with exactly one remote write", async () => {
    let local: TestDocument = { value: 0 };
    const putRemote = vi.fn(async (document: TestDocument) => document);
    const controller = createHistorySync<TestDocument>({
      loadLocal: async () => local,
      saveLocal: async (document) => {
        local = document;
      },
      fetchRemote: async () => ({ value: 0 }),
      putRemote,
      merge: (_left, right) => right,
    });

    await controller.enqueuePush({ value: 1 });

    expect(putRemote).toHaveBeenCalledTimes(1);
    expect(local).toEqual({ value: 1 });
  });

  it("persists a failed remote snapshot locally for a later retry", async () => {
    let local: TestDocument = { value: 0 };
    const controller = createHistorySync<TestDocument>({
      loadLocal: async () => local,
      saveLocal: async (document) => {
        local = document;
      },
      fetchRemote: async () => ({ value: 0 }),
      putRemote: async () => {
        throw new Error("offline");
      },
      merge: (_left, right) => right,
    });

    await expect(controller.enqueuePush({ value: 2 })).rejects.toThrow("offline");
    expect(local).toEqual({ value: 2 });
  });

  it("drains a newer snapshot queued while a remote write is in flight", async () => {
    let local: TestDocument = { value: 0 };
    let releaseFirst!: () => void;
    const firstWrite = new Promise<void>((resolve) => {
      releaseFirst = resolve;
    });
    const putRemote = vi.fn(async (document: TestDocument) => {
      if (document.value === 1) await firstWrite;
      return document;
    });
    const controller = createHistorySync<TestDocument>({
      loadLocal: async () => local,
      saveLocal: async (document) => {
        local = document;
      },
      fetchRemote: async () => ({ value: 0 }),
      putRemote,
      merge: (_left, right) => right,
    });

    const first = controller.enqueuePush({ value: 1 });
    await vi.waitFor(() => expect(putRemote).toHaveBeenCalledTimes(1));
    const second = controller.enqueuePush({ value: 2 });
    releaseFirst();
    await Promise.all([first, second]);

    expect(putRemote.mock.calls.map(([document]) => document.value)).toEqual([1, 2]);
    expect(local).toEqual({ value: 2 });
  });
});
