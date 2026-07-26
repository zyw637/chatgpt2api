"use client";

import { useEffect, useRef } from "react";

export type HistorySyncAdapters<TDoc> = {
  loadLocal: () => Promise<TDoc>;
  saveLocal: (document: TDoc) => Promise<void>;
  fetchRemote: () => Promise<TDoc>;
  /** Must return the server-merged document. */
  putRemote: (document: TDoc) => Promise<TDoc>;
  merge: (left: TDoc, right: TDoc) => TDoc;
  equal?: (left: TDoc, right: TDoc) => boolean;
  /** Overlay live memory / strip heavy fields before network write. */
  prepareOutgoing?: (document: TDoc) => TDoc;
  prepareIncoming?: (document: TDoc) => TDoc;
};

export type HistorySyncController<TDoc> = {
  pull: () => Promise<TDoc>;
  push: (document?: TDoc) => Promise<TDoc>;
  sync: () => Promise<TDoc>;
  enqueuePush: (document?: TDoc) => Promise<TDoc>;
};

const HISTORY_VISIBLE_SYNC_INTERVAL_MS = 15_000;
const HISTORY_BROADCAST_CHANNEL = "chatgpt2api:history-sync";
const HISTORY_TAB_ID =
  typeof crypto !== "undefined" && "randomUUID" in crypto
    ? crypto.randomUUID()
    : `${Date.now()}-${Math.random().toString(16).slice(2)}`;

type HistoryChangeMessage = {
  topic: string;
  source: string;
};

let historyPublisher: BroadcastChannel | null = null;

export async function withHistoryStorageLock<T>(name: string, operation: () => Promise<T>): Promise<T> {
  if (typeof navigator === "undefined" || !navigator.locks) {
    return operation();
  }
  return navigator.locks.request(`chatgpt2api:history:${name}`, operation);
}

export function publishHistoryChange(topic: string) {
  if (typeof window === "undefined") {
    return;
  }
  window.dispatchEvent(new Event(topic));
  if (typeof BroadcastChannel === "undefined") {
    return;
  }
  historyPublisher ??= new BroadcastChannel(HISTORY_BROADCAST_CHANNEL);
  historyPublisher.postMessage({ topic, source: HISTORY_TAB_ID } satisfies HistoryChangeMessage);
}

export function subscribeHistoryChange(topic: string, listener: () => void) {
  const channel = typeof BroadcastChannel === "undefined" ? null : new BroadcastChannel(HISTORY_BROADCAST_CHANNEL);
  const onMessage = (event: MessageEvent<HistoryChangeMessage>) => {
    if (event.data?.topic === topic && event.data.source !== HISTORY_TAB_ID) listener();
  };
  window.addEventListener(topic, listener);
  channel?.addEventListener("message", onMessage);
  return () => {
    window.removeEventListener(topic, listener);
    channel?.removeEventListener("message", onMessage);
    channel?.close();
  };
}

/**
 * Shared pull/push/sync lifecycle for document-style conversation history.
 * push always re-merges the server response into local storage.
 */
export function createHistorySync<TDoc>(adapters: HistorySyncAdapters<TDoc>): HistorySyncController<TDoc> {
  let pushQueue: Promise<TDoc | undefined> = Promise.resolve(undefined);
  let pendingPush: TDoc | undefined;

  const prepareOut = (document: TDoc) => (adapters.prepareOutgoing ? adapters.prepareOutgoing(document) : document);
  const prepareIn = (document: TDoc) => (adapters.prepareIncoming ? adapters.prepareIncoming(document) : document);

  const pull = async () => {
    const local = prepareOut(await adapters.loadLocal());
    const remote = await adapters.fetchRemote();
    const merged = prepareIn(adapters.merge(local, remote));
    await adapters.saveLocal(merged);
    return merged;
  };

  const push = async (document?: TDoc) => {
    const local = prepareOut(document ?? (await adapters.loadLocal()));
    let saved: TDoc;
    try {
      saved = await adapters.putRemote(local);
    } catch (error) {
      // Keep the local snapshot durable even when remote write fails.
      await adapters.saveLocal(local);
      throw error;
    }
    const resolved = prepareIn(adapters.merge(local, saved));
    await adapters.saveLocal(resolved);
    return resolved;
  };

  const sync = async () => {
    const pulled = await pull();
    try {
      return await push(pulled);
    } catch {
      return pulled;
    }
  };

  const enqueuePush = (document?: TDoc) => {
    if (document !== undefined) {
      pendingPush = document;
    }
    const run = async (): Promise<TDoc> => {
      let last: TDoc | undefined;
      while (pendingPush !== undefined) {
        const next = pendingPush;
        pendingPush = undefined;
        try {
          last = await push(next);
        } catch (error) {
          // A newer full snapshot supersedes the failed one and can still be drained.
          if (pendingPush === undefined) {
            throw error;
          }
        }
      }
      return last ?? prepareIn(await adapters.loadLocal());
    };
    const result = pushQueue.then(run, run);
    pushQueue = result.then(
      () => undefined,
      () => undefined,
    );
    return result;
  };

  return { pull, push, sync, enqueuePush };
}

/**
 * Re-run sync when the window regains focus, the tab becomes visible, the
 * browser comes online, or a visible tab reaches the retry interval.
 * Single-flight + short debounce coalesces overlapping triggers.
 * Overlapping triggers while a resume is in flight are coalesced into one follow-up run.
 */
export function useHistoryResumeSync(onResume: () => void | Promise<void>, enabled = true) {
  const onResumeRef = useRef(onResume);
  onResumeRef.current = onResume;
  const inflightRef = useRef(false);
  const pendingRef = useRef(false);
  const timerRef = useRef<number | null>(null);

  useEffect(() => {
    if (!enabled || typeof window === "undefined") {
      return;
    }

    const execute = () => {
      inflightRef.current = true;
      Promise.resolve(onResumeRef.current())
        .catch(() => undefined)
        .finally(() => {
          inflightRef.current = false;
          if (pendingRef.current) {
            pendingRef.current = false;
            execute();
          }
        });
    };

    const run = () => {
      if (inflightRef.current) {
        pendingRef.current = true;
        return;
      }
      if (timerRef.current != null) {
        window.clearTimeout(timerRef.current);
      }
      timerRef.current = window.setTimeout(() => {
        timerRef.current = null;
        if (inflightRef.current) {
          pendingRef.current = true;
          return;
        }
        execute();
      }, 300);
    };

    const onFocus = () => {
      if (document.visibilityState === "visible") {
        run();
      }
    };
    const onVisibility = () => {
      if (document.visibilityState === "visible") {
        run();
      }
    };

    const interval = window.setInterval(() => {
      if (document.visibilityState === "visible") {
        run();
      }
    }, HISTORY_VISIBLE_SYNC_INTERVAL_MS);
    window.addEventListener("focus", onFocus);
    window.addEventListener("online", run);
    document.addEventListener("visibilitychange", onVisibility);
    return () => {
      window.clearInterval(interval);
      window.removeEventListener("focus", onFocus);
      window.removeEventListener("online", run);
      document.removeEventListener("visibilitychange", onVisibility);
      if (timerRef.current != null) {
        window.clearTimeout(timerRef.current);
      }
      pendingRef.current = false;
    };
  }, [enabled]);
}
