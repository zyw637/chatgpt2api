"use client";

import localforage from "localforage";

import { compareHistoryTimes } from "@/lib/history-document";

import {
  DEFAULT_CHAT_MODEL,
  DEFAULT_IMAGE_MODEL,
  isChatModel,
  isImageCreationModel,
  isImageModel,
  isImageOutputFormat,
  isImageQuality,
  supportsImageOutputCompression,
  type ImageModel,
  type ImageOutputFormat,
  type ImageQuality,
  type ImageVisibility,
} from "@/lib/api";
import { getManagedImagePathFromUrl } from "@/lib/image-path";
import { getStoredAuthSession, type StoredAuthSession } from "@/store/auth";
import { httpRequest } from "@/lib/request";
import {
  applyHistoryDeletion,
  clearHistoryWithTombstones,
  mergeHistoryDocuments,
  type HistoryDeletion,
} from "@/lib/history-document";
import { publishHistoryChange, withHistoryStorageLock } from "@/lib/history-sync";

export type ImageConversationMode = "chat" | "generate" | "image" | "edit";
export type StoredReferenceImageSource = "upload" | "conversation";

export type StoredReferenceImage = {
  name: string;
  type: string;
  /** Inline preview/payload. Prefer url/path for persisted history. */
  dataUrl?: string;
  url?: string;
  path?: string;
  source?: StoredReferenceImageSource;
};

export type StoredImage = {
  id: string;
  taskId?: string;
  status?: "loading" | "success" | "error" | "cancelled" | "message";
  taskStatus?: "queued" | "running" | "success" | "error" | "cancelled";
  path?: string;
  visibility?: ImageVisibility;
  b64_json?: string;
  url?: string;
  width?: number;
  height?: number;
  resolution?: string;
  outputFormat?: ImageOutputFormat;
  revised_prompt?: string;
  error?: string;
  text_response?: string;
};

export type ImageTurnStatus = "queued" | "generating" | "success" | "error" | "cancelled" | "message";

export type StoredImageSizeSelection = {
  mode: string;
  aspectRatio: string;
  resolution: string;
  customRatio?: string;
  customWidth: string;
  customHeight: string;
};

export type ImageTurn = {
  id: string;
  prompt: string;
  model: ImageModel;
  mode: ImageConversationMode;
  referenceImages: StoredReferenceImage[];
  count: number;
  size: string;
  sizeSelection?: StoredImageSizeSelection;
  quality?: ImageQuality;
  outputFormat?: ImageOutputFormat;
  outputCompression?: number;
  visibility?: ImageVisibility;
  images: StoredImage[];
  createdAt: string;
  updatedAt?: string;
  processingStartedAt?: string;
  status: ImageTurnStatus;
  error?: string;
};

export type ImageConversation = {
  id: string;
  title: string;
  createdAt: string;
  updatedAt: string;
  turns: ImageTurn[];
};

export type ImageConversationDeletion = HistoryDeletion;

export type ImageConversationHistoryDocument = {
  items: ImageConversation[];
  deletions: ImageConversationDeletion[];
};

export type ImageConversationStats = {
  queued: number;
  running: number;
};

export type ImageTurnLoadingCounts = {
  queued: number;
  running: number;
};

export type ImageTurnLoadingPhase = "queued" | "running" | "idle";

const imageConversationStorage = localforage.createInstance({
  name: "chatgpt2api",
  storeName: "image_conversations",
});

export const IMAGE_CONVERSATIONS_CHANGED_EVENT = "chatgpt2api:image-conversations-changed";
export const ACTIVE_IMAGE_CONVERSATION_STORAGE_KEY = "chatgpt2api:image_active_conversation_id";
export const IMAGE_ACTIVE_CONVERSATION_REQUEST_EVENT = "chatgpt2api:image-open-conversation";
const IMAGE_CONVERSATIONS_KEY_PREFIX = "items";
const IMAGE_CONVERSATION_DELETIONS_KEY_PREFIX = "deletions";
let imageConversationWriteQueue: Promise<void> = Promise.resolve();
let imageConversationRemoteQueue: Promise<void> = Promise.resolve();

function dispatchImageConversationsChanged() {
  if (typeof window === "undefined") {
    return;
  }
  publishHistoryChange(IMAGE_CONVERSATIONS_CHANGED_EVENT);
}

export function getImageTurnLoadingCounts(turn: { images: StoredImage[] }): ImageTurnLoadingCounts {
  const loadingImages = turn.images.filter((image) => image.status === "loading");
  return {
    // Missing taskStatus on a loading image means "not yet started" → count as queued.
    queued: loadingImages.filter((image) => image.taskStatus !== "running").length,
    running: loadingImages.filter((image) => image.taskStatus === "running").length,
  };
}

export function getImageTurnLoadingPhase(turn: { images: StoredImage[] }): ImageTurnLoadingPhase {
  const { queued, running } = getImageTurnLoadingCounts(turn);
  if (running > 0) {
    return "running";
  }
  if (queued > 0) {
    return "queued";
  }
  return "idle";
}

export function getStoredImageLoadingPhase(image: StoredImage): ImageTurnLoadingPhase {
  if (image.status !== "loading") {
    return "idle";
  }
  return image.taskStatus === "running" ? "running" : "queued";
}

/** True while a turn still has in-flight image work or a non-terminal status. */
export function isImageTurnInFlight(turn: Pick<ImageTurn, "status" | "images">) {
  return (
    turn.status === "queued" ||
    turn.status === "generating" ||
    turn.images.some((image) => image.status === "loading")
  );
}

export function isImageConversationInFlight(conversation: Pick<ImageConversation, "turns">) {
  return conversation.turns.some((turn) => isImageTurnInFlight(turn));
}

function imageTurnProgressScore(turn: Pick<ImageTurn, "status" | "images">) {
  const settled = turn.images.filter((image) => image.status !== "loading").length;
  const success = turn.images.filter(
    (image) => image.status === "success" || image.status === "message",
  ).length;
  const statusScore =
    turn.status === "success" || turn.status === "message"
      ? 100
      : turn.status === "error" || turn.status === "cancelled"
        ? 90
        : turn.status === "generating"
          ? 50
          : turn.status === "queued"
            ? 20
            : 0;
  return statusScore * 1000 + success * 10 + settled;
}

function imageTurnTimestamp(turn: Pick<ImageTurn, "createdAt" | "updatedAt">, conversationTimestamp = 0) {
  return turn.updatedAt ? getTimestamp(turn.updatedAt) : conversationTimestamp || getTimestamp(turn.createdAt);
}

function conversationProgressScore(conversation: Pick<ImageConversation, "turns">) {
  return conversation.turns.reduce((sum, turn) => sum + imageTurnProgressScore(turn), 0);
}

/**
 * Prefer the newer conversation version. When versions tie, use completion progress
 * so a lagging in-flight snapshot cannot overwrite a completed snapshot.
 */
export function pickPreferredConversation(
  current: ImageConversation,
  next: ImageConversation,
  options: { preferNextOnTie?: boolean } = {},
): ImageConversation {
  const currentTs = getTimestamp(current.updatedAt);
  const nextTs = getTimestamp(next.updatedAt);
  if (nextTs !== currentTs) {
    return nextTs > currentTs ? next : current;
  }
  const currentScore = conversationProgressScore(current);
  const nextScore = conversationProgressScore(next);
  if (nextScore !== currentScore) {
    return nextScore > currentScore ? next : current;
  }

  const currentInFlight = isImageConversationInFlight(current);
  const nextInFlight = isImageConversationInFlight(next);
  // When scores match and either side is mid-flight, keep the side that still has
  // live loading work if the other is also mid-flight; otherwise prefer terminal.
  if (currentInFlight !== nextInFlight) {
    // Prefer the terminal (not in-flight) snapshot when scores are equal — e.g.
    // both report the same settled counts but one still has status "generating".
    return nextInFlight ? current : next;
  }

  // Equal progress + equal timestamp: caller decides (write path prefers next).
  return options.preferNextOnTie === false ? current : next;
}

/** Keep only live local work on top of a cloud-authoritative snapshot. */
export function preserveInFlightConversations(
  cloudItems: ImageConversation[],
  memoryItems: ImageConversation[],
): ImageConversation[] {
  const cloudById = new Map(cloudItems.map((item) => [item.id, item]));
  const memoryById = new Map(memoryItems.map((item) => [item.id, item]));
  const resolved = cloudItems.map((cloud) => {
    const memory = memoryById.get(cloud.id);
    return memory && isImageConversationInFlight(memory)
      ? mergeImageConversationSnapshots(cloud, memory)
      : cloud;
  });
  for (const memory of memoryItems) {
    if (!cloudById.has(memory.id) && isImageConversationInFlight(memory)) {
      resolved.push(memory);
    }
  }
  return sortImageConversations(resolved);
}

/** Derive turn status from images; once processing has started, never fall back to queued. */
export function deriveImageTurnStatus(
  turn: Pick<ImageTurn, "status" | "images" | "processingStartedAt">,
): Pick<ImageTurn, "status" | "error"> {
  const loadingCounts = getImageTurnLoadingCounts(turn);
  const failedCount = turn.images.filter((image) => image.status === "error").length;
  const successCount = turn.images.filter((image) => image.status === "success").length;
  const cancelledCount = turn.images.filter((image) => image.status === "cancelled").length;
  const messageCount = turn.images.filter((image) => image.status === "message").length;
  const hasStartedProcessing =
    Boolean(turn.processingStartedAt) ||
    turn.status === "generating" ||
    turn.images.some((image) => image.taskStatus === "running");

  if (loadingCounts.running > 0) {
    return { status: "generating", error: undefined };
  }
  if (loadingCounts.queued > 0) {
    // Monotonic: after submit/bootstrap, keep "generating" even if slots are still queued.
    if (hasStartedProcessing) {
      return { status: "generating", error: undefined };
    }
    return { status: "queued", error: undefined };
  }
  if (failedCount > 0) {
    return {
      status: "error",
      error: buildTurnOutcomeMessage(successCount, failedCount, cancelledCount),
    };
  }
  if (cancelledCount > 0) {
    return {
      status: "cancelled",
      error: buildTurnOutcomeMessage(successCount, failedCount, cancelledCount),
    };
  }
  if (successCount > 0) {
    return { status: "success", error: undefined };
  }
  if (messageCount > 0) {
    return { status: "message", error: undefined };
  }
  return { status: hasStartedProcessing ? "generating" : "queued", error: undefined };
}

function buildTurnOutcomeMessage(successCount: number, failedCount: number, cancelledCount: number) {
  const parts = [`成功 ${successCount} 张`];
  if (failedCount > 0) {
    parts.push(`失败 ${failedCount} 张`);
  }
  if (cancelledCount > 0) {
    parts.push(`终止 ${cancelledCount} 张`);
  }
  return parts.join("，");
}

/**
 * Never let a completed image regress. By default also blocks running→queued
 * (submit/bootstrap flicker). Callers may allow running→queued when the backend
 * task is already running and this slot is waiting on a creation-unit.
 */
export function preferMonotonicImageTaskStatus(
  previous: StoredImage["taskStatus"] | undefined,
  next: StoredImage["taskStatus"] | undefined,
  options: { allowRunningToQueued?: boolean } = {},
): StoredImage["taskStatus"] | undefined {
  if (next === "success" || next === "error" || next === "cancelled") {
    return next;
  }
  if (previous === "running" && next === "queued") {
    return options.allowRunningToQueued ? "queued" : "running";
  }
  if (previous === "success" || previous === "error" || previous === "cancelled") {
    return previous;
  }
  return next ?? previous;
}

function conversationScopeFromSession(session: StoredAuthSession | null) {
  if (!session) {
    return "anonymous";
  }
  const subjectId = session.subjectId.trim();
  if (!subjectId) {
    return `${session.provider || "local"}:${session.role}:unknown`;
  }
  return `${session.provider || "local"}:${session.role}:${subjectId}`;
}

async function imageConversationsStorageKey() {
  const session = await getStoredAuthSession();
  return `${IMAGE_CONVERSATIONS_KEY_PREFIX}:${conversationScopeFromSession(session)}`;
}

async function imageConversationDeletionsStorageKey() {
  const session = await getStoredAuthSession();
  return `${IMAGE_CONVERSATION_DELETIONS_KEY_PREFIX}:${conversationScopeFromSession(session)}`;
}

function isInlineDataUrl(value: unknown): boolean {
  return typeof value === "string" && value.startsWith("data:") && value.includes(";base64,");
}

function shouldPersistInlineDataUrl(value: string | undefined, hasDurableRef: boolean): boolean {
  if (!value) {
    return false;
  }
  // Prefer durable url/path for history. Keep upload-only dataUrls so regenerate works.
  if (isInlineDataUrl(value)) {
    return !hasDurableRef;
  }
  // Non-data URLs are small and may be useful as absolute remote links.
  return value.length > 0;
}

function normalizeStoredImage(image: StoredImage): StoredImage {
  const url = typeof image.url === "string" && image.url ? image.url : undefined;
  const width = Number(image.width);
  const height = Number(image.height);
  const resolution = typeof image.resolution === "string" && image.resolution ? image.resolution : undefined;
  const path =
    typeof image.path === "string" && image.path
      ? image.path
      : url
        ? getManagedImagePathFromUrl(url) || undefined
        : undefined;
  const taskStatus =
    image.taskStatus === "queued" ||
    image.taskStatus === "running" ||
    image.taskStatus === "success" ||
    image.taskStatus === "error" ||
    image.taskStatus === "cancelled"
      ? image.taskStatus
      : image.status === "loading"
        ? "queued"
        : undefined;
  const hasDurableImage = Boolean(url || path);
  const b64 =
    typeof image.b64_json === "string" && image.b64_json && !hasDurableImage ? image.b64_json : undefined;
  const normalized: StoredImage = {
    id: String(image.id || ""),
    taskId: typeof image.taskId === "string" && image.taskId ? image.taskId : undefined,
    taskStatus,
    path,
    visibility:
      image.visibility === "public" || image.visibility === "private" ? image.visibility : undefined,
    // Never persist full image base64 into conversation history when url/path exists.
    ...(b64 ? { b64_json: b64 } : {}),
    url,
    width: Number.isFinite(width) && width > 0 ? width : undefined,
    height: Number.isFinite(height) && height > 0 ? height : undefined,
    resolution,
    outputFormat: isImageOutputFormat(image.outputFormat) ? image.outputFormat : undefined,
    revised_prompt: typeof image.revised_prompt === "string" ? image.revised_prompt : undefined,
    text_response: typeof image.text_response === "string" && image.text_response ? image.text_response : undefined,
    error: typeof image.error === "string" ? image.error : undefined,
  };
  if (image.status === "loading" || image.status === "error" || image.status === "success" || image.status === "cancelled" || image.status === "message") {
    return { ...normalized, status: image.status };
  }
  return {
    ...normalized,
    status: b64 || url || path ? "success" : "loading",
  };
}

function normalizeReferenceImage(image: StoredReferenceImage & Record<string, unknown>): StoredReferenceImage {
  const source =
    image.source === "upload" || image.source === "conversation"
      ? image.source
      : undefined;
  const rawUrl = typeof image.url === "string" && image.url ? image.url : undefined;
  const path =
    typeof image.path === "string" && image.path
      ? image.path
      : rawUrl
        ? getManagedImagePathFromUrl(rawUrl) || undefined
        : undefined;
  const url = rawUrl;
  const hasDurableRef = Boolean(url || path);
  const dataUrl =
    typeof image.dataUrl === "string" && shouldPersistInlineDataUrl(image.dataUrl, hasDurableRef)
      ? image.dataUrl
      : undefined;
  return {
    name: image.name || "reference.png",
    type: image.type || "image/png",
    ...(dataUrl ? { dataUrl } : {}),
    ...(url ? { url } : {}),
    ...(path ? { path } : {}),
    ...(source ? { source } : {}),
  };
}

/** Compact conversation payload for local + remote history APIs. */
export function compactImageConversationForHistory(conversation: ImageConversation): ImageConversation {
  return normalizeConversation(conversation);
}

function normalizeImageMode(value: unknown, referenceImages: StoredReferenceImage[]): ImageConversationMode {
  if (value === "chat") {
    return "chat";
  }
  if (value === "generate") {
    return "generate";
  }
  if (value === "image") {
    return "image";
  }
  if (value === "edit") {
    return referenceImages.some((image) => image.source === "conversation") ? "edit" : "image";
  }
  return referenceImages.length > 0 ? "image" : "generate";
}

function normalizeSizeSelection(value: unknown): StoredImageSizeSelection | undefined {
  if (!value || typeof value !== "object") {
    return undefined;
  }
  const source = value as Record<string, unknown>;
  const selection = {
    mode: typeof source.mode === "string" ? source.mode : "",
    aspectRatio: typeof source.aspectRatio === "string" ? source.aspectRatio : "",
    resolution: typeof source.resolution === "string" ? source.resolution : "",
    customRatio: typeof source.customRatio === "string" ? source.customRatio : "",
    customWidth: typeof source.customWidth === "string" ? source.customWidth : "",
    customHeight: typeof source.customHeight === "string" ? source.customHeight : "",
  };
  if (
    !selection.mode &&
    !selection.aspectRatio &&
    !selection.resolution &&
    !selection.customRatio &&
    !selection.customWidth &&
    !selection.customHeight
  ) {
    return undefined;
  }
  return selection;
}

function normalizeOutputCompression(value: unknown): number | undefined {
  if (value === undefined || value === null || String(value).trim() === "") {
    return undefined;
  }
  const numeric = Number(value);
  if (!Number.isFinite(numeric) || numeric < 0) {
    return undefined;
  }
  return Math.min(100, Math.round(numeric));
}

function dataUrlMimeType(dataUrl: string) {
  const match = dataUrl.match(/^data:(.*?);base64,/);
  return match?.[1] || "image/png";
}

function hasReferenceImagePayload(image: StoredReferenceImage) {
  return Boolean(
    (typeof image.dataUrl === "string" && image.dataUrl.length > 0) ||
      (typeof image.url === "string" && image.url.length > 0) ||
      (typeof image.path === "string" && image.path.length > 0),
  );
}

/** Prefer inline dataUrl, then managed url/path for UI preview/submit. */
export function resolveReferenceImageSrc(image: Pick<StoredReferenceImage, "dataUrl" | "url" | "path">): string {
  if (typeof image.dataUrl === "string" && image.dataUrl) {
    return image.dataUrl;
  }
  if (typeof image.url === "string" && image.url) {
    return image.url;
  }
  if (typeof image.path === "string" && image.path) {
    const normalized = image.path.replace(/^\/+/, "");
    if (normalized.startsWith("images/")) {
      return `/${normalized}`;
    }
    return `/images/${normalized
      .split("/")
      .filter(Boolean)
      .map((segment) => encodeURIComponent(segment))
      .join("/")}`;
  }
  return "";
}

function getLegacyReferenceImages(source: Record<string, unknown>): StoredReferenceImage[] {
  if (Array.isArray(source.referenceImages)) {
    return source.referenceImages
      .filter((image): image is StoredReferenceImage => {
        if (!image || typeof image !== "object") {
          return false;
        }
        const candidate = image as StoredReferenceImage;
        return hasReferenceImagePayload(candidate);
      })
      .map(normalizeReferenceImage)
      .filter(hasReferenceImagePayload);
  }

  if (source.sourceImage && typeof source.sourceImage === "object") {
    const image = source.sourceImage as { dataUrl?: unknown; fileName?: unknown };
    if (typeof image.dataUrl === "string" && image.dataUrl) {
      return [
        {
          name: typeof image.fileName === "string" && image.fileName ? image.fileName : "reference.png",
          type: dataUrlMimeType(image.dataUrl),
          dataUrl: image.dataUrl,
          source: "upload",
        },
      ];
    }
  }

  return [];
}

function normalizeTurn(turn: ImageTurn & Record<string, unknown>): ImageTurn {
  const normalizedImages = Array.isArray(turn.images) ? turn.images.map(normalizeStoredImage) : [];
  const referenceImages = getLegacyReferenceImages(turn);
  const mode = normalizeImageMode(turn.mode, referenceImages);
  const sizeSelection = normalizeSizeSelection(turn.sizeSelection);
  const visibility: ImageVisibility = turn.visibility === "public" ? "public" : "private";
  const images = normalizedImages.map((image) =>
    image.visibility ? image : { ...image, visibility },
  );
  const model =
    mode === "chat"
      ? isChatModel(turn.model)
        ? turn.model
        : DEFAULT_CHAT_MODEL
      : isImageCreationModel(turn.model)
        ? turn.model
        : DEFAULT_IMAGE_MODEL;
  const storedStatus: ImageTurnStatus | undefined =
    turn.status === "queued" ||
    turn.status === "generating" ||
    turn.status === "success" ||
    turn.status === "error" ||
    turn.status === "cancelled" ||
    turn.status === "message"
      ? turn.status
      : undefined;
  const processingStartedAt =
    typeof turn.processingStartedAt === "string" ? turn.processingStartedAt : undefined;
  const createdAt = String(turn.createdAt || new Date().toISOString());
  const updatedAt = typeof turn.updatedAt === "string" && turn.updatedAt ? turn.updatedAt : createdAt;
  const hasLoadingImages = images.some((image) => image.status === "loading");
  // When every image is settled, always re-derive so stale "generating" cannot stick.
  // While loading, keep a monotonic derive that will not bounce processing → queued.
  const derived = deriveImageTurnStatus({
    status: storedStatus || "queued",
    images,
    processingStartedAt,
  });
  const status: ImageTurnStatus = hasLoadingImages
    ? storedStatus === "generating" || derived.status === "generating"
      ? "generating"
      : derived.status
    : derived.status;

  return {
    id: String(turn.id || `${Date.now()}`),
    prompt: String(turn.prompt || ""),
    model,
    mode,
    referenceImages,
    count: Math.max(1, Number(turn.count || images.length || 1)),
    size: typeof turn.size === "string" ? turn.size : "",
    ...(sizeSelection ? { sizeSelection } : {}),
    quality: isImageQuality(turn.quality) ? turn.quality : undefined,
    outputFormat: isImageOutputFormat(turn.outputFormat) ? turn.outputFormat : undefined,
    outputCompression:
      isImageOutputFormat(turn.outputFormat) && supportsImageOutputCompression(turn.outputFormat)
        ? normalizeOutputCompression(turn.outputCompression)
        : undefined,
    visibility,
    images,
    createdAt,
    updatedAt,
    processingStartedAt,
    status,
    error:
      status === "error" || status === "cancelled"
        ? typeof turn.error === "string" && turn.error
          ? turn.error
          : derived.error
        : undefined,
  };
}

function normalizeConversation(conversation: ImageConversation & Record<string, unknown>): ImageConversation {
  const legacyReferenceImages = getLegacyReferenceImages(conversation);
  const legacyMode = normalizeImageMode(conversation.mode, legacyReferenceImages);
  const turns = Array.isArray(conversation.turns)
    ? conversation.turns.map((turn) => normalizeTurn(turn as ImageTurn & Record<string, unknown>))
    : [
        normalizeTurn({
          id: String(conversation.id || `${Date.now()}`),
          prompt: String(conversation.prompt || ""),
          model: isImageModel(conversation.model)
            ? conversation.model
            : legacyMode === "chat"
              ? DEFAULT_CHAT_MODEL
              : DEFAULT_IMAGE_MODEL,
          mode: legacyMode,
          referenceImages: legacyReferenceImages,
          count: Number(conversation.count || 1),
          size: typeof conversation.size === "string" ? conversation.size : "",
          quality: isImageQuality(conversation.quality) ? conversation.quality : undefined,
          outputFormat: isImageOutputFormat(conversation.outputFormat) ? conversation.outputFormat : undefined,
          outputCompression: normalizeOutputCompression(conversation.outputCompression),
          images: Array.isArray(conversation.images) ? (conversation.images as StoredImage[]) : [],
          createdAt: String(conversation.createdAt || new Date().toISOString()),
          status:
            conversation.status === "generating" || conversation.status === "success" || conversation.status === "error" || conversation.status === "message"
              ? conversation.status
              : "success",
          error: typeof conversation.error === "string" ? conversation.error : undefined,
        }),
      ];
  const lastTurn = turns.length > 0 ? turns[turns.length - 1] : null;

  return {
    id: String(conversation.id || `${Date.now()}`),
    title: String(conversation.title || ""),
    createdAt: String(conversation.createdAt || lastTurn?.createdAt || new Date().toISOString()),
    updatedAt: String(conversation.updatedAt || lastTurn?.createdAt || new Date().toISOString()),
    turns,
  };
}

function sortImageConversations(conversations: ImageConversation[]): ImageConversation[] {
  return [...conversations].sort((a, b) => compareHistoryTimes(b.updatedAt, a.updatedAt));
}

function getTimestamp(value: string) {
  const time = new Date(value).getTime();
  return Number.isFinite(time) ? time : 0;
}

/** Merge concurrent append-only turns without dropping either device's work. */
export function mergeImageConversationSnapshots(current: ImageConversation, next: ImageConversation) {
  const preferred = pickPreferredConversation(current, next, { preferNextOnTie: true });
  const currentTimestamp = getTimestamp(current.updatedAt);
  const nextTimestamp = getTimestamp(next.updatedAt);
  const turns = mergeImageTurnsWithVersion(current.turns, next.turns, currentTimestamp, nextTimestamp);
  return {
    ...preferred,
    turns,
    updatedAt: nextTimestamp >= currentTimestamp ? next.updatedAt : current.updatedAt,
  };
}

function mergeImageTurnsWithVersion(
  left: ImageTurn[],
  right: ImageTurn[],
  leftConversationTimestamp: number,
  rightConversationTimestamp: number,
) {
  const turns = new Map<string, { turn: ImageTurn; conversationTimestamp: number }>();
  for (const [turn, conversationTimestamp] of [
    ...left.map((item) => [item, leftConversationTimestamp] as const),
    ...right.map((item) => [item, rightConversationTimestamp] as const),
  ]) {
    const currentEntry = turns.get(turn.id);
    if (!currentEntry) {
      turns.set(turn.id, { turn, conversationTimestamp });
      continue;
    }
    const current = currentEntry.turn;
    const currentTimestamp = imageTurnTimestamp(current, currentEntry.conversationTimestamp);
    const nextTimestamp = imageTurnTimestamp(turn, conversationTimestamp);
    const currentScore = imageTurnProgressScore(current);
    const nextScore = imageTurnProgressScore(turn);
    if (
      nextTimestamp > currentTimestamp ||
      (nextTimestamp === currentTimestamp &&
        (nextScore > currentScore || (nextScore === currentScore && turn.createdAt >= current.createdAt)))
    ) {
      turns.set(turn.id, { turn, conversationTimestamp });
    }
  }
  return [...turns.values()].map((entry) => entry.turn).sort(
    (leftTurn, rightTurn) => getTimestamp(leftTurn.createdAt) - getTimestamp(rightTurn.createdAt),
  );
}

function queueImageConversationWrite<T>(operation: () => Promise<T>): Promise<T> {
  const result = imageConversationWriteQueue.then(operation);
  imageConversationWriteQueue = result.then(
    () => undefined,
    () => undefined,
  );
  return result;
}

function queueImageConversationRemoteOperation<T>(operation: () => Promise<T>): Promise<T> {
  const result = imageConversationRemoteQueue.then(operation, operation);
  imageConversationRemoteQueue = result.then(
    () => undefined,
    () => undefined,
  );
  return result;
}

async function readStoredImageConversations(storageKey?: string): Promise<ImageConversation[]> {
  storageKey = storageKey || await imageConversationsStorageKey();
  const items =
    (await imageConversationStorage.getItem<Array<ImageConversation & Record<string, unknown>>>(
      storageKey,
    )) || [];
  return items.map(normalizeConversation);
}

async function readStoredImageHistory(
  storageKey?: string,
  deletionsStorageKey?: string,
): Promise<ImageConversationHistoryDocument> {
  const [items, deletions] = await Promise.all([
    readStoredImageConversations(storageKey),
    imageConversationStorage.getItem<ImageConversationDeletion[]>(
      deletionsStorageKey || (await imageConversationDeletionsStorageKey()),
    ),
  ]);
  return { items: sortImageConversations(items), deletions: Array.isArray(deletions) ? deletions : [] };
}

function mergeImageHistoryDocuments(left: ImageConversationHistoryDocument, right: ImageConversationHistoryDocument): ImageConversationHistoryDocument {
  return mergeHistoryDocuments(
    {
      items: (left.items || []).map(normalizeConversation),
      deletions: left.deletions || [],
    },
    {
      items: (right.items || []).map(normalizeConversation),
      deletions: right.deletions || [],
    },
    {
      maxItems: 50,
      maxDeletions: 500,
      pickItemWinner: mergeImageConversationSnapshots,
    },
  );
}

async function updateStoredImageHistory(
  updater: (current: ImageConversationHistoryDocument) => ImageConversationHistoryDocument,
) {
  const storageKey = await imageConversationsStorageKey();
  const deletionsStorageKey = await imageConversationDeletionsStorageKey();
  const next = await withHistoryStorageLock(`image:${storageKey}`, async () => {
    const current = await readStoredImageHistory(storageKey, deletionsStorageKey);
    const document = updater(current);
    const compact: ImageConversationHistoryDocument = {
      items: document.items.map(compactImageConversationForHistory),
      deletions: document.deletions,
    };
    await Promise.all([
      imageConversationStorage.setItem(storageKey, compact.items),
      imageConversationStorage.setItem(deletionsStorageKey, compact.deletions),
    ]);
    return compact;
  });
  dispatchImageConversationsChanged();
  return next;
}

async function replaceStoredImageHistory(document: ImageConversationHistoryDocument) {
  const storageKey = await imageConversationsStorageKey();
  const deletionsStorageKey = await imageConversationDeletionsStorageKey();
  const compact = mergeImageHistoryDocuments(
    { items: [], deletions: [] },
    {
      items: (document.items || []).map(compactImageConversationForHistory),
      deletions: document.deletions || [],
    },
  );
  await withHistoryStorageLock(`image:${storageKey}`, async () => {
    await Promise.all([
      imageConversationStorage.setItem(storageKey, compact.items),
      imageConversationStorage.setItem(deletionsStorageKey, compact.deletions),
    ]);
  });
  dispatchImageConversationsChanged();
  return compact;
}

export async function listImageConversations(): Promise<ImageConversation[]> {
  return sortImageConversations(await readStoredImageConversations());
}

/**
 * GET the cloud-authoritative history and replace the local cache.
 */
export function pullImageConversationsRemote(): Promise<ImageConversation[]> {
  return queueImageConversationRemoteOperation(async () => {
    const remote = await httpRequest<Partial<ImageConversationHistoryDocument>>("/api/image-conversations");
    const persisted = await replaceStoredImageHistory({
      items: Array.isArray(remote.items) ? remote.items : [],
      deletions: Array.isArray(remote.deletions) ? remote.deletions : [],
    });
    return persisted.items;
  });
}

export type PushImageConversationsOptions = {
  /** Canonical document to push, used by delete and clear operations. */
  document?: ImageConversationHistoryDocument;
};

/**
 * PUT the current in-memory history to the cloud and cache the canonical response.
 */
export function pushImageConversationsRemote(
  memoryItems?: ImageConversation[],
  options: PushImageConversationsOptions = {},
): Promise<ImageConversation[]> {
  return queueImageConversationRemoteOperation(async () => {
    const cached = options.document || (await readStoredImageHistory());
    const outgoing: ImageConversationHistoryDocument = options.document
      ? cached
      : {
          items: (memoryItems === undefined ? cached.items : memoryItems).map(compactImageConversationForHistory),
          deletions: cached.deletions,
        };
    const saved = await httpRequest<ImageConversationHistoryDocument>("/api/image-conversations", {
      method: "PUT",
      body: outgoing,
    });
    const persisted = await replaceStoredImageHistory({
      items: Array.isArray(saved.items) ? saved.items : [],
      deletions: Array.isArray(saved.deletions) ? saved.deletions : [],
    });
    return persisted.items;
  });
}

export async function saveImageConversations(conversations: ImageConversation[]): Promise<void> {
  await queueImageConversationWrite(async () => {
    await updateStoredImageHistory((current) =>
      mergeImageHistoryDocuments(current, { items: conversations, deletions: [] }),
    );
  });
}

export async function saveImageConversation(conversation: ImageConversation): Promise<void> {
  await queueImageConversationWrite(async () => {
    await updateStoredImageHistory((current) =>
      mergeImageHistoryDocuments(current, { items: [conversation], deletions: [] }),
    );
  });
}

export async function deleteImageConversation(id: string): Promise<ImageConversation[]> {
  const cached = await readStoredImageHistory();
  const document = applyHistoryDeletion(cached, id, new Date().toISOString(), {
    maxItems: 50,
    maxDeletions: 500,
  });
  return pushImageConversationsRemote(undefined, { document });
}

export async function clearImageConversations(): Promise<ImageConversation[]> {
  const cached = await readStoredImageHistory();
  const document = clearHistoryWithTombstones(cached, new Date().toISOString(), {
    maxItems: 50,
    maxDeletions: 500,
  });
  return pushImageConversationsRemote(undefined, { document });
}

export function getImageConversationStats(conversation: ImageConversation | null): ImageConversationStats {
  if (!conversation) {
    return { queued: 0, running: 0 };
  }

  return conversation.turns.reduce(
    (acc, turn) => {
      if (turn.status === "queued") {
        acc.queued += 1;
      } else if (turn.status === "generating") {
        acc.running += 1;
      }
      return acc;
    },
    { queued: 0, running: 0 },
  );
}
