import { describe, expect, it, vi } from "vitest";

const stores = vi.hoisted(() => new Map<string, Map<string, unknown>>());

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

import {
  createExternalChatConversation,
  loadExternalChatConversations,
  saveExternalChatConversations,
} from "./store";

describe("external chat local history writes", () => {
	it("replaces the local cache with the cloud-authoritative snapshot", async () => {
		const scope = `test-${Date.now()}`;
		const first = createExternalChatConversation("provider", "model");
		const second = createExternalChatConversation("provider", "model");

    await saveExternalChatConversations(scope, { items: [first], deletions: [] });
		await saveExternalChatConversations(scope, { items: [second], deletions: [] });

		const stored = await loadExternalChatConversations(scope);
		expect(stored.items.map((item) => item.id)).toEqual([second.id]);
	});
});
