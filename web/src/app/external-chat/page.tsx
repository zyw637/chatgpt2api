"use client";

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
	ArrowUp,
	Bot,
	Check,
	Copy,
	MessageCircle,
	MessageSquarePlus,
	RotateCcw,
	Server,
	SlidersHorizontal,
	Square,
	Trash2,
	UserRound,
} from "lucide-react";
import { toast } from "sonner";

import { ApiLoadingMark } from "@/components/api-loading-mark";
import { ConversationEmptyState } from "@/app/image/components/conversation-empty-state";
import { ConversationComposerSurface, ConversationComposerToolbar } from "@/app/image/components/conversation-composer-surface";

import {
	createExternalChatConversation,
	createExternalChatID,
	limitExternalChatMessages,
	loadExternalChatConversations,
	externalChatHistoryDocumentsEqual,
	mergeExternalChatHistoryDocuments,
	normalizeExternalChatHistoryDocument,
	saveExternalChatConversations,
	EXTERNAL_CHAT_CONVERSATIONS_CHANGED_EVENT,
	type ExternalChatDeletion,
	type ExternalChatHistoryDocument,
	type StoredExternalChatConversation,
	type StoredExternalChatMessage,
} from "@/app/external-chat/store";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Textarea } from "@/components/ui/textarea";
import webConfig from "@/constants/common-env";
import { compareHistoryTimes, historyTimeAtOrAfter } from "@/lib/history-document";
import {
	fetchExternalChatConversations,
	fetchExternalChatProviders,
	saveExternalChatConversationsRemote,
	} from "@/lib/external-api";
import {
	type ExternalChatProvider,
} from "@/lib/api";
import { subscribeHistoryChange, useHistoryResumeSync } from "@/lib/history-sync";
import { useAuthGuard } from "@/lib/use-auth-guard";
import { cn } from "@/lib/utils";

type ChatRequestMessage = {
	role: "system" | "user" | "assistant";
	content: string;
};

type ChatSettingsDraft = {
	systemPrompt: string;
	temperature: string;
	maxCompletionTokens: string;
};

const MAX_CHAT_REQUEST_MESSAGES = 200;

function messagesForChatRequest(messages: StoredExternalChatMessage[], hasSystemPrompt: boolean) {
	const limit = MAX_CHAT_REQUEST_MESSAGES - (hasSystemPrompt ? 1 : 0);
	let selected = messages
		.filter((message) => !message.error && message.status !== "streaming" && message.content.trim())
		.slice(-limit);
	while (selected.length > 1 && selected[0].role === "assistant") {
		selected = selected.slice(1);
	}
	return selected;
}

function LoadingState() {
	return <div className="flex min-h-[60vh] items-center justify-center"><ApiLoadingMark size="page" label="正在连接聊天接口" /></div>;
}

function errorMessageFromPayload(payload: unknown): string {
	if (typeof payload === "string") return payload;
	if (!payload || typeof payload !== "object") return "";
	const item = payload as { detail?: unknown; error?: unknown; message?: unknown };
	if (typeof item.message === "string") return item.message;
	return errorMessageFromPayload(item.detail) || errorMessageFromPayload(item.error);
}

function deltaTextFromChunk(chunk: unknown) {
	if (!chunk || typeof chunk !== "object") return "";
	const payload = chunk as {
		error?: unknown;
		choices?: Array<{ delta?: { content?: unknown }; message?: { content?: unknown } }>;
	};
	if (payload.error) throw new Error(errorMessageFromPayload(payload.error) || "上游返回流式错误");
	const content = payload.choices?.[0]?.delta?.content ?? payload.choices?.[0]?.message?.content;
	if (typeof content === "string") return content;
	if (Array.isArray(content)) {
		return content.map((part) => {
			if (typeof part === "string") return part;
			if (part && typeof part === "object" && "text" in part && typeof part.text === "string") return part.text;
			return "";
		}).join("");
	}
	return "";
}

function chunkHasFinishReason(chunk: unknown) {
	if (!chunk || typeof chunk !== "object") return false;
	const payload = chunk as { choices?: Array<{ finish_reason?: unknown }> };
	const finishReason = payload.choices?.[0]?.finish_reason;
	return finishReason !== undefined && finishReason !== null;
}

async function streamExternalChat(
	payload: {
		provider_id: string;
		model: string;
		messages: ChatRequestMessage[];
		temperature?: number;
		max_completion_tokens?: number;
	},
	signal: AbortSignal,
	onDelta: (delta: string) => void,
) {
	const response = await fetch(`${webConfig.apiUrl.replace(/\/$/, "")}/api/external-chat/completions`, {
		method: "POST",
		credentials: "include",
		headers: {
			"Content-Type": "application/json",
			Accept: "text/event-stream",
		},
		body: JSON.stringify(payload),
		signal,
	});
	if (!response.ok) {
		const text = await response.text();
		let decoded: unknown = text;
		try { decoded = JSON.parse(text); } catch { /* Keep the response text. */ }
		throw new Error(errorMessageFromPayload(decoded) || `请求失败 (${response.status})`);
	}
	if (response.headers.get("Content-Type")?.includes("application/json")) {
		const delta = deltaTextFromChunk(await response.json());
		if (delta) onDelta(delta);
		return;
	}
	if (!response.body) throw new Error("浏览器未提供流式响应");

	const reader = response.body.getReader();
	const decoder = new TextDecoder();
	let buffer = "";
	let streamCompleted = false;
	const consumeLine = (line: string) => {
		const trimmed = line.trim();
		if (!trimmed.startsWith("data:")) return false;
		const data = trimmed.slice(5).trim();
		if (!data) return false;
		if (data === "[DONE]") {
			streamCompleted = true;
			return true;
		}
		const chunk = JSON.parse(data);
		streamCompleted = streamCompleted || chunkHasFinishReason(chunk);
		const delta = deltaTextFromChunk(chunk);
		if (delta) onDelta(delta);
		return false;
	};

	while (true) {
		const { done, value } = await reader.read();
		buffer += decoder.decode(value, { stream: !done });
		let newline = buffer.indexOf("\n");
		while (newline >= 0) {
			const line = buffer.slice(0, newline).replace(/\r$/, "");
			buffer = buffer.slice(newline + 1);
			if (consumeLine(line)) {
				await reader.cancel();
				return;
			}
			newline = buffer.indexOf("\n");
		}
		if (done) break;
	}
	if (buffer.trim()) consumeLine(buffer);
	if (!streamCompleted) throw new Error("聊天流提前结束，已接收内容可能不完整");
}

function MessageContent({ content }: { content: string }) {
	const blocks = content.split("```");
	return (
		<div className="min-w-0 text-sm leading-7">
			{blocks.map((block, index) => index % 2 === 0 ? (
				<span key={index} className="whitespace-pre-wrap break-words">{block}</span>
			) : (
				<pre key={index} className="my-3 max-w-full overflow-x-auto rounded-lg bg-[#181e25] p-4 text-xs leading-6 text-[#f8fafc] dark:bg-black/40"><code>{block.replace(/^[a-zA-Z0-9_+-]+\n/, "")}</code></pre>
			))}
		</div>
	);
}

function ExternalChatContent({ session }: { session: NonNullable<ReturnType<typeof useAuthGuard>["session"]> }) {
	const scope = `${session.provider || "local"}:${session.role}:${session.subjectId || session.name}`;
	const [providers, setProviders] = useState<ExternalChatProvider[]>([]);
	const [conversations, setConversations] = useState<StoredExternalChatConversation[]>([]);
	const [activeID, setActiveID] = useState("");
	const [composer, setComposer] = useState("");
	const [isLoading, setIsLoading] = useState(true);
	const [isStreaming, setIsStreaming] = useState(false);
	const [settingsOpen, setSettingsOpen] = useState(false);
	const [settingsDraft, setSettingsDraft] = useState<ChatSettingsDraft>({ systemPrompt: "", temperature: "", maxCompletionTokens: "" });
	const [copiedMessageID, setCopiedMessageID] = useState("");
	const abortRef = useRef<AbortController | null>(null);
	const composerRef = useRef<HTMLTextAreaElement | null>(null);
	const messagesScrollRef = useRef<HTMLDivElement | null>(null);
	const conversationsRef = useRef<StoredExternalChatConversation[]>([]);
	const deletionsRef = useRef<ExternalChatDeletion[]>([]);
	const activeIDRef = useRef("");
	const isStreamingRef = useRef(false);
	const remoteWarningShownRef = useRef(false);
	const localWarningShownRef = useRef(false);
	const remotePushRef = useRef<{
		running: boolean;
		pending: ExternalChatHistoryDocument | null;
	}>({ running: false, pending: null });
	// Stable refs so mount/resume/push do not re-subscribe when streaming toggles.
	const applyHistoryDocumentRef = useRef<(document: ExternalChatHistoryDocument) => ExternalChatHistoryDocument>(
		(document) => normalizeExternalChatHistoryDocument(document),
	);
	const enqueueRemotePushRef = useRef<(document: ExternalChatHistoryDocument) => void>(() => undefined);

	useEffect(() => {
		activeIDRef.current = activeID;
	}, [activeID]);
	useEffect(() => {
		isStreamingRef.current = isStreaming;
	}, [isStreaming]);

	const reportRemoteSyncFailure = useCallback(() => {
		if (remoteWarningShownRef.current) return;
		remoteWarningShownRef.current = true;
		toast.warning("云端聊天记录暂不可用，将继续使用本机记录");
	}, []);

	const reportLocalStorageFailure = useCallback(() => {
		if (localWarningShownRef.current) return;
		localWarningShownRef.current = true;
		toast.error("本机聊天记录保存失败");
	}, []);

	/** Prefer in-memory streaming messages over a lagging remote/disk snapshot. */
	const protectStreamingConversations = useCallback((
		incoming: StoredExternalChatConversation[],
	): StoredExternalChatConversation[] => {
		if (!isStreamingRef.current) return incoming;
		const memoryById = new Map(conversationsRef.current.map((item) => [item.id, item]));
		const resolved = incoming.map((item) => {
			const memory = memoryById.get(item.id);
			if (!memory) return item;
			const hasStreaming = memory.messages.some((message) => message.status === "streaming");
			if (!hasStreaming) return item;
			// Keep the live streaming snapshot; only adopt non-message metadata if remote is newer.
			return historyTimeAtOrAfter(memory.updatedAt, item.updatedAt)
				? memory
				: {
						...item,
						messages: memory.messages,
						updatedAt: memory.updatedAt,
					};
		});
		const incomingIDs = new Set(incoming.map((item) => item.id));
		for (const memory of conversationsRef.current) {
			if (!incomingIDs.has(memory.id) && memory.messages.some((message) => message.status === "streaming")) {
				resolved.push(memory);
			}
		}
		return resolved;
	}, []);

	const applyHistoryDocument = useCallback((document: ExternalChatHistoryDocument) => {
		const normalized = normalizeExternalChatHistoryDocument(document);
		const protectedItems = protectStreamingConversations(normalized.items);
		const resolved = normalizeExternalChatHistoryDocument({
			items: protectedItems,
			deletions: normalized.deletions,
		});
		deletionsRef.current = resolved.deletions;
		conversationsRef.current = resolved.items;
		setConversations(resolved.items);
		setActiveID((current) => {
			if (current && resolved.items.some((item) => item.id === current)) return current;
			return resolved.items[0]?.id || "";
		});
		return resolved;
	}, [protectStreamingConversations]);

	const pushHistoryDocument = useCallback(async (document: ExternalChatHistoryDocument) => {
		const pending = normalizeExternalChatHistoryDocument(document);
		const resolved = await saveExternalChatConversationsRemote(pending);
		await saveExternalChatConversations(scope, resolved).catch(reportLocalStorageFailure);
		const currentDoc = normalizeExternalChatHistoryDocument({
			items: conversationsRef.current,
			deletions: deletionsRef.current,
		});
		if (!externalChatHistoryDocumentsEqual(currentDoc, resolved)) {
			const activeWasDeleted =
				Boolean(activeIDRef.current) && !resolved.items.some((item) => item.id === activeIDRef.current);
			if (activeWasDeleted && isStreamingRef.current) {
				abortRef.current?.abort();
			}
			applyHistoryDocument(resolved);
		} else {
			deletionsRef.current = resolved.deletions;
		}
		return resolved;
	}, [applyHistoryDocument, reportLocalStorageFailure, scope]);

	const enqueueRemotePush = useCallback((document: ExternalChatHistoryDocument) => {
		const writeState = remotePushRef.current;
		writeState.pending = document;
		if (writeState.running) return;
		writeState.running = true;
		void (async () => {
			try {
				while (writeState.pending) {
					const pending = writeState.pending;
					writeState.pending = null;
					try {
						await pushHistoryDocument(pending);
					} catch {
						reportRemoteSyncFailure();
					}
				}
			} finally {
				writeState.running = false;
			}
		})();
	}, [pushHistoryDocument, reportRemoteSyncFailure]);

	const persistConversations = useCallback((
		items: StoredExternalChatConversation[],
		deletions = deletionsRef.current,
	) => {
		const document = normalizeExternalChatHistoryDocument({ items, deletions });
		deletionsRef.current = document.deletions;
		enqueueRemotePush(document);
	}, [enqueueRemotePush]);

	applyHistoryDocumentRef.current = applyHistoryDocument;
	enqueueRemotePushRef.current = enqueueRemotePush;

	useEffect(() => {
		let active = true;
		const unsubscribe = subscribeHistoryChange(EXTERNAL_CHAT_CONVERSATIONS_CHANGED_EVENT, () => {
			void loadExternalChatConversations(scope).then((stored) => {
				if (!active) return;
				applyHistoryDocumentRef.current(stored);
			}).catch(() => undefined);
		});
		return () => {
			active = false;
			unsubscribe();
		};
	}, [scope]);

	const applyConversations = useCallback((
		updater: (current: StoredExternalChatConversation[]) => StoredExternalChatConversation[],
		persist = true,
	) => {
		const next = updater(conversationsRef.current);
		conversationsRef.current = next;
		setConversations(next);
		if (persist) persistConversations(next);
	}, [persistConversations]);

	// Mount load only re-runs when the auth scope changes — never on streaming/active toggles.
	useEffect(() => {
		let active = true;
		const loadHistory = async () => {
			try {
				const [providerItems, remoteHistory] = await Promise.all([
					fetchExternalChatProviders(),
					fetchExternalChatConversations()
						.then((document) => ({ ok: true as const, document }))
						.catch(() => ({ ok: false as const, document: normalizeExternalChatHistoryDocument(null) })),
				]);
				if (!active) return;
				let source = remoteHistory.document;
				if (!remoteHistory.ok) {
					reportRemoteSyncFailure();
					try {
						source = await loadExternalChatConversations(scope);
					} catch {
						reportLocalStorageFailure();
						source = normalizeExternalChatHistoryDocument(null);
					}
				}
				if (!active) return;
				setProviders(providerItems);
				const firstProvider = providerItems[0];
				const repaired = source.items.map((conversation) => {
					const provider = providerItems.find((item) => item.id === conversation.providerId) || firstProvider;
					if (!provider) return conversation;
					return {
						...conversation,
						providerId: provider.id,
						model: provider.models.includes(conversation.model) ? conversation.model : provider.default_model,
					};
				});
				const nextDocument = normalizeExternalChatHistoryDocument({
					items: repaired.length > 0
						? repaired
						: [createExternalChatConversation(firstProvider?.id, firstProvider?.default_model)],
					deletions: source.deletions,
				});
				applyHistoryDocumentRef.current(nextDocument);
				if (remoteHistory.ok) {
					if (externalChatHistoryDocumentsEqual(source, nextDocument)) {
						await saveExternalChatConversations(scope, nextDocument).catch(reportLocalStorageFailure);
					} else {
						enqueueRemotePushRef.current(nextDocument);
					}
				} else {
					await saveExternalChatConversations(scope, nextDocument).catch(reportLocalStorageFailure);
				}
			} catch (error) {
				toast.error(error instanceof Error ? error.message : "加载聊天渠道失败");
			} finally {
				if (active) setIsLoading(false);
			}
		};
		void loadHistory();
		return () => {
			active = false;
			abortRef.current?.abort();
		};
	}, [reportLocalStorageFailure, reportRemoteSyncFailure, scope]);

	useHistoryResumeSync(async () => {
		try {
			const remote = await fetchExternalChatConversations();
			const resolved = applyHistoryDocumentRef.current(remote);
			await saveExternalChatConversations(scope, resolved).catch(reportLocalStorageFailure);
		} catch {
			reportRemoteSyncFailure();
		}
	}, !isLoading);

	const activeConversation = conversations.find((item) => item.id === activeID) || conversations[0];
	const activeProvider = providers.find((item) => item.id === activeConversation?.providerId);
	const sortedConversations = useMemo(
		() => [...conversations].sort((left, right) => compareHistoryTimes(right.updatedAt, left.updatedAt)),
		[conversations],
	);

	useEffect(() => {
		const container = messagesScrollRef.current;
		if (!container) return;
		const frame = window.requestAnimationFrame(() => {
			container.scrollTo({ top: container.scrollHeight, behavior: "auto" });
		});
		return () => window.cancelAnimationFrame(frame);
	}, [activeConversation?.messages, isStreaming]);

	useEffect(() => {
		const textarea = composerRef.current;
		if (!textarea) return;
		textarea.style.height = "0px";
		textarea.style.height = `${Math.min(Math.max(textarea.scrollHeight, 104), 240)}px`;
	}, [composer]);

	const newConversation = () => {
		if (isStreaming) abortRef.current?.abort();
		const provider = activeProvider || providers[0];
		const next = createExternalChatConversation(provider?.id, provider?.default_model);
		applyConversations((current) => [next, ...current]);
		setActiveID(next.id);
		setComposer("");
	};

	const deleteConversation = (conversation: StoredExternalChatConversation) => {
		if (!window.confirm(`确认删除对话“${conversation.title}”？`)) return;
		if (isStreaming && conversation.id === activeID) abortRef.current?.abort();
		const remaining = conversationsRef.current.filter((item) => item.id !== conversation.id);
		if (remaining.length === 0) {
			const provider = providers[0];
			remaining.push(createExternalChatConversation(provider?.id, provider?.default_model));
		}
		const deleted = mergeExternalChatHistoryDocuments({
			items: remaining,
			deletions: [...deletionsRef.current, { id: conversation.id, deletedAt: new Date().toISOString() }],
		}, null);
		deletionsRef.current = deleted.deletions;
		applyConversations(() => deleted.items);
		if (conversation.id === activeID) setActiveID(remaining[0].id);
	};

	const updateActiveConversation = (updates: Partial<StoredExternalChatConversation>) => {
		if (!activeConversation) return;
		applyConversations((current) => current.map((item) => item.id === activeConversation.id
			? { ...item, ...updates, updatedAt: new Date().toISOString() }
			: item));
	};

	const runCompletion = async (
		conversation: StoredExternalChatConversation,
		baseMessages: StoredExternalChatMessage[],
		userContent?: string,
	) => {
		const provider = providers.find((item) => item.id === conversation.providerId);
		if (!provider || !conversation.model) {
			toast.error("请先选择可用的渠道和模型");
			return;
		}
		const now = new Date().toISOString();
		const userMessage: StoredExternalChatMessage | null = userContent ? {
			id: createExternalChatID("message"), role: "user", content: userContent, createdAt: now, status: "complete",
		} : null;
		const assistantID = createExternalChatID("message");
		const requestMessages = [...baseMessages, ...(userMessage ? [userMessage] : [])];
		const assistantMessage: StoredExternalChatMessage = { id: assistantID, role: "assistant", content: "", createdAt: now, status: "streaming" };
		applyConversations((current) => current.map((item) => item.id === conversation.id ? {
			...item,
			title: item.messages.length === 0 && userContent ? userContent.slice(0, 30) : item.title,
			messages: limitExternalChatMessages([...requestMessages, assistantMessage]),
			updatedAt: now,
		} : item), false);
		setActiveID(conversation.id);
		setIsStreaming(true);
		const controller = new AbortController();
		abortRef.current = controller;
		let receivedText = false;

		try {
			const systemPrompt = conversation.systemPrompt.trim();
			const selectedMessages = messagesForChatRequest(requestMessages, Boolean(systemPrompt));
			const messages: ChatRequestMessage[] = [
				...(systemPrompt ? [{ role: "system" as const, content: systemPrompt }] : []),
				...selectedMessages.map((message) => ({ role: message.role, content: message.content })),
			];
			await streamExternalChat({
				provider_id: provider.id,
				model: conversation.model,
				messages,
				...(conversation.temperature.trim() ? { temperature: Number(conversation.temperature) } : {}),
				...(conversation.maxCompletionTokens.trim() ? { max_completion_tokens: Number(conversation.maxCompletionTokens) } : {}),
			}, controller.signal, (delta) => {
				receivedText = true;
				applyConversations((current) => current.map((item) => item.id === conversation.id ? {
					...item,
					messages: item.messages.map((message) => message.id === assistantID
						? { ...message, content: message.content + delta }
						: message),
				} : item), false);
			});
			applyConversations((current) => current.map((item) => item.id === conversation.id ? {
				...item,
				messages: item.messages.map((message) => message.id === assistantID
					? receivedText
						? { ...message, status: "complete" as const }
						: { ...message, status: "error" as const, error: "上游未返回文本内容" }
					: message),
			} : item), false);
		} catch (error) {
			const stopped = error instanceof DOMException && error.name === "AbortError";
			const messageError = stopped ? "已停止生成" : (error instanceof Error ? error.message : "生成失败");
			applyConversations((current) => current.map((item) => item.id === conversation.id ? {
				...item,
				messages: item.messages.map((message) => message.id === assistantID
					? { ...message, status: stopped ? "stopped" as const : "error" as const, error: messageError }
					: message),
			} : item), false);
			if (!stopped) toast.error(error instanceof Error ? error.message : "聊天请求失败");
		} finally {
			abortRef.current = null;
			applyConversations((current) => current.map((item) => item.id === conversation.id
				? { ...item, updatedAt: new Date().toISOString() }
				: item));
			setIsStreaming(false);
		}
	};

	const send = async () => {
		const content = composer.trim();
		if (!content || !activeConversation || isStreaming) return;
		setComposer("");
		await runCompletion(activeConversation, activeConversation.messages, content);
	};

	const regenerate = async (messageID: string) => {
		if (!activeConversation || isStreaming) return;
		const index = activeConversation.messages.findIndex((message) => message.id === messageID && message.role === "assistant");
		if (index < 0) return;
		const baseMessages = activeConversation.messages.slice(0, index);
		const deletedAt = new Date().toISOString();
		const removedMessages = activeConversation.messages.slice(index);
		applyConversations((current) => current.map((item) => item.id === activeConversation.id
			? {
				...item,
				messages: baseMessages,
				messageDeletions: [
					...(item.messageDeletions || []),
					...removedMessages.map((message) => ({ id: message.id, deletedAt })),
				],
				updatedAt: deletedAt,
			}
			: item), false);
		await runCompletion(activeConversation, baseMessages);
	};

	const copyMessage = async (message: StoredExternalChatMessage) => {
		await navigator.clipboard.writeText(message.content);
		setCopiedMessageID(message.id);
		window.setTimeout(() => setCopiedMessageID(""), 1500);
	};

	const openSettings = () => {
		if (!activeConversation) return;
		setSettingsDraft({
			systemPrompt: activeConversation.systemPrompt,
			temperature: activeConversation.temperature,
			maxCompletionTokens: activeConversation.maxCompletionTokens,
		});
		setSettingsOpen(true);
	};

	const saveSettings = () => {
		const temperature = settingsDraft.temperature.trim();
		const maxTokens = settingsDraft.maxCompletionTokens.trim();
		if (temperature && (!Number.isFinite(Number(temperature)) || Number(temperature) < 0 || Number(temperature) > 2)) {
			toast.error("Temperature 必须在 0 到 2 之间");
			return;
		}
		if (maxTokens && (!Number.isInteger(Number(maxTokens)) || Number(maxTokens) < 1 || Number(maxTokens) > 131072)) {
			toast.error("最大输出必须是 1 到 131072 的整数");
			return;
		}
		updateActiveConversation(settingsDraft);
		setSettingsOpen(false);
	};

	if (isLoading) return <LoadingState />;

	return (
		<section className="mx-auto grid h-[calc(100dvh-6.25rem)] min-h-0 w-full max-w-[1380px] grid-cols-1 gap-2 px-0 pb-[calc(env(safe-area-inset-bottom)+0.5rem)] sm:h-[calc(100dvh-5rem)] sm:gap-3 sm:px-3 sm:pb-6 lg:grid-cols-[240px_minmax(0,1fr)]">
			<aside className="hidden h-full min-h-0 border-r border-[#f2f3f5] pr-3 lg:flex lg:flex-col dark:border-border">
				<Button className="mb-3 h-10 w-full rounded-full" onClick={newConversation}><MessageSquarePlus className="size-4" />新建对话</Button>
				<div className="min-h-0 flex-1 space-y-1 overflow-y-auto pr-1 [scrollbar-color:rgba(142,142,147,.45)_transparent] [scrollbar-width:thin]">
					{sortedConversations.map((conversation) => (
						<div key={conversation.id} className={cn("group relative rounded-lg", conversation.id === activeConversation?.id ? "bg-white shadow-sm dark:bg-card" : "hover:bg-white/70 dark:hover:bg-card/60")}>
							<button type="button" className="block w-full px-3 py-2.5 pr-9 text-left" onClick={() => setActiveID(conversation.id)}>
								<div className="truncate text-sm font-medium">{conversation.title}</div>
								<div className="mt-1 truncate text-xs text-muted-foreground">{providers.find((item) => item.id === conversation.providerId)?.name || "渠道不可用"}</div>
							</button>
							<button type="button" onClick={() => deleteConversation(conversation)} className="absolute top-2.5 right-2 inline-flex size-7 items-center justify-center rounded-md text-muted-foreground opacity-0 hover:bg-muted hover:text-rose-600 group-hover:opacity-100" aria-label="删除对话" title="删除对话"><Trash2 className="size-3.5" /></button>
						</div>
					))}
				</div>
			</aside>

			<div className="relative flex min-h-0 min-w-0 flex-col gap-3">
				<div className="flex min-h-14 shrink-0 items-center gap-2 px-1 lg:hidden">
						<Select value={activeConversation?.id} onValueChange={setActiveID}>
							<SelectTrigger className="h-10 min-w-0 flex-1 sm:h-9"><SelectValue placeholder="选择对话" /></SelectTrigger>
							<SelectContent>{sortedConversations.map((conversation) => <SelectItem key={conversation.id} value={conversation.id}>{conversation.title}</SelectItem>)}</SelectContent>
						</Select>
						<Button size="icon" variant="outline" className="size-9 shrink-0 sm:size-10" onClick={newConversation} title="新建对话"><MessageSquarePlus className="size-4" /></Button>
						{activeConversation ? <Button size="icon" variant="ghost" className="size-9 shrink-0 text-muted-foreground hover:text-rose-600 sm:size-10" onClick={() => deleteConversation(activeConversation)} title="删除当前对话"><Trash2 className="size-4" /></Button> : null}
					</div>

				<div ref={messagesScrollRef} className="hide-scrollbar min-h-0 flex-1 overflow-y-auto overscroll-contain px-1 pt-2 pb-[14rem] sm:px-4 sm:pt-4 sm:pb-[15rem] [scrollbar-color:rgba(142,142,147,.45)_transparent] [scrollbar-width:thin]">
					{providers.length === 0 ? (
						<div className="flex h-full min-h-48 items-center justify-center text-center text-sm text-muted-foreground">暂无可用聊天渠道，请联系管理员在 API 渠道管理中启用聊天能力。</div>
					) : activeConversation?.messages.length === 0 ? (
						<ConversationEmptyState mode="chat" model={activeConversation.model} />
					) : (
						<div className="mx-auto w-full max-w-3xl space-y-7">
							{activeConversation?.messages.map((message) => (
								<div key={message.id} className={cn("group flex gap-2 sm:gap-3", message.role === "user" && "justify-end")}>
									{message.role === "assistant" ? <div className="mt-1 flex size-7 shrink-0 items-center justify-center rounded-full bg-[#181e25] text-white dark:bg-foreground dark:text-background"><Bot className="size-4" /></div> : null}
									<div className={cn("min-w-0", message.role === "user" ? "max-w-[85%] rounded-[14px] bg-[#eef3f8] px-3 py-2.5 dark:bg-muted sm:rounded-lg sm:px-4" : "flex-1 pt-0.5")}>
						{message.content ? <MessageContent content={message.content} /> : message.error ? <div className="text-sm text-rose-600">{message.error}</div> : message.status === "streaming" ? <ApiLoadingMark className="mt-1" size="media" label="正在生成回复" /> : <div className="text-sm text-rose-600">回复内容为空</div>}
										{message.content && message.error ? <div className="mt-2 text-xs text-rose-600">{message.error}</div> : null}
										{message.role === "assistant" && message.content ? <div className="mt-2 flex gap-1 opacity-100 sm:opacity-0 sm:transition sm:group-hover:opacity-100">
											<button type="button" className="inline-flex size-9 items-center justify-center rounded-lg text-muted-foreground hover:bg-muted hover:text-foreground sm:size-7 sm:rounded-md" onClick={() => void copyMessage(message)} title="复制回复" aria-label="复制回复">{copiedMessageID === message.id ? <Check className="size-4 sm:size-3.5" /> : <Copy className="size-4 sm:size-3.5" />}</button>
											<button type="button" className="inline-flex size-9 items-center justify-center rounded-lg text-muted-foreground hover:bg-muted hover:text-foreground disabled:opacity-40 sm:size-7 sm:rounded-md" onClick={() => void regenerate(message.id)} disabled={isStreaming} title="重新生成" aria-label="重新生成"><RotateCcw className="size-4 sm:size-3.5" /></button>
										</div> : null}
									</div>
									{message.role === "user" ? <div className="mt-1 flex size-7 shrink-0 items-center justify-center rounded-full bg-[#e8edf3] text-[#45515e] dark:bg-muted dark:text-foreground"><UserRound className="size-4" /></div> : null}
								</div>
							))}
							<div aria-hidden="true" />
						</div>
					)}
				</div>

				<div className="pointer-events-none absolute inset-x-0 bottom-0 z-30 px-1 pb-[calc(env(safe-area-inset-bottom)+0.5rem)] sm:px-4 sm:pb-2">
					<ConversationComposerSurface className="pointer-events-auto mx-auto max-w-[900px]">
						<div className="cursor-text" onClick={() => composerRef.current?.focus()}>
							<Textarea
								ref={composerRef}
								value={composer}
								onChange={(event) => setComposer(event.target.value.slice(0, 12000))}
								onKeyDown={(event) => {
									if (event.key === "Enter" && !event.shiftKey && !event.nativeEvent.isComposing) {
										event.preventDefault();
										void send();
									}
								}}
								placeholder="输入消息与 AI 聊天"
								enterKeyHint="send"
								onFocus={() => { document.body.dataset.externalChatComposerFocused = "true"; }}
								onBlur={() => { delete document.body.dataset.externalChatComposerFocused; }}
								className="min-h-[96px] max-h-[240px] resize-none rounded-none border-0 bg-transparent px-6 pt-6 pb-2 text-[17px] leading-7 text-[#222222] shadow-none placeholder:text-[#8e8e93] focus-visible:ring-0 dark:text-foreground dark:placeholder:text-muted-foreground sm:min-h-0 sm:px-5 sm:py-4 sm:text-[15px] sm:leading-6"
								disabled={providers.length === 0}
							/>

							<ConversationComposerToolbar onClick={(event) => event.stopPropagation()}>
								<div className="grid grid-cols-[minmax(0,1fr)_auto] items-center gap-2 sm:gap-3">
									<div className="flex min-w-0 flex-nowrap items-center gap-1.5 sm:gap-2">
										<div className="inline-flex h-9 shrink-0 items-center rounded-full bg-transparent p-0 text-xs font-medium text-[#45515e] dark:text-muted-foreground sm:h-8 sm:bg-[#f0f0f0] sm:p-0.5 sm:dark:bg-muted/70">
											<div className="inline-flex size-9 items-center justify-center gap-1.5 rounded-full bg-[#fff1f7] text-[#ea5ec1] dark:bg-rose-950/30 dark:text-pink-300 sm:h-7 sm:w-auto sm:bg-white sm:px-2.5 sm:text-[#18181b] sm:shadow-sm sm:dark:bg-background sm:dark:text-foreground">
												<MessageCircle className="size-5 sm:size-3.5" />
												<span className="hidden sm:inline">对话</span>
											</div>
										</div>

										<Select value={activeConversation?.providerId} onValueChange={(providerID) => {
											const provider = providers.find((item) => item.id === providerID);
											updateActiveConversation({ providerId: providerID, model: provider?.default_model || "" });
										}} disabled={isStreaming || providers.length === 0}>
											<SelectTrigger className="size-9 shrink-0 justify-center rounded-full border-0 bg-transparent px-0 text-[#686b73] shadow-none dark:text-muted-foreground sm:h-8 sm:w-[160px] sm:justify-between sm:border sm:border-[#e5e7eb] sm:bg-white sm:px-3 sm:text-xs sm:text-[#45515e] sm:dark:border-border sm:dark:bg-background/70 [&>svg:last-child]:hidden sm:[&>svg:last-child]:block" aria-label="选择渠道" title={`渠道：${activeProvider?.name || "未选择"}`}>
												<Server className="size-5 sm:hidden" />
												<span className="hidden min-w-0 flex-1 truncate text-left font-semibold sm:block"><SelectValue placeholder="选择渠道" /></span>
											</SelectTrigger>
											<SelectContent>{providers.map((provider) => <SelectItem key={provider.id} value={provider.id}>{provider.name}</SelectItem>)}</SelectContent>
										</Select>

										<Select value={activeConversation?.model} onValueChange={(model) => updateActiveConversation({ model })} disabled={isStreaming || !activeProvider}>
											<SelectTrigger className="size-9 shrink-0 justify-center rounded-full border-0 bg-transparent px-0 text-[#686b73] shadow-none dark:text-muted-foreground sm:h-8 sm:w-[190px] sm:justify-between sm:border sm:border-[#e5e7eb] sm:bg-white sm:px-3 sm:text-xs sm:text-[#45515e] sm:dark:border-border sm:dark:bg-background/70 [&>svg:last-child]:hidden sm:[&>svg:last-child]:block" aria-label="选择模型" title={`模型：${activeConversation?.model || "未选择"}`}>
												<Bot className="size-5 sm:hidden" />
												<span className="hidden min-w-0 flex-1 truncate text-left font-semibold sm:block"><SelectValue placeholder="选择模型" /></span>
											</SelectTrigger>
											<SelectContent>{activeProvider?.models.map((model) => <SelectItem key={model} value={model}>{model}</SelectItem>)}</SelectContent>
										</Select>

										<Popover open={settingsOpen} onOpenChange={(open) => open ? openSettings() : setSettingsOpen(false)}>
											<PopoverTrigger asChild>
												<button type="button" className={cn("inline-flex size-9 shrink-0 items-center justify-center gap-1.5 rounded-full text-[#686b73] transition hover:bg-black/[0.05] dark:text-muted-foreground dark:hover:bg-accent/60 sm:h-8 sm:w-auto sm:border sm:border-[#e5e7eb] sm:bg-white sm:px-3 sm:text-xs sm:font-medium sm:text-[#45515e] sm:dark:border-border sm:dark:bg-background/70", settingsOpen && "bg-[#eef4ff] text-[#1456f0] sm:border-[#bfdbfe] sm:bg-[#eef4ff] sm:text-[#1456f0]")} aria-label="对话参数" title="对话参数">
													<SlidersHorizontal className="size-5 sm:size-3.5" />
													<span className="hidden sm:inline">参数</span>
												</button>
											</PopoverTrigger>
											<PopoverContent align="end" side="top" sideOffset={8} className="z-[80] max-h-[min(calc(100dvh-2rem),32rem)] w-[min(calc(100vw-1rem),28rem)] overflow-y-auto rounded-[20px] border-[#e5e7eb] bg-white p-3 shadow-[0_24px_80px_-32px_rgba(15,23,42,0.35)] dark:border-border dark:bg-card sm:w-[28rem]" onOpenAutoFocus={(event) => event.preventDefault()}>
												<div className="grid gap-3">
													<label className="grid gap-1.5 text-xs font-medium text-[#45515e] dark:text-muted-foreground">System Prompt<Textarea value={settingsDraft.systemPrompt} onChange={(event) => setSettingsDraft((current) => ({ ...current, systemPrompt: event.target.value }))} className="min-h-24 resize-y text-sm" placeholder="可选" /></label>
													<div className="grid grid-cols-2 gap-2">
														<label className="grid gap-1.5 text-xs font-medium text-[#45515e] dark:text-muted-foreground">Temperature<Input type="number" min="0" max="2" step="0.1" value={settingsDraft.temperature} onChange={(event) => setSettingsDraft((current) => ({ ...current, temperature: event.target.value }))} className="h-9 text-sm" placeholder={activeProvider?.temperature == null ? "模型默认" : String(activeProvider.temperature)} /></label>
														<label className="grid gap-1.5 text-xs font-medium text-[#45515e] dark:text-muted-foreground">最大输出 Token<Input type="number" min="1" max="131072" step="1" value={settingsDraft.maxCompletionTokens} onChange={(event) => setSettingsDraft((current) => ({ ...current, maxCompletionTokens: event.target.value }))} className="h-9 text-sm" placeholder="模型默认" /></label>
													</div>
													<div className="flex justify-end gap-2"><Button size="sm" variant="outline" onClick={() => setSettingsOpen(false)}>取消</Button><Button size="sm" onClick={saveSettings}>保存参数</Button></div>
												</div>
											</PopoverContent>
										</Popover>
									</div>

									{isStreaming ? (
										<button type="button" className="inline-flex size-11 shrink-0 items-center justify-center rounded-full bg-[#181e25] text-white shadow-[0_4px_10px_rgba(24,30,37,0.12)] transition hover:bg-[#2a323d] dark:bg-foreground dark:text-background sm:size-10" onClick={() => abortRef.current?.abort()} title="停止生成" aria-label="停止生成"><Square className="size-4 fill-current" /></button>
									) : (
										<button type="button" className="inline-flex size-11 shrink-0 items-center justify-center rounded-full bg-[#181e25] text-white shadow-[0_4px_10px_rgba(24,30,37,0.12)] transition hover:bg-[#2a323d] disabled:cursor-not-allowed disabled:bg-[#e1e2e4] disabled:text-[#73777f] dark:bg-foreground dark:text-background dark:disabled:bg-muted dark:disabled:text-muted-foreground sm:size-10" onClick={() => void send()} disabled={!composer.trim() || !activeProvider} title="发送消息" aria-label="发送消息"><ArrowUp className="size-5 sm:size-4" /></button>
									)}
								</div>
							</ConversationComposerToolbar>
						</div>
					</ConversationComposerSurface>
				</div>
			</div>
		</section>
	);
}

export default function ExternalChatPage() {
	const { isCheckingAuth, session } = useAuthGuard(undefined, "/external-chat");
	if (isCheckingAuth || !session) return <LoadingState />;
	return <ExternalChatContent session={session} />;
}
