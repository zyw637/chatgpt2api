import { describe, expect, it } from "vitest";

import {
  compareHistoryTimes,
  mergeHistoryDocuments,
  normalizeHistoryDeletions,
  resolveHistoryTombstones,
} from "./history-document";

type Item = { id: string; updatedAt: string; value: string };

describe("history document timestamps", () => {
  it("compares mixed RFC3339 fractional precision by time", () => {
    expect(compareHistoryTimes("2026-07-20T10:00:00.5Z", "2026-07-20T10:00:00Z")).toBeGreaterThan(0);
    expect(compareHistoryTimes("2026-07-20T10:00:00.500Z", "2026-07-20T10:00:00.5Z")).toBe(0);
    expect(compareHistoryTimes("2026-07-20T10:00:00.000Z", "2026-07-20T10:00:00Z")).toBe(0);
  });

  it("keeps a newer fractional item over a whole-second tombstone", () => {
    const result = resolveHistoryTombstones<Item>(
      [{ id: "item", updatedAt: "2026-07-20T10:00:00.5Z", value: "newer" }],
      [{ id: "item", deletedAt: "2026-07-20T10:00:00Z" }],
    );
    expect(result.items.map((item) => item.value)).toEqual(["newer"]);
    expect(result.deletions).toEqual([]);
  });

  it("uses parsed timestamps for deletion and item LWW", () => {
    const deletions = normalizeHistoryDeletions([
      { id: "item", deletedAt: "2026-07-20T10:00:00.5Z" },
      { id: "item", deletedAt: "2026-07-20T10:00:00Z" },
    ]);
    expect(deletions[0].deletedAt).toBe("2026-07-20T10:00:00.5Z");

    const merged = mergeHistoryDocuments<Item>(
      { items: [{ id: "item", updatedAt: "2026-07-20T10:00:00Z", value: "old" }], deletions: [] },
      { items: [{ id: "item", updatedAt: "2026-07-20T10:00:00.5Z", value: "new" }], deletions: [] },
    );
    expect(merged.items[0].value).toBe("new");
  });
});
