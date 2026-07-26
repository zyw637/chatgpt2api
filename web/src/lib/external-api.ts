import {
  normalizeExternalChatHistoryDocument,
  type ExternalChatHistoryDocument,
} from "@/app/external-chat/store";
import { httpRequest } from "@/lib/request";
import type {
  ExternalChatProvider,
  ExternalImageProvider,
  ExternalImageProviderInput,
  ExternalImageSettings,
  ExternalImageSubmission,
  ExternalImageTask,
} from "@/lib/api";

export async function fetchExternalImageProviders() {
  const data = await httpRequest<{ items?: ExternalImageProvider[] | null }>("/api/external-image-providers");
  return Array.isArray(data.items) ? data.items : [];
}

export async function fetchExternalChatProviders() {
  const data = await httpRequest<{ items?: ExternalChatProvider[] | null }>("/api/external-chat-providers");
  return Array.isArray(data.items) ? data.items : [];
}

export async function fetchExternalChatConversations() {
  const data = await httpRequest<ExternalChatHistoryDocument>("/api/external-chat-conversations");
  return normalizeExternalChatHistoryDocument(data);
}

export async function saveExternalChatConversationsRemote(document: ExternalChatHistoryDocument) {
  const data = await httpRequest<ExternalChatHistoryDocument>("/api/external-chat-conversations", {
    method: "PUT",
    body: normalizeExternalChatHistoryDocument(document),
    redirectOnUnauthorized: false,
  });
  return normalizeExternalChatHistoryDocument(data);
}

export async function fetchAdminExternalImageProviders() {
  const data = await httpRequest<{
    items?: ExternalImageProvider[] | null;
    settings?: ExternalImageSettings;
  }>("/api/admin/external-image-providers");
  return {
    items: Array.isArray(data.items) ? data.items : [],
    settings: data.settings ?? {
      user_concurrent_limit: 2,
      user_rpm_limit: 10,
      chat_user_concurrent_limit: 2,
      chat_user_rpm_limit: 20,
    },
  };
}

export async function createExternalImageProvider(provider: ExternalImageProviderInput) {
  return httpRequest<{ item: ExternalImageProvider; items: ExternalImageProvider[] }>(
    "/api/admin/external-image-providers",
    { method: "POST", body: provider },
  );
}

export async function updateExternalImageProvider(providerId: string, updates: Partial<ExternalImageProviderInput>) {
  return httpRequest<{ item: ExternalImageProvider; items: ExternalImageProvider[] }>(
    `/api/admin/external-image-providers/${encodeURIComponent(providerId)}`,
    { method: "PATCH", body: updates },
  );
}

export async function deleteExternalImageProvider(providerId: string) {
  return httpRequest<{ items: ExternalImageProvider[] }>(
    `/api/admin/external-image-providers/${encodeURIComponent(providerId)}`,
    { method: "DELETE" },
  );
}

export async function testExternalImageProvider(providerId: string) {
  return httpRequest<{ result: { ok: boolean; status: number; latency_ms: number; available_models: string[]; missing_models: string[]; default_model_available: boolean } }>(
    `/api/admin/external-image-providers/${encodeURIComponent(providerId)}/test`,
    { method: "POST", body: {} },
  );
}

export async function updateExternalImageSettings(settings: ExternalImageSettings) {
  return httpRequest<{ settings: ExternalImageSettings }>("/api/admin/external-image-providers/settings", {
    method: "PATCH",
    body: settings,
  });
}

export async function createExternalImageTask(submission: ExternalImageSubmission) {
  const formData = new FormData();
  formData.append("client_task_id", submission.clientTaskId);
  formData.append("provider_id", submission.providerId);
  formData.append("model", submission.model);
  formData.append("prompt", submission.prompt);
  formData.append("n", String(submission.count));
  if (submission.size) formData.append("size", submission.size);
  if (submission.aspectRatio) formData.append("aspect_ratio", submission.aspectRatio);
  if (submission.quality) formData.append("quality", submission.quality);
  if (typeof submission.temperature === "number") formData.append("temperature", String(submission.temperature));
  submission.references?.forEach((file) => formData.append("references", file));
  return httpRequest<ExternalImageTask>("/api/external-image-tasks/generations", {
    method: "POST",
    body: formData,
  });
}

export async function fetchExternalImageTasks(ids: string[] = []) {
  const params = new URLSearchParams({ page_size: "100" });
  if (ids.length > 0) params.set("ids", ids.join(","));
  const data = await httpRequest<{ items?: ExternalImageTask[] | null }>(
    `/api/external-image-tasks?${params.toString()}`,
    { headers: { "Cache-Control": "no-cache", Pragma: "no-cache" } },
  );
  return Array.isArray(data.items) ? data.items : [];
}

export async function cancelExternalImageTask(clientTaskId: string) {
  return httpRequest<ExternalImageTask>(`/api/external-image-tasks/${encodeURIComponent(clientTaskId)}/cancel`, {
    method: "POST",
    body: {},
  });
}

export async function retryExternalImageTask(clientTaskId: string) {
  return httpRequest<ExternalImageTask>(`/api/external-image-tasks/${encodeURIComponent(clientTaskId)}/retry`, {
    method: "POST",
    body: {},
  });
}

export async function deleteExternalImageTasks(ids: string[]) {
  return httpRequest<{ items?: ExternalImageTask[] | null }>("/api/external-image-tasks", {
    method: "DELETE",
    body: { ids },
  }).then((data) => (Array.isArray(data.items) ? data.items : []));
}
