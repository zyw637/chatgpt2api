"use client";

export type HistoryDeletion = {
  id: string;
  deletedAt: string;
};

export type HistoryItemBase = {
  id: string;
  updatedAt: string;
};

export type HistoryDocument<T extends HistoryItemBase> = {
  items: T[];
  deletions: HistoryDeletion[];
};

export const DEFAULT_HISTORY_MAX_ITEMS = 50;
export const DEFAULT_HISTORY_MAX_DELETIONS = 500;

export type MergeHistoryOptions<T extends HistoryItemBase> = {
  maxItems?: number;
  maxDeletions?: number;
  /** When both sides have the same id, pick the winner. Default: LWW by updatedAt (>=). */
  pickItemWinner?: (current: T, candidate: T) => T;
};

function nowISO() {
  return new Date().toISOString();
}

export function historyTimestamp(value: string): number {
  const timestamp = Date.parse(String(value || ""));
  return Number.isFinite(timestamp) ? timestamp : 0;
}

export function compareHistoryTimes(left: string, right: string): number {
  return historyTimestamp(left) - historyTimestamp(right);
}

export function historyTimeAtOrAfter(left: string, right: string): boolean {
  return compareHistoryTimes(left, right) >= 0;
}

export function normalizeHistoryDeletions(
  items: HistoryDeletion[] | null | undefined,
  maxDeletions = DEFAULT_HISTORY_MAX_DELETIONS,
): HistoryDeletion[] {
  if (!Array.isArray(items)) return [];
  const normalized = new Map<string, HistoryDeletion>();
  for (const item of items) {
    const id = String(item?.id || "").trim();
    if (!id) continue;
    const deletedAt = String(item.deletedAt || nowISO());
    const existing = normalized.get(id);
    if (!existing || historyTimeAtOrAfter(deletedAt, existing.deletedAt)) {
      normalized.set(id, { id, deletedAt });
    }
  }
  return [...normalized.values()]
    .sort((left, right) => compareHistoryTimes(right.deletedAt, left.deletedAt))
    .slice(0, maxDeletions);
}

export function mergeHistoryItemsByUpdatedAt<T extends HistoryItemBase>(
  left: T[],
  right: T[],
  pickItemWinner?: (current: T, candidate: T) => T,
): T[] {
  const pick =
    pickItemWinner ||
    ((current: T, candidate: T) => (historyTimeAtOrAfter(candidate.updatedAt, current.updatedAt) ? candidate : current));
  const merged = new Map<string, T>();
  for (const item of [...left, ...right]) {
    const id = String(item?.id || "").trim();
    if (!id) continue;
    const existing = merged.get(id);
    merged.set(id, existing ? pick(existing, item) : item);
  }
  return [...merged.values()];
}

/**
 * Apply tombstones: drop items when deletedAt >= updatedAt; drop tombstones when item is newer.
 */
export function resolveHistoryTombstones<T extends HistoryItemBase>(
  items: T[],
  deletions: HistoryDeletion[],
  maxItems = DEFAULT_HISTORY_MAX_ITEMS,
  maxDeletions = DEFAULT_HISTORY_MAX_DELETIONS,
): HistoryDocument<T> {
  const deletionMap = new Map(normalizeHistoryDeletions(deletions, maxDeletions).map((item) => [item.id, item]));
  const resolvedItems: T[] = [];
  for (const item of items) {
    const id = String(item?.id || "").trim();
    if (!id) continue;
    const deletion = deletionMap.get(id);
    if (deletion && historyTimeAtOrAfter(deletion.deletedAt, item.updatedAt)) {
      continue;
    }
    if (deletion && !historyTimeAtOrAfter(deletion.deletedAt, item.updatedAt)) {
      deletionMap.delete(id);
    }
    resolvedItems.push(item);
  }
  return {
    items: resolvedItems
      .sort((left, right) => compareHistoryTimes(right.updatedAt, left.updatedAt))
      .slice(0, maxItems),
    deletions: [...deletionMap.values()]
      .sort((left, right) => compareHistoryTimes(right.deletedAt, left.deletedAt))
      .slice(0, maxDeletions),
  };
}

export function mergeHistoryDocuments<T extends HistoryItemBase>(
  left: Partial<HistoryDocument<T>> | null | undefined,
  right: Partial<HistoryDocument<T>> | null | undefined,
  options: MergeHistoryOptions<T> = {},
): HistoryDocument<T> {
  const maxItems = options.maxItems ?? DEFAULT_HISTORY_MAX_ITEMS;
  const maxDeletions = options.maxDeletions ?? DEFAULT_HISTORY_MAX_DELETIONS;
  const items = mergeHistoryItemsByUpdatedAt(left?.items || [], right?.items || [], options.pickItemWinner);
  const deletions = normalizeHistoryDeletions([...(left?.deletions || []), ...(right?.deletions || [])], maxDeletions);
  return resolveHistoryTombstones(items, deletions, maxItems, maxDeletions);
}

export function applyHistoryDeletion<T extends HistoryItemBase>(
  document: HistoryDocument<T>,
  id: string,
  deletedAt = nowISO(),
  options: { maxItems?: number; maxDeletions?: number } = {},
): HistoryDocument<T> {
  const maxItems = options.maxItems ?? DEFAULT_HISTORY_MAX_ITEMS;
  const maxDeletions = options.maxDeletions ?? DEFAULT_HISTORY_MAX_DELETIONS;
  const target = String(id || "").trim();
  return resolveHistoryTombstones(
    (document.items || []).filter((item) => item.id !== target),
    normalizeHistoryDeletions(
      [...(document.deletions || []), { id: target, deletedAt }],
      maxDeletions,
    ),
    maxItems,
    maxDeletions,
  );
}

export function clearHistoryWithTombstones<T extends HistoryItemBase>(
  document: HistoryDocument<T>,
  deletedAt = nowISO(),
  options: { maxItems?: number; maxDeletions?: number } = {},
): HistoryDocument<T> {
  const maxItems = options.maxItems ?? DEFAULT_HISTORY_MAX_ITEMS;
  const maxDeletions = options.maxDeletions ?? DEFAULT_HISTORY_MAX_DELETIONS;
  return resolveHistoryTombstones(
    [],
    normalizeHistoryDeletions(
      [
        ...(document.deletions || []),
        ...(document.items || []).map((item) => ({ id: item.id, deletedAt })),
      ],
      maxDeletions,
    ),
    maxItems,
    maxDeletions,
  );
}

export function historyDocumentsEqual<T extends HistoryItemBase>(
  left: HistoryDocument<T>,
  right: HistoryDocument<T>,
  normalize?: (document: HistoryDocument<T>) => HistoryDocument<T>,
): boolean {
  const resolve = normalize || ((document: HistoryDocument<T>) => document);
  return JSON.stringify(resolve(left)) === JSON.stringify(resolve(right));
}
