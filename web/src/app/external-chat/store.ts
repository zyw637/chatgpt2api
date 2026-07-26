"use client";

import localforage from "localforage";

import {
	compareHistoryTimes,
	historyDocumentsEqual,
	historyTimeAtOrAfter,
	mergeHistoryDocuments,
	normalizeHistoryDeletions,
	resolveHistoryTombstones,
	type HistoryDeletion,
} from "@/lib/history-document";
import { publishHistoryChange, withHistoryStorageLock } from "@/lib/history-sync";

export type ExternalChatRole = "user" | "assistant";
export type ExternalChatMessageStatus = "streaming" | "complete" | "error" | "stopped";

export type StoredExternalChatMessage = {
	id: string;
	role: ExternalChatRole;
	content: string;
	createdAt: string;
	status?: ExternalChatMessageStatus;
	error?: string;
};

export type StoredExternalChatConversation = {
	id: string;
	title: string;
	providerId: string;
	model: string;
	systemPrompt: string;
	temperature: string;
	maxCompletionTokens: string;
	createdAt: string;
	updatedAt: string;
	messages: StoredExternalChatMessage[];
	/** IDs intentionally removed by regenerate/edit actions. */
	messageDeletions?: HistoryDeletion[];
};

export type ExternalChatDeletion = HistoryDeletion;

export type ExternalChatHistoryDocument = {
	items: StoredExternalChatConversation[];
	deletions: ExternalChatDeletion[];
};

const storage = localforage.createInstance({
	name: "chatgpt2api",
	storeName: "external_chat_conversations",
});

const MAX_CONVERSATIONS = 50;
const MAX_MESSAGES = 200;
const MAX_DELETIONS = 500;
const MAX_MESSAGE_DELETIONS = 500;
let externalChatWriteQueue: Promise<void> = Promise.resolve();
export const EXTERNAL_CHAT_CONVERSATIONS_CHANGED_EVENT = "chatgpt2api:external-chat-conversations-changed";

export function limitExternalChatMessages(messages: StoredExternalChatMessage[]) {
	let limited = messages.slice(-MAX_MESSAGES);
	if (limited.length > 1 && limited[0].role === "assistant") {
		limited = limited.slice(1);
	}
	return limited;
}

function normalizeExternalChatMessage(message: StoredExternalChatMessage): StoredExternalChatMessage {
	const status = message.status === "streaming" || message.status === "complete" || message.status === "error" || message.status === "stopped"
		? message.status
		: message.error ? "error" : "complete";
	return {
		...message,
		role: message.role === "assistant" ? "assistant" : "user",
		content: String(message.content || ""),
		createdAt: String(message.createdAt || new Date().toISOString()),
		status,
		error: message.error ? String(message.error) : undefined,
	};
}

function messageStatusScore(message: StoredExternalChatMessage) {
	switch (message.status) {
		case "complete":
			return 4;
		case "error":
			return 3;
		case "stopped":
			return 2;
		case "streaming":
			return 1;
		default:
			return 0;
	}
}

function mergeExternalChatMessages(
	left: StoredExternalChatMessage[],
	right: StoredExternalChatMessage[],
	deletions: HistoryDeletion[],
) {
	const byId = new Map<string, StoredExternalChatMessage>();
	for (const message of [...left, ...right]) {
		const normalized = normalizeExternalChatMessage(message);
		const current = byId.get(normalized.id);
		if (!current) {
			byId.set(normalized.id, normalized);
			continue;
		}
		const currentScore = messageStatusScore(current);
		const nextScore = messageStatusScore(normalized);
		if (
			nextScore > currentScore ||
			(nextScore === currentScore && normalized.content.length >= current.content.length)
		) {
			byId.set(normalized.id, normalized);
		}
	}
	const deletionIDs = new Set(deletions.map((item) => item.id));
	return limitExternalChatMessages(
		[...byId.values()]
			.filter((message) => !deletionIDs.has(message.id))
			.sort((leftMessage, rightMessage) => compareHistoryTimes(leftMessage.createdAt, rightMessage.createdAt)),
	);
}

function mergeExternalChatConversation(
	current: StoredExternalChatConversation,
	candidate: StoredExternalChatConversation,
) {
	const deletions = normalizeHistoryDeletions(
		[...(current.messageDeletions || []), ...(candidate.messageDeletions || [])],
		MAX_MESSAGE_DELETIONS,
	);
	const preferred = historyTimeAtOrAfter(candidate.updatedAt, current.updatedAt) ? candidate : current;
	return {
		...preferred,
		messages: mergeExternalChatMessages(current.messages, candidate.messages, deletions),
		messageDeletions: deletions.length > 0 ? deletions : undefined,
	};
}

export function normalizeExternalChatConversations(items: StoredExternalChatConversation[]) {
	if (!Array.isArray(items)) return [];
	const now = new Date().toISOString();
	return items
		.filter((item) => item && typeof item.id === "string" && item.id.trim())
		.map((item) => ({
			...item,
			id: item.id.trim(),
			title: String(item.title || "新对话"),
			providerId: String(item.providerId || ""),
			model: String(item.model || ""),
			systemPrompt: String(item.systemPrompt || ""),
			temperature: String(item.temperature || ""),
			maxCompletionTokens: String(item.maxCompletionTokens || ""),
			createdAt: String(item.createdAt || now),
			updatedAt: String(item.updatedAt || item.createdAt || now),
			messageDeletions: normalizeHistoryDeletions(item.messageDeletions || [], MAX_MESSAGE_DELETIONS),
			messages: Array.isArray(item.messages)
				? mergeExternalChatMessages(
					item.messages.map(normalizeExternalChatMessage),
					[],
					normalizeHistoryDeletions(item.messageDeletions || [], MAX_MESSAGE_DELETIONS),
				)
				: [],
		}))
		.sort((left, right) => compareHistoryTimes(right.updatedAt, left.updatedAt))
		.slice(0, MAX_CONVERSATIONS);
}

export function mergeExternalChatConversations(
	localItems: StoredExternalChatConversation[],
	remoteItems: StoredExternalChatConversation[],
) {
	return mergeHistoryDocuments(
		{ items: normalizeExternalChatConversations(localItems), deletions: [] },
		{ items: normalizeExternalChatConversations(remoteItems), deletions: [] },
		{
			maxItems: MAX_CONVERSATIONS,
			maxDeletions: MAX_DELETIONS,
			pickItemWinner: mergeExternalChatConversation,
		},
	).items;
}

export function normalizeExternalChatHistoryDocument(document: Partial<ExternalChatHistoryDocument> | null | undefined) {
	const items = normalizeExternalChatConversations(document?.items || []);
	const deletions = normalizeHistoryDeletions(document?.deletions || [], MAX_DELETIONS);
	return resolveHistoryTombstones(items, deletions, MAX_CONVERSATIONS, MAX_DELETIONS);
}

export function mergeExternalChatHistoryDocuments(
	left: Partial<ExternalChatHistoryDocument> | null | undefined,
	right: Partial<ExternalChatHistoryDocument> | null | undefined,
) {
	return mergeHistoryDocuments(
		{
			items: normalizeExternalChatConversations(left?.items || []),
			deletions: left?.deletions || [],
		},
		{
			items: normalizeExternalChatConversations(right?.items || []),
			deletions: right?.deletions || [],
		},
		{
			maxItems: MAX_CONVERSATIONS,
			maxDeletions: MAX_DELETIONS,
			pickItemWinner: mergeExternalChatConversation,
		},
	);
}

export function externalChatHistoryDocumentsEqual(
	left: ExternalChatHistoryDocument,
	right: ExternalChatHistoryDocument,
) {
	return historyDocumentsEqual(left, right, normalizeExternalChatHistoryDocument);
}

export function createExternalChatID(prefix: string) {
	const random = typeof crypto !== "undefined" && "randomUUID" in crypto
		? crypto.randomUUID()
		: `${Date.now()}-${Math.random().toString(16).slice(2)}`;
	return `${prefix}-${random}`;
}

export function createExternalChatConversation(providerId = "", model = ""): StoredExternalChatConversation {
	const now = new Date().toISOString();
	return {
		id: createExternalChatID("chat"),
		title: "新对话",
		providerId,
		model,
		systemPrompt: "",
		temperature: "",
		maxCompletionTokens: "",
		createdAt: now,
		updatedAt: now,
		messages: [],
	};
}

export async function loadExternalChatConversations(scope: string) {
	const document = await storage.getItem<ExternalChatHistoryDocument>(`items:${scope}`);
	return normalizeExternalChatHistoryDocument(document);
}

export async function saveExternalChatConversations(scope: string, document: ExternalChatHistoryDocument) {
	const normalized = normalizeExternalChatHistoryDocument(document);
	const write = () => withHistoryStorageLock(`external-chat:${scope}`, async () => {
		await storage.setItem(`items:${scope}`, normalized);
		publishHistoryChange(EXTERNAL_CHAT_CONVERSATIONS_CHANGED_EVENT);
		return undefined;
	});
	externalChatWriteQueue = externalChatWriteQueue.then(write, write);
	await externalChatWriteQueue;
}
