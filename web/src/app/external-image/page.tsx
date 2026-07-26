"use client";

import {
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ClipboardEvent,
  type CSSProperties,
  type DragEvent,
  type KeyboardEvent,
  type PointerEvent,
  type ReactNode,
} from "react";
import {
  ArrowUp,
  Bot,
  Check,
  ChevronDown,
  CircleStop,
  Download,
  Eye,
  ImagePlus,
  Images,
  Plus,
  RefreshCw,
  RotateCcw,
  Server,
  SlidersHorizontal,
  Store,
  Trash2,
  X,
} from "lucide-react";

import { ApiLoadingMark } from "@/components/api-loading-mark";
import { useNavigate } from "react-router-dom";
import { toast } from "sonner";

import { ImagePromptMarket } from "@/app/image/components/image-prompt-market";
import { ConversationEmptyState } from "@/app/image/components/conversation-empty-state";
import { IMAGE_PROMPT_PRESETS, type ImagePromptPreset } from "@/app/image/image-presets";
import { consumeSimilarImageIntent } from "@/app/image/similar-image-intent";
import type { BananaPrompt } from "@/app/image/banana-prompts";
import {
  CUSTOM_IMAGE_ASPECT_RATIO,
  DEFAULT_IMAGE_CUSTOM_RATIO,
  IMAGE_ASPECT_RATIO_OPTIONS,
  getImageSizeSelectionFromSize,
  isImageAspectRatio,
  parseImageRatio,
  type ImageAspectRatio,
} from "@/app/image/image-options";
import { ImageLightbox } from "@/components/image-lightbox";
import { AuthenticatedImage } from "@/components/authenticated-image";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Textarea } from "@/components/ui/textarea";
import {
  cancelExternalImageTask,
  createExternalImageTask,
  deleteExternalImageTasks,
  fetchExternalImageProviders,
  fetchExternalImageTasks,
  retryExternalImageTask,
} from "@/lib/external-api";
import {
  type ExternalImageProvider,
  type ExternalImageTask,
  type ManagedImage,
} from "@/lib/api";
import { fetchAuthenticatedImageBlob } from "@/lib/authenticated-image";
import { clearImageManagerCache } from "@/lib/image-manager-cache";
import { cn } from "@/lib/utils";
import { useHistoryResumeSync } from "@/lib/history-sync";
import { useAuthGuard } from "@/lib/use-auth-guard";
import { hasAPIPermission, type StoredAuthSession } from "@/store/auth";

const ACCEPTED_REFERENCE_TYPES = new Set(["image/png", "image/jpeg", "image/webp"]);
const MAX_REFERENCE_BYTES = 10 * 1024 * 1024;
const PROMPT_AREA_MIN_HEIGHT = 74;
const PROMPT_AREA_DEFAULT_HEIGHT = 104;
const PROMPT_AREA_MAX_HEIGHT = 320;
const EXTERNAL_IMAGE_PREFS_KEY = "chatgpt2api:external_image_prefs";

function promptReferenceImageUrls(prompt: BananaPrompt) {
  const urls = prompt.referenceImageUrls.length > 0 ? prompt.referenceImageUrls : [prompt.preview];
  return Array.from(new Set(urls.map((url) => url.trim()).filter(Boolean)));
}

function promptReferenceFileName(url: string, index: number) {
  const rawName = url.split(/[?#]/, 1)[0].split("/").filter(Boolean).pop() || "";
  let name = rawName;
  try {
    name = rawName ? decodeURIComponent(rawName) : "";
  } catch {
    name = rawName;
  }
  if (!name) return `template-reference-${index + 1}.png`;
  return name.includes(".") ? name : `${name}.png`;
}

const statusMeta: Record<ExternalImageTask["status"], { label: string; className: string }> = {
  queued: { label: "排队中", className: "bg-amber-50 text-amber-700" },
  running: { label: "生成中", className: "bg-blue-50 text-blue-700" },
  success: { label: "已完成", className: "bg-emerald-50 text-emerald-700" },
  error: { label: "失败", className: "bg-rose-50 text-rose-700" },
  cancelled: { label: "已取消", className: "bg-stone-100 text-stone-600" },
};

type ExternalImagePrefs = {
  aspectRatio: ImageAspectRatio;
  customRatio: string;
  count: string;
};

const DEFAULT_PREFS: ExternalImagePrefs = {
  aspectRatio: "",
  customRatio: DEFAULT_IMAGE_CUSTOM_RATIO,
  count: "1",
};

function getStoredPrefs(): ExternalImagePrefs {
  if (typeof window === "undefined") {
    return DEFAULT_PREFS;
  }
  try {
    const raw = window.localStorage.getItem(EXTERNAL_IMAGE_PREFS_KEY);
    if (!raw) {
      return DEFAULT_PREFS;
    }
    const parsed = JSON.parse(raw) as Partial<ExternalImagePrefs>;
    return {
      aspectRatio: isImageAspectRatio(parsed.aspectRatio) ? parsed.aspectRatio : DEFAULT_PREFS.aspectRatio,
      customRatio: parsed.customRatio || DEFAULT_PREFS.customRatio,
      count: typeof parsed.count === "string" ? parsed.count : DEFAULT_PREFS.count,
    };
  } catch {
    return DEFAULT_PREFS;
  }
}

function createTaskId() {
  if (typeof crypto !== "undefined" && "randomUUID" in crypto) return crypto.randomUUID();
  return `external-${Date.now()}-${Math.random().toString(16).slice(2)}`;
}

function getPromptAreaMaxHeight() {
  if (typeof window === "undefined") return PROMPT_AREA_MAX_HEIGHT;
  return Math.max(PROMPT_AREA_MIN_HEIGHT, Math.min(PROMPT_AREA_MAX_HEIGHT, Math.floor(window.innerHeight * 0.42)));
}

function clampPromptAreaHeight(height: number) {
  return Math.min(Math.max(height, PROMPT_AREA_MIN_HEIGHT), getPromptAreaMaxHeight());
}

function hasDraggedImage(dataTransfer: DataTransfer) {
  if (!Array.from(dataTransfer.types).includes("Files")) return false;
  const items = Array.from(dataTransfer.items);
  return items.length === 0 || items.some((item) => item.kind === "file" && (item.type === "" || item.type.startsWith("image/")));
}

function formatTime(value?: string) {
  if (!value) return "--";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return value;
  return new Intl.DateTimeFormat("zh-CN", { month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit" }).format(date);
}

function formatDuration(task: ExternalImageTask) {
  const start = new Date(task.started_at || task.created_at || "").getTime();
  const end = new Date(task.finished_at || task.updated_at || "").getTime();
  if (!Number.isFinite(start) || !Number.isFinite(end) || end < start) return "--";
  const seconds = Math.max(0, Math.round((end - start) / 1000));
  return seconds < 60 ? `${seconds} 秒` : `${Math.floor(seconds / 60)} 分 ${seconds % 60} 秒`;
}

// Images API only accepts its standard square, landscape, and portrait sizes.
function toImagesApiSize(size: string) {
  const trimmed = size.trim();
  if (!trimmed) return "";
  const ratio = parseImageRatio(trimmed);
  if (!ratio) return "";
  const value = ratio.width / ratio.height;
  if (Math.abs(value - 1) <= 0.1) return "1024x1024";
  return value > 1 ? "1536x1024" : "1024x1536";
}

function activeCompositionRatio(aspectRatio: ImageAspectRatio, customRatio: string) {
  if (aspectRatio === CUSTOM_IMAGE_ASPECT_RATIO) return parseImageRatio(customRatio) ? customRatio.trim() : "";
  return aspectRatio;
}

async function downloadImage(item: ManagedImage) {
  const blob = await fetchAuthenticatedImageBlob(item.url);
  const extension = blob.type === "image/jpeg" ? "jpg" : blob.type.split("/")[1] || "png";
  const objectURL = URL.createObjectURL(blob);
  const link = document.createElement("a");
  link.href = objectURL;
  link.download = item.name || `api-image-${Date.now()}.${extension}`;
  document.body.appendChild(link);
  link.click();
  link.remove();
  window.setTimeout(() => URL.revokeObjectURL(objectURL), 1000);
}

function LoadingState() {
  return <div className="flex min-h-[50vh] items-center justify-center"><ApiLoadingMark size="page" label="正在连接生图接口" /></div>;
}

function ExternalTaskConversation({
  task,
  actionBusy,
  canDelete,
  onCancel,
  onRetry,
  onLoadParameters,
  onDelete,
  onOpenLightbox,
  onOpenReference,
}: {
  task: ExternalImageTask;
  actionBusy: boolean;
  canDelete: boolean;
  onCancel: (task: ExternalImageTask) => void;
  onRetry: (task: ExternalImageTask) => void;
  onLoadParameters: (task: ExternalImageTask) => void;
  onDelete: (task: ExternalImageTask) => void;
  onOpenLightbox: (index: number) => void;
  onOpenReference: (index: number) => void;
}) {
  const busy = task.status === "queued" || task.status === "running";
  const images = task.data || [];
  const meta = statusMeta[task.status];
  const resultCount = images.length || (busy ? task.n : 0);

  return (
    <div className="mx-auto flex w-full max-w-[980px] flex-col gap-3 sm:gap-4">
      <div className="flex justify-end">
        <article className="w-full max-w-[min(94%,760px)] rounded-[24px] border border-[#f2f3f5] bg-white px-4 py-3 text-left text-[14px] leading-6 text-[#222222] shadow-[0_4px_6px_rgba(0,0,0,0.08)] dark:border-border dark:bg-card dark:text-foreground sm:px-5 sm:py-4 sm:text-[15px] sm:leading-7">
          <div className="mb-3 flex items-start justify-between gap-3 border-b border-[#f2f3f5] pb-2 dark:border-border">
            <div className="flex min-w-0 flex-wrap items-center gap-1.5 text-[11px] leading-5 text-[#45515e] dark:text-muted-foreground">
              <span className="rounded-full bg-[#f0f0f0] px-2.5 py-0.5 dark:bg-muted">API 生图</span>
              <span className="rounded-full bg-[#eef4ff] px-2.5 py-0.5 text-[#1456f0] dark:bg-sky-950/40 dark:text-sky-300">{task.provider_name}</span>
              <span className="rounded-full bg-[#f0f0f0] px-2.5 py-0.5 dark:bg-muted">{task.model}</span>
              <span className={cn("rounded-full px-2.5 py-0.5", meta.className)}>{meta.label}</span>
              <span className="px-1 text-[#8e8e93]">{formatTime(task.created_at)}</span>
            </div>
            {busy ? (
              <Button
                type="button"
                variant="outline"
                size="icon"
                className="size-8 shrink-0 rounded-full border-amber-200 bg-amber-50 text-amber-700 shadow-none hover:bg-amber-100"
                onClick={() => onCancel(task)}
                aria-label="终止生成任务"
                title="终止"
              >
                <CircleStop className="size-4" />
              </Button>
            ) : (
              <div className="flex shrink-0 items-center gap-1">
                <Button type="button" variant="outline" size="icon" className="size-8 rounded-full border-[#e5e7eb] bg-white text-[#45515e] shadow-none" onClick={() => onLoadParameters(task)} disabled={actionBusy} title="载入参数">
                  <SlidersHorizontal className="size-4" />
                </Button>
                {task.status === "error" || task.status === "cancelled" ? (
                  <Button type="button" variant="outline" size="icon" className="size-8 rounded-full border-[#e5e7eb] bg-white text-[#45515e] shadow-none" onClick={() => onRetry(task)} disabled={actionBusy} title="重新生成">
                    <RotateCcw className="size-4" />
                  </Button>
                ) : null}
                {canDelete ? (
                  <Button type="button" variant="outline" size="icon" className="size-8 rounded-full border-[#e5e7eb] bg-white text-[#45515e] shadow-none hover:bg-black/[0.05]" onClick={() => onDelete(task)} disabled={actionBusy} title="删除记录">
                    <Trash2 className="size-4" />
                  </Button>
                ) : null}
              </div>
            )}
          </div>
          <div className="whitespace-pre-wrap break-words">{task.prompt}</div>
          {task.references && task.references.length > 0 ? (
            <div className="mt-3 flex flex-wrap justify-start gap-2">
              {task.references.map((reference, index) => (
                <button key={`${task.id}-reference-${reference.index}`} type="button" onClick={() => onOpenReference(index)} className="group relative size-20 shrink-0 overflow-hidden rounded-2xl border border-stone-200/80 bg-stone-100/60 text-left transition hover:border-stone-300 sm:size-24" title={reference.name}>
                  <AuthenticatedImage src={reference.url} alt={reference.name} className="absolute inset-0 size-full object-cover transition duration-200 group-hover:scale-[1.02]" />
                </button>
              ))}
            </div>
          ) : null}
        </article>
      </div>

      <div className="flex justify-start">
        <section className="w-full px-1">
          <div className="mb-3 flex flex-wrap items-center justify-between gap-2 sm:mb-4">
            <div className="flex flex-wrap items-center gap-1.5 text-[11px] text-[#45515e] dark:text-muted-foreground sm:gap-2 sm:text-xs">
              <span className="font-medium text-[#222222] dark:text-foreground">生成结果</span>
              <span className="rounded-full bg-[#f0f0f0] px-3 py-1 dark:bg-muted">{resultCount} 张</span>
              {task.n !== resultCount ? <span className="rounded-full bg-[#f0f0f0] px-3 py-1 dark:bg-muted">目标 {task.n} 张</span> : null}
              {task.aspect_ratio ? <span className="rounded-full bg-[#f0f0f0] px-3 py-1 dark:bg-muted">构图 {task.aspect_ratio}</span> : null}
              <span className={cn("rounded-full px-3 py-1", meta.className)}>{meta.label}</span>
              <span className="rounded-full bg-[#f0f0f0] px-3 py-1 dark:bg-muted">耗时 {formatDuration(task)}</span>
            </div>
            {busy ? (
              <span className="flex items-center gap-2 rounded-2xl bg-amber-50 px-3 py-1 text-[11px] font-medium leading-5 text-amber-700 sm:text-xs">
                <ApiLoadingMark size="inline" />
                {task.status === "queued" ? "等待渠道并发额度" : `正在通过 ${task.provider_name} 生成图片`}
              </span>
            ) : null}
          </div>

          {task.error ? (
            <div className="mb-3 rounded-[20px] border border-rose-200 bg-rose-50 px-4 py-3 text-sm leading-6 text-rose-700 dark:border-rose-900/60 dark:bg-rose-950/30 dark:text-rose-200">
              {task.error}
            </div>
          ) : null}

          {busy && images.length === 0 ? (
            <div className="columns-1 gap-3 sm:columns-2 sm:gap-4 xl:columns-3">
              {Array.from({ length: task.n }, (_, index) => (
                <div key={`${task.id}-loading-${index}`} className="mb-3 inline-flex aspect-square w-full break-inside-avoid flex-col items-center justify-center gap-3 overflow-hidden rounded-[22px] bg-[#f0f0f0] text-[#45515e] dark:bg-muted dark:text-muted-foreground sm:mb-4">
                  <ApiLoadingMark size="media" label={task.status === "queued" ? "排队等待" : "正在生成"} />
                  <span className="text-xs">{task.status === "queued" ? "排队等待" : "正在生成"}</span>
                </div>
              ))}
            </div>
          ) : null}

          {images.length > 0 ? (
            <div className="columns-1 gap-3 sm:columns-2 sm:gap-4 xl:columns-3">
              {images.map((item, index) => (
                <figure
                  key={item.path || `${task.id}-${index}`}
                  className="group relative mb-3 inline-block w-full break-inside-avoid overflow-hidden rounded-[22px] bg-[#f0f0f0] shadow-[0_0_15px_rgba(44,30,116,0.16)] sm:mb-4"
                >
                  <button type="button" className="block w-full cursor-pointer overflow-hidden text-left" onClick={() => onOpenLightbox(index)}>
                    <AuthenticatedImage src={item.url} alt={`${task.prompt} ${index + 1}`} className="block max-h-[760px] w-full object-contain" />
                  </button>
                  <div className="pointer-events-none absolute top-2 right-2 z-10 flex items-center gap-1 opacity-0 transition duration-150 group-hover:pointer-events-auto group-hover:opacity-100 group-focus-within:pointer-events-auto group-focus-within:opacity-100">
                    <button
                      type="button"
                      onClick={() => onOpenLightbox(index)}
                      className="inline-flex h-7 items-center gap-1 rounded-full bg-white/95 px-2 text-[11px] font-medium text-stone-800 shadow-sm transition hover:bg-white hover:text-stone-950"
                      title="查看原图"
                    >
                      <Eye className="size-3" />
                      查看原图
                    </button>
                    <button
                      type="button"
                      onClick={() => void downloadImage(item).catch((error) => toast.error(error instanceof Error ? error.message : "下载失败"))}
                      className="inline-flex size-7 items-center justify-center rounded-full bg-white/95 text-stone-800 shadow-sm transition hover:bg-white hover:text-stone-950"
                      title="下载"
                    >
                      <Download className="size-3.5" />
                    </button>
                  </div>
                  <div className="pointer-events-none absolute inset-x-0 bottom-0 bg-gradient-to-t from-black/55 via-black/20 to-transparent px-3 pt-10 pb-2.5 opacity-0 transition duration-150 group-hover:opacity-100 group-focus-within:opacity-100">
                    <div className="truncate text-[11px] font-medium text-white drop-shadow-sm">已保存到图片库</div>
                  </div>
                </figure>
              ))}
            </div>
          ) : null}

          {!busy && !task.error && images.length === 0 ? (
            <div className="rounded-[20px] border border-[#f2f3f5] bg-white px-4 py-6 text-center text-sm text-[#8e8e93] dark:border-border dark:bg-card dark:text-muted-foreground">
              此任务没有返回可保存的图片。
            </div>
          ) : null}
        </section>
      </div>
    </div>
  );
}

const paramFieldClass =
  "flex min-h-8 min-w-0 items-center justify-between gap-2 rounded-xl border border-[#e5e7eb] bg-white px-3 py-1 text-[11px] dark:border-border dark:bg-background/70";

function ParamMenu<Value extends string>({
  label,
  value,
  valueLabel,
  options,
  open,
  onOpenChange,
  onValueChange,
  align = "end",
}: {
  label: string;
  value: Value;
  valueLabel: string;
  options: ReadonlyArray<{ value: Value; label: string; description?: string }>;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onValueChange: (value: Value) => void;
  align?: "start" | "center" | "end";
}) {
  return (
    <Popover open={open} onOpenChange={onOpenChange}>
      <PopoverTrigger asChild>
        <button
          type="button"
          className="flex h-7 min-w-0 flex-1 items-center justify-end gap-1 bg-transparent text-right text-xs font-semibold text-[#18181b] dark:text-foreground"
          aria-label={`选择${label}，当前 ${valueLabel}`}
          aria-expanded={open}
        >
          <span className="truncate">{valueLabel}</span>
          <ChevronDown className={cn("size-4 shrink-0 opacity-60 transition", open && "rotate-180")} />
        </button>
      </PopoverTrigger>
      <PopoverContent
        align={align}
        side="top"
        sideOffset={8}
        collisionPadding={12}
        className="z-[120] max-h-[min(var(--radix-popover-content-available-height),14rem)] w-[min(24rem,calc(100vw-2rem))] overflow-x-hidden overflow-y-auto overscroll-contain rounded-[16px] border-[#e5e7eb] bg-white p-1.5 shadow-[0_18px_46px_-26px_rgba(15,23,42,0.35)] dark:border-border dark:bg-card"
        onOpenAutoFocus={(event) => event.preventDefault()}
      >
        <div className="grid gap-1" role="listbox" aria-label={label}>
          {options.map((option) => {
            const active = option.value === value;
            return (
              <button
                key={`${label}-${option.value || option.label}`}
                type="button"
                role="option"
                aria-selected={active}
                className={cn(
                  "flex w-full max-w-full items-start justify-between gap-3 rounded-lg px-3 py-2 text-left text-sm text-[#45515e] transition hover:bg-black/[0.05] dark:text-muted-foreground dark:hover:bg-accent/60",
                  active && "bg-black/[0.05] font-medium text-[#18181b] dark:bg-accent dark:text-foreground",
                )}
                title={option.description}
                onClick={() => {
                  onValueChange(option.value);
                  onOpenChange(false);
                }}
              >
                <span className="min-w-0 max-w-full">
                  <span className="block whitespace-normal break-words">{option.label}</span>
                  {option.description ? (
                    <span className="block whitespace-normal break-words text-[11px] font-normal text-[#8e8e93] dark:text-muted-foreground">
                      {option.description}
                    </span>
                  ) : null}
                </span>
                {active ? <Check className="mt-0.5 size-4 shrink-0" /> : null}
              </button>
            );
          })}
        </div>
      </PopoverContent>
    </Popover>
  );
}

function ExternalImagePageContent({ session }: { session: StoredAuthSession }) {
  const navigate = useNavigate();
  const fileInputRef = useRef<HTMLInputElement>(null);
  const textareaRef = useRef<HTMLTextAreaElement>(null);
  const composerDockRef = useRef<HTMLDivElement>(null);
  const composerPanelRef = useRef<HTMLDivElement>(null);
  const composerToolbarRef = useRef<HTMLDivElement>(null);
  const promptResizeRef = useRef<{ offset: number } | null>(null);
  const referenceDragDepthRef = useRef(0);
  const canSubmit = hasAPIPermission(session, "POST", "/api/external-image-tasks");
  const canDelete = hasAPIPermission(session, "DELETE", "/api/external-image-tasks");

  const [providers, setProviders] = useState<ExternalImageProvider[]>([]);
  const [tasks, setTasks] = useState<ExternalImageTask[]>([]);
  const [selectedTaskId, setSelectedTaskId] = useState<string | null>(null);
  const [isLoading, setIsLoading] = useState(true);
  const [isRefreshing, setIsRefreshing] = useState(false);
  const [isSubmitting, setIsSubmitting] = useState(false);
  const [busyTaskId, setBusyTaskId] = useState("");
  const [deleteConfirm, setDeleteConfirm] = useState<{ type: "one"; ids: string[] } | { type: "failed"; ids: string[] } | null>(null);
  const [providerId, setProviderId] = useState("");
  const [model, setModel] = useState("");
  const [prompt, setPrompt] = useState("");
  const [count, setCount] = useState(() => getStoredPrefs().count);
  const [references, setReferences] = useState<File[]>([]);
  const [previewImage, setPreviewImage] = useState<ManagedImage | null>(null);
  const [promptMarketOpen, setPromptMarketOpen] = useState(false);
  const [isParamsOpen, setIsParamsOpen] = useState(false);
  const [isModelMenuOpen, setIsModelMenuOpen] = useState(false);
  const [isAspectRatioMenuOpen, setIsAspectRatioMenuOpen] = useState(false);
  const [lightboxOpen, setLightboxOpen] = useState(false);
  const [lightboxIndex, setLightboxIndex] = useState(0);
  const [referenceLightboxOpen, setReferenceLightboxOpen] = useState(false);
  const [referenceLightboxIndex, setReferenceLightboxIndex] = useState(0);
  const [taskReferenceLightboxOpen, setTaskReferenceLightboxOpen] = useState(false);
  const [taskReferenceLightboxIndex, setTaskReferenceLightboxIndex] = useState(0);
  const [promptAreaHeight, setPromptAreaHeight] = useState(PROMPT_AREA_DEFAULT_HEIGHT);
  const [composerDockHeight, setComposerDockHeight] = useState(0);
  const [isReferenceDragActive, setIsReferenceDragActive] = useState(false);

  const initialPrefs = getStoredPrefs();
  const [aspectRatio, setAspectRatio] = useState<ImageAspectRatio>(initialPrefs.aspectRatio);
  const [customRatio, setCustomRatio] = useState(initialPrefs.customRatio);

  const selectedProvider = useMemo(() => providers.find((provider) => provider.id === providerId) ?? null, [providerId, providers]);
  const selectedTask = useMemo(() => tasks.find((task) => task.id === selectedTaskId) ?? null, [selectedTaskId, tasks]);
  const activeTasks = useMemo(() => tasks.some((task) => task.status === "queued" || task.status === "running"), [tasks]);
  const failedTaskIds = useMemo(() => tasks.filter((task) => task.status === "error" || task.status === "cancelled").map((task) => task.id), [tasks]);
  const supportsReferences = selectedProvider?.protocol === "openai_chat_images" || selectedProvider?.protocol === "openai_image_edits";
  const requiresReferences = selectedProvider?.protocol === "openai_image_edits";
  const providerMaxImages = selectedProvider?.max_images ?? 4;
  const providerMaxReferences = selectedProvider?.max_reference_images ?? 4;

  const compositionRatio = useMemo(() => activeCompositionRatio(aspectRatio, customRatio), [aspectRatio, customRatio]);
  const imagesApiSize = useMemo(() => toImagesApiSize(compositionRatio), [compositionRatio]);
  const isCustomRatioInvalid = aspectRatio === CUSTOM_IMAGE_ASPECT_RATIO && !parseImageRatio(customRatio);

  const aspectRatioLabel = aspectRatio === CUSTOM_IMAGE_ASPECT_RATIO
    ? "自定义比例"
    : IMAGE_ASPECT_RATIO_OPTIONS.find((option) => option.value === aspectRatio)?.label || "Auto";
  const parsedCount = Math.max(1, Math.min(providerMaxImages, Number(count) || 1));
  const referenceLightboxItems = useMemo(
    () => references.map((file, index) => ({ id: `${file.name}-${file.lastModified}-${index}`, src: URL.createObjectURL(file), fileName: file.name })),
    [references],
  );
  const taskReferenceLightboxItems = useMemo(
    () => (selectedTask?.references || []).map((reference) => ({ id: `${selectedTask?.id}-${reference.index}`, src: reference.url, fileName: reference.name })),
    [selectedTask],
  );

  useEffect(() => () => referenceLightboxItems.forEach((item) => URL.revokeObjectURL(item.src)), [referenceLightboxItems]);
  useEffect(() => {
    const handleResize = () => setPromptAreaHeight((height) => clampPromptAreaHeight(height));
    window.addEventListener("resize", handleResize);
    return () => window.removeEventListener("resize", handleResize);
  }, []);
  useEffect(() => {
    const dock = composerDockRef.current;
    if (!dock) return;
    const updateHeight = () => setComposerDockHeight(dock.getBoundingClientRect().height);
    updateHeight();
    const observer = new ResizeObserver(updateHeight);
    observer.observe(dock);
    window.addEventListener("resize", updateHeight);
    return () => {
      observer.disconnect();
      window.removeEventListener("resize", updateHeight);
    };
  }, [providers.length, references.length]);

  const selectProvider = useCallback((items: ExternalImageProvider[], preferredId?: string) => {
    const next = items.find((item) => item.id === preferredId) ?? items[0];
    setProviderId(next?.id || "");
    setModel(next?.default_model || next?.models[0] || "");
    setCount((current) => String(Math.max(1, Math.min(next?.max_images ?? 4, Number(current) || 1))));
    if (next?.protocol === "openai_images") {
      setReferences([]);
      if (fileInputRef.current) fileInputRef.current.value = "";
    }
    return next;
  }, []);

  const applySimilarIntent = useCallback(async (items: ExternalImageProvider[]) => {
    const intent = consumeSimilarImageIntent();
    if (!intent) return;
    const preferred = intent.providerId ? items.find((item) => item.id === intent.providerId) : undefined;
    const provider = selectProvider(items, preferred?.id);
    if (!preferred && intent.providerId) toast.warning(`原渠道“${intent.providerName || intent.providerId}”已不可用，已选择其他渠道`);
    setPrompt(intent.prompt);
    if (provider && intent.model && provider.models.includes(intent.model)) setModel(intent.model);
    if (intent.requestedSize) {
      const selection = getImageSizeSelectionFromSize(intent.requestedSize);
      setAspectRatio(selection.aspectRatio);
      setCustomRatio(selection.customRatio);
    }
    if (provider?.protocol === "openai_images") {
      toast.info("当前渠道不支持参考图，已带入提示词和生成参数");
      return;
    }
    const urls = intent.sourceImageUrls.slice(0, provider.max_reference_images ?? 4);
    const loaded = await Promise.allSettled(urls.map(async (url, index) => {
      const blob = await fetchAuthenticatedImageBlob(url);
      return new File([blob], intent.sourceImageName || `reference-${index + 1}.png`, { type: blob.type || "image/png" });
    }));
    const files = loaded.flatMap((result) => result.status === "fulfilled" ? [result.value] : []);
    setReferences(files);
    if (files.length < urls.length) toast.warning("部分参考图读取失败，已带入可用图片");
  }, [selectProvider]);

  const load = useCallback(async (quiet = false) => {
    if (!quiet) setIsRefreshing(true);
    try {
      const [providerItems, taskItems] = await Promise.all([fetchExternalImageProviders(), fetchExternalImageTasks()]);
      setProviders(providerItems);
      setTasks(taskItems);
      setSelectedTaskId((current) => current && taskItems.some((task) => task.id === current) ? current : taskItems[0]?.id || null);
      if (!quiet) {
        selectProvider(providerItems, providerId);
        await applySimilarIntent(providerItems);
      }
    } catch (error) {
      if (!quiet) toast.error(error instanceof Error ? error.message : "加载 API 生图数据失败");
    } finally {
      setIsLoading(false);
      setIsRefreshing(false);
    }
  }, [applySimilarIntent, providerId, selectProvider]);

  useEffect(() => { void load(); }, []); // eslint-disable-line react-hooks/exhaustive-deps

  useHistoryResumeSync(() => load(true), !isLoading);

  useEffect(() => {
    if (!activeTasks) return;
    const timer = window.setInterval(() => {
      void fetchExternalImageTasks().then((items) => {
        setTasks(items);
        if (items.some((task) => task.status === "success")) clearImageManagerCache();
      }).catch(() => {});
    }, 2000);
    return () => window.clearInterval(timer);
  }, [activeTasks]);

  useEffect(() => {
    if (typeof window === "undefined") return;
    const prefs: ExternalImagePrefs = { aspectRatio, customRatio, count };
    window.localStorage.setItem(EXTERNAL_IMAGE_PREFS_KEY, JSON.stringify(prefs));
  }, [aspectRatio, customRatio, count]);

  const handleProviderChange = (nextId: string) => {
    const next = providers.find((item) => item.id === nextId);
    setProviderId(nextId);
    setModel(next?.default_model || next?.models[0] || "");
    setCount((current) => String(Math.max(1, Math.min(next?.max_images ?? 4, Number(current) || 1))));
    if (next?.protocol === "openai_images") {
      setReferences([]);
      if (fileInputRef.current) fileInputRef.current.value = "";
    }
  };

  const handleReferenceFiles = (files: File[] | FileList | null) => {
    const next = [...references, ...Array.from(files || [])];
    if (next.length > providerMaxReferences) return toast.error(`参考图最多 ${providerMaxReferences} 张`);
    for (const file of next) {
      if (!ACCEPTED_REFERENCE_TYPES.has(file.type)) return toast.error(`${file.name} 不是 PNG、JPEG 或 WebP`);
      if (file.size > MAX_REFERENCE_BYTES) return toast.error(`${file.name} 超过 10 MB`);
    }
    setReferences(next);
  };

  const handlePromptPaste = (event: ClipboardEvent<HTMLTextAreaElement>) => {
    if (!supportsReferences) return;
    const files = Array.from(event.clipboardData.files).filter((file) => file.type.startsWith("image/"));
    if (files.length === 0) return;
    event.preventDefault();
    handleReferenceFiles(files);
  };

  const handleReferenceDragEnter = (event: DragEvent<HTMLDivElement>) => {
    if (!supportsReferences || !hasDraggedImage(event.dataTransfer)) return;
    event.preventDefault();
    referenceDragDepthRef.current += 1;
    setIsReferenceDragActive(true);
  };

  const handleReferenceDragLeave = (event: DragEvent<HTMLDivElement>) => {
    if (!supportsReferences) return;
    event.preventDefault();
    referenceDragDepthRef.current = Math.max(0, referenceDragDepthRef.current - 1);
    if (referenceDragDepthRef.current === 0) setIsReferenceDragActive(false);
  };

  const handleReferenceDrop = (event: DragEvent<HTMLDivElement>) => {
    if (!supportsReferences || !hasDraggedImage(event.dataTransfer)) return;
    event.preventDefault();
    referenceDragDepthRef.current = 0;
    setIsReferenceDragActive(false);
    handleReferenceFiles(Array.from(event.dataTransfer.files));
  };

  const handlePromptResizeStart = (event: PointerEvent<HTMLButtonElement>) => {
    event.preventDefault();
    promptResizeRef.current = { offset: event.clientY - event.currentTarget.getBoundingClientRect().top };
    event.currentTarget.setPointerCapture(event.pointerId);
  };

  const handlePromptResizeMove = (event: PointerEvent<HTMLButtonElement>) => {
    if (!promptResizeRef.current || !composerPanelRef.current) return;
    event.preventDefault();
    const panel = composerPanelRef.current.getBoundingClientRect();
    const toolbarHeight = composerToolbarRef.current?.getBoundingClientRect().height ?? 0;
    setPromptAreaHeight(clampPromptAreaHeight(panel.bottom - toolbarHeight - event.clientY + promptResizeRef.current.offset));
  };

  const handlePromptResizeEnd = (event: PointerEvent<HTMLButtonElement>) => {
    promptResizeRef.current = null;
    if (event.currentTarget.hasPointerCapture(event.pointerId)) event.currentTarget.releasePointerCapture(event.pointerId);
  };

  const handlePromptResizeKeyDown = (event: KeyboardEvent<HTMLButtonElement>) => {
    if (event.key !== "ArrowUp" && event.key !== "ArrowDown") return;
    event.preventDefault();
    setPromptAreaHeight((height) => clampPromptAreaHeight(height + (event.key === "ArrowUp" ? 16 : -16)));
  };

  const submit = async () => {
    if (!canSubmit) return toast.error("没有提交外部生图任务的权限");
    if (!selectedProvider) return toast.error("请先选择可用渠道");
    if (!prompt.trim()) return toast.error("请输入提示词");
    if (isCustomRatioInvalid) return toast.error("请输入有效的构图比例");
    if (requiresReferences && references.length === 0) return toast.error("Images Edits 渠道至少需要一张参考图");
    setIsSubmitting(true);
    try {
      const task = await createExternalImageTask({
        clientTaskId: createTaskId(),
        providerId: selectedProvider.id,
        model,
        prompt: prompt.trim(),
        count: parsedCount,
        ...(compositionRatio ? { aspectRatio: compositionRatio } : {}),
        ...(selectedProvider.protocol !== "openai_chat_images" && imagesApiSize ? { size: imagesApiSize } : {}),
        ...(supportsReferences ? { references } : {}),
      });
      setTasks((current) => [task, ...current.filter((item) => item.id !== task.id)]);
      setSelectedTaskId(task.id);
      toast.success("任务已提交");
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "提交任务失败");
    } finally {
      setIsSubmitting(false);
    }
  };

  const cancel = async (task: ExternalImageTask) => {
    try {
      const next = await cancelExternalImageTask(task.id);
      setTasks((current) => current.map((item) => item.id === next.id ? next : item));
      toast.success("任务已取消");
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "取消任务失败");
    }
  };

  const retryTask = async (task: ExternalImageTask) => {
    setBusyTaskId(task.id);
    try {
      const next = await retryExternalImageTask(task.id);
      setTasks((current) => [next, ...current.filter((item) => item.id !== next.id)]);
      setSelectedTaskId(next.id);
      toast.success("已创建新的重试任务");
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "重试任务失败");
    } finally {
      setBusyTaskId("");
    }
  };

  const loadTaskParameters = async (task: ExternalImageTask) => {
    const provider = providers.find((item) => item.id === task.provider_id);
    if (!provider) {
      toast.error("原渠道已不存在，无法完整载入参数");
      return;
    }
    handleProviderChange(provider.id);
    setModel(provider.models.includes(task.model) ? task.model : provider.default_model);
    setPrompt(task.prompt);
    setCount(String(Math.max(1, Math.min(provider.max_images ?? 4, task.n || 1))));
    if (task.aspect_ratio) {
      const selection = getImageSizeSelectionFromSize(task.aspect_ratio);
      setAspectRatio(selection.aspectRatio || CUSTOM_IMAGE_ASPECT_RATIO);
      setCustomRatio(selection.aspectRatio ? selection.customRatio : task.aspect_ratio);
    } else {
      const selection = getImageSizeSelectionFromSize(task.size || "auto");
      setAspectRatio(selection.aspectRatio);
      setCustomRatio(selection.customRatio);
    }
    if (provider.protocol !== "openai_images" && task.references?.length) {
      const loaded = await Promise.allSettled(task.references.map(async (reference) => {
        const blob = await fetchAuthenticatedImageBlob(reference.url);
        return new File([blob], reference.name, { type: blob.type || reference.content_type || "image/png" });
      }));
      const files = loaded.flatMap((result) => result.status === "fulfilled" ? [result.value] : []);
      setReferences(files);
      if (files.length < task.references.length) toast.warning("部分参考图已失效，请重新上传");
    }
    setSelectedTaskId(null);
    window.setTimeout(() => textareaRef.current?.focus(), 0);
    toast.success("任务参数已载入编辑器");
  };

  const deleteTasks = async (ids: string[]) => {
    if (!canDelete) return toast.error("没有删除 API 生图记录的权限");
    if (ids.length === 0) return;
    setBusyTaskId(ids.length === 1 ? ids[0] : "batch");
    try {
      const next = await deleteExternalImageTasks(ids);
      setTasks(next);
      if (selectedTaskId && ids.includes(selectedTaskId)) setSelectedTaskId(null);
      toast.success(ids.length === 1 ? "任务记录已删除" : `已删除 ${ids.length} 条任务记录`);
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "删除任务记录失败");
    } finally {
      setBusyTaskId("");
    }
  };

  const openDeleteConfirm = (ids: string[], type: "one" | "failed") => {
    if (!canDelete) {
      toast.error("没有删除 API 生图记录的权限");
      return;
    }
    if (ids.length > 0) setDeleteConfirm({ type, ids });
  };

  const handleConfirmDelete = async () => {
    const target = deleteConfirm;
    setDeleteConfirm(null);
    if (target) await deleteTasks(target.ids);
  };

  const deleteConfirmTitle = deleteConfirm?.type === "failed" ? "清理失败记录" : deleteConfirm?.type === "one" ? "删除任务记录" : "";
  const deleteConfirmDescription = deleteConfirm?.type === "failed"
    ? `确认删除 ${deleteConfirm.ids.length} 条失败或已取消的 API 生图记录吗？删除后无法恢复，已生成的图片仍保留在图片库。`
    : deleteConfirm?.type === "one"
      ? "确认删除这条 API 生图任务记录吗？删除后无法恢复，已生成的图片仍保留在图片库。"
      : "";

  const applyTemplateReferences = async (referenceUrls: string[]) => {
    setReferences([]);
    if (fileInputRef.current) fileInputRef.current.value = "";
    if (referenceUrls.length === 0) return false;
    let targetProvider = selectedProvider;
    const targetSupportsReferences = targetProvider?.protocol === "openai_chat_images" || targetProvider?.protocol === "openai_image_edits";
    if (!targetSupportsReferences) {
      targetProvider = providers.find((provider) => provider.protocol === "openai_chat_images" || provider.protocol === "openai_image_edits") ?? null;
      if (targetProvider) {
        handleProviderChange(targetProvider.id);
      }
    }
    if (!targetProvider || (targetProvider.protocol !== "openai_chat_images" && targetProvider.protocol !== "openai_image_edits")) {
      toast.warning("当前没有支持参考图的 API 渠道，已套用提示词");
      return false;
    }

    const urls = referenceUrls.slice(0, targetProvider.max_reference_images ?? 4);
    const toastId = toast.loading(`正在读取 ${urls.length} 张模板参考图`);
    const results = await Promise.allSettled(urls.map(async (url, index) => {
      const blob = await fetchAuthenticatedImageBlob(url);
      if (blob.size > MAX_REFERENCE_BYTES) throw new Error("参考图超过 10 MB");
      const type = blob.type || "image/png";
      if (!ACCEPTED_REFERENCE_TYPES.has(type)) throw new Error("模板图片格式不受支持");
      return new File([blob], promptReferenceFileName(url, index), { type });
    }));
    toast.dismiss(toastId);
    const files = results.flatMap((result) => result.status === "fulfilled" ? [result.value] : []);
    setReferences(files);
    if (files.length === urls.length) {
      toast.success(`已套用提示词和 ${files.length} 张参考图`);
    } else if (files.length > 0) {
      toast.warning(`已套用提示词和 ${files.length} 张参考图，部分图片读取失败`);
    } else {
      toast.error("已套用提示词，但模板参考图读取失败");
    }
    return files.length > 0;
  };

  const applyPreset = async (preset: ImagePromptPreset) => {
    setSelectedTaskId(null);
    setPrompt(preset.prompt);
    setCount(String(Math.max(1, Math.min(providerMaxImages, preset.count))));
    const selection = getImageSizeSelectionFromSize(preset.size);
    setAspectRatio(selection.aspectRatio);
    setCustomRatio(selection.customRatio);
    textareaRef.current?.focus();
    await applyTemplateReferences([preset.imageSrc]);
  };

  const applyMarketPrompt = async (item: BananaPrompt) => {
    setSelectedTaskId(null);
    setPrompt(item.prompt);
    const referenceUrls = promptReferenceImageUrls(item);
    setPromptMarketOpen(false);
    textareaRef.current?.focus();
    if (referenceUrls.length === 0) {
      toast.success("已套用提示词");
      return;
    }
    await applyTemplateReferences(referenceUrls);
  };

  const lightboxItems = useMemo(
    () =>
      (selectedTask?.data || []).map((item, index) => ({
        id: item.path || `${selectedTask?.id}-${index}`,
        src: item.url,
        fileName: item.name,
      })),
    [selectedTask],
  );

  const openLightbox = (index: number) => {
    if (lightboxItems.length === 0) return;
    setLightboxIndex(Math.max(0, Math.min(index, lightboxItems.length - 1)));
    setLightboxOpen(true);
  };

  if (isLoading) return <LoadingState />;

  return (
    <section className="mx-auto grid h-[calc(100dvh-6.25rem)] min-h-0 w-full max-w-[1380px] grid-cols-1 gap-2 px-0 pb-[calc(env(safe-area-inset-bottom)+0.5rem)] sm:h-[calc(100dvh-5rem)] sm:gap-3 sm:px-3 sm:pb-6 lg:grid-cols-[240px_minmax(0,1fr)]">
      <aside className="hidden h-full min-h-0 overflow-hidden border-r border-[#f2f3f5] pr-3 lg:flex lg:flex-col dark:border-border">
        <div className="mb-3 flex items-center gap-2">
          <Button className="h-10 flex-1 rounded-full" onClick={() => setSelectedTaskId(null)}><Plus className="size-4" />新建任务</Button>
          {canDelete ? <Button size="icon" variant="outline" className="size-10 rounded-full border-[#e5e7eb] bg-white text-[#45515e] hover:bg-black/[0.05]" onClick={() => openDeleteConfirm(failedTaskIds, "failed")} disabled={failedTaskIds.length === 0 || busyTaskId === "batch"} title="清理失败记录"><Trash2 className="size-4" /></Button> : null}
          <Button size="icon" variant="outline" className="size-10 rounded-full" onClick={() => void load()} disabled={isRefreshing} title="刷新任务"><RefreshCw className={cn("size-4", isRefreshing && "animate-spin")} /></Button>
        </div>
        <div className="min-h-0 flex-1 space-y-2 overflow-y-auto pr-1 [scrollbar-color:rgba(142,142,147,.45)_transparent] [scrollbar-width:thin] [&::-webkit-scrollbar]:w-1.5 [&::-webkit-scrollbar-thumb]:rounded-full [&::-webkit-scrollbar-thumb]:bg-[#8e8e93]/45 [&::-webkit-scrollbar-track]:bg-transparent">
          {tasks.length === 0 ? (
            <div className="px-2 py-8 text-center text-sm text-muted-foreground">还没有 API 生图任务，输入提示词后会在这里显示。</div>
          ) : tasks.map((task) => {
            const active = task.id === selectedTaskId;
            const meta = statusMeta[task.status];
            return (
              <div
                key={`${task.owner_id || "mine"}-${task.id}`}
                className={cn(
                  "group relative w-full rounded-[16px] border px-3 py-3 text-left transition",
                  active
                    ? "border-[#f2f3f5] bg-white text-[#18181b] shadow-[0_4px_6px_rgba(0,0,0,0.08)]"
                    : "border-transparent text-[#45515e] hover:border-[#f2f3f5] hover:bg-white",
                )}
              >
                <button type="button" onClick={() => setSelectedTaskId(task.id)} className="block w-full pr-8 text-left">
                  <div className="truncate text-sm font-semibold">{task.prompt}</div>
                  <div className="mt-1 flex items-center justify-between gap-2 text-xs text-muted-foreground"><span className="truncate">{task.provider_name}</span><span>{formatTime(task.updated_at)}</span></div>
                  <span className={cn("mt-2 inline-flex rounded-full px-2 py-0.5 text-[11px] font-medium", meta.className)}>{meta.label}</span>
                </button>
                {canDelete && task.status !== "queued" && task.status !== "running" ? (
                  <button type="button" onClick={() => openDeleteConfirm([task.id], "one")} disabled={busyTaskId === task.id} className="absolute top-3 right-2 inline-flex size-7 items-center justify-center rounded-md text-stone-400 opacity-0 transition hover:bg-stone-100 hover:text-[#45515e] group-hover:opacity-100 disabled:opacity-50" aria-label="删除任务记录" title="删除任务记录"><Trash2 className="size-4" /></button>
                ) : null}
              </div>
            );
          })}
        </div>
      </aside>

      <div className="relative flex min-h-0 flex-col gap-3">
        <div className="flex items-center gap-2 px-1 lg:hidden">
          <Select value={selectedTaskId || "new"} onValueChange={(value) => setSelectedTaskId(value === "new" ? null : value)}>
            <SelectTrigger className="h-10 flex-1 rounded-full"><SelectValue placeholder="任务历史" /></SelectTrigger>
            <SelectContent><SelectItem value="new">新建任务</SelectItem>{tasks.map((task) => <SelectItem key={task.id} value={task.id}>{task.prompt.slice(0, 24)}</SelectItem>)}</SelectContent>
          </Select>
          <Button size="icon" variant="outline" className="size-10 rounded-full" onClick={() => void load()}><RefreshCw className={cn("size-4", isRefreshing && "animate-spin")} /></Button>
        </div>

        <div
          className="hide-scrollbar min-h-0 flex-1 overflow-y-auto px-1 pt-2 pb-[14rem] sm:px-4 sm:pt-4 sm:pb-[15rem]"
          style={composerDockHeight > 0 ? { paddingBottom: composerDockHeight + 24 } : undefined}
        >
          {selectedTask ? (
            <ExternalTaskConversation
              task={selectedTask}
              actionBusy={busyTaskId === selectedTask.id}
              canDelete={canDelete}
              onCancel={(task) => void cancel(task)}
              onRetry={(task) => void retryTask(task)}
              onLoadParameters={(task) => void loadTaskParameters(task)}
              onDelete={(task) => openDeleteConfirm([task.id], "one")}
              onOpenLightbox={openLightbox}
              onOpenReference={(index) => { setTaskReferenceLightboxIndex(index); setTaskReferenceLightboxOpen(true); }}
            />
          ) : <ConversationEmptyState mode="image" promptPresets={IMAGE_PROMPT_PRESETS} onApplyPromptPreset={applyPreset} />}
        </div>

        <div
          ref={composerDockRef}
          className="pointer-events-none absolute inset-x-0 bottom-0 z-30 px-1 pb-[calc(env(safe-area-inset-bottom)+0.5rem)] sm:px-4 sm:pb-2"
          style={{ "--image-composer-dock-height": `${composerDockHeight}px` } as CSSProperties}
        >
          <div className="pointer-events-auto mx-auto w-full max-w-[900px]">
            {providers.length === 0 ? (
              <div className="rounded-[24px] border border-[#f2f3f5] bg-white/95 p-3 shadow-[0_24px_80px_-34px_rgba(15,23,42,0.42)] backdrop-blur dark:border-border dark:bg-card/95">
                <div className="flex min-h-28 items-center justify-center text-sm text-muted-foreground">暂无可用渠道，请联系管理员配置并启用渠道。</div>
              </div>
            ) : (
              <>
                {supportsReferences && references.length > 0 ? (
                  <div className="hide-scrollbar mb-2 flex max-h-20 gap-2 overflow-x-auto px-1 py-1">
                    {referenceLightboxItems.map((preview, index) => (
                      <div key={preview.id} className="relative size-16 shrink-0">
                        <button type="button" className="size-16 overflow-hidden rounded-xl border border-stone-200 bg-stone-50" onClick={() => { setReferenceLightboxIndex(index); setReferenceLightboxOpen(true); }}>
                          <img src={preview.src} alt={references[index]?.name || `参考图 ${index + 1}`} className="size-full object-cover" />
                        </button>
                        <button type="button" className="absolute -right-1 -top-1 inline-flex size-5 items-center justify-center rounded-full border border-stone-200 bg-white text-stone-500 shadow-sm" onClick={() => setReferences((items) => items.filter((_, current) => current !== index))} aria-label={`移除 ${references[index]?.name || "参考图"}`}><X className="size-3" /></button>
                      </div>
                    ))}
                  </div>
                ) : null}
                <div
                  ref={composerPanelRef}
                  className={cn(
                    "relative overflow-visible rounded-[30px] border border-[#dedee3] bg-[#fffcff]/95 shadow-[0_20px_70px_-42px_rgba(15,23,42,0.5)] backdrop-blur-xl transition-colors dark:border-border dark:bg-card/95 dark:shadow-[0_24px_80px_-38px_rgba(0,0,0,0.78)] sm:rounded-[24px] sm:border-[#f2f3f5] sm:bg-white/95 sm:shadow-[0_24px_80px_-34px_rgba(15,23,42,0.42)] sm:dark:border-border sm:dark:bg-card/95",
                    isReferenceDragActive && "border-[#1456f0] bg-[#eef4ff]/95 dark:border-sky-500/70 dark:bg-sky-950/45 sm:border-[#1456f0] sm:bg-[#eef4ff]/95 sm:dark:border-sky-500/70 sm:dark:bg-sky-950/45",
                  )}
                  onDragEnter={handleReferenceDragEnter}
                  onDragOver={(event) => { if (supportsReferences && hasDraggedImage(event.dataTransfer)) event.preventDefault(); }}
                  onDragLeave={handleReferenceDragLeave}
                  onDrop={handleReferenceDrop}
                >
                  {isReferenceDragActive ? <div className="pointer-events-none absolute inset-0 z-20 flex items-center justify-center rounded-[30px] border-2 border-dashed border-[#1456f0]/70 bg-white/70 text-sm font-medium text-[#1456f0] backdrop-blur-sm dark:border-sky-400/70 dark:bg-background/70 dark:text-sky-300 sm:rounded-[24px]"><span className="inline-flex items-center gap-2 rounded-full bg-white/90 px-4 py-2 shadow-[0_10px_30px_-18px_rgba(15,23,42,0.5)] dark:bg-card/90"><ImagePlus className="size-4" />松开上传图片</span></div> : null}
                  <input ref={fileInputRef} type="file" accept="image/png,image/jpeg,image/webp" multiple className="hidden" onChange={(event) => { handleReferenceFiles(event.target.files); event.target.value = ""; }} />
                  <button
                    type="button"
                    className="hidden h-4 w-full cursor-[ns-resize] touch-none select-none items-center justify-center rounded-t-[24px] focus-visible:outline-none sm:flex"
                    onPointerDown={handlePromptResizeStart}
                    onPointerMove={handlePromptResizeMove}
                    onPointerUp={handlePromptResizeEnd}
                    onPointerCancel={handlePromptResizeEnd}
                    onKeyDown={handlePromptResizeKeyDown}
                    aria-label="调整提示词输入区域高度"
                  ><span className="h-1 w-10 rounded-full bg-[#8e8e93]/40" /></button>
                  <div className="cursor-text" onClick={() => textareaRef.current?.focus()}>
                    <Textarea
                      ref={textareaRef}
                      value={prompt}
                      onChange={(event) => setPrompt(event.target.value)}
                      onPaste={handlePromptPaste}
                      placeholder={supportsReferences && references.length > 0 ? "描述你希望如何修改参考图" : "输入你想要生成的画面..."}
                      className="min-h-[96px] resize-none rounded-none border-0 bg-transparent px-6 pt-6 pb-2 text-[17px] leading-7 text-[#222222] shadow-none placeholder:text-[#8e8e93] focus-visible:ring-0 dark:text-foreground dark:placeholder:text-muted-foreground sm:min-h-0 sm:px-5 sm:py-4 sm:text-[15px] sm:leading-6"
                      maxLength={12000}
                      style={{ height: promptAreaHeight }}
                      onKeyDown={(event) => { if (event.key === "Enter" && !event.shiftKey) { event.preventDefault(); void submit(); } }}
                    />
                  </div>

                  <div ref={composerToolbarRef} className="rounded-b-[30px] bg-transparent px-3 pt-1 pb-3 sm:rounded-b-[24px] sm:border-t sm:border-[#f2f3f5] sm:bg-white/80 sm:px-4 sm:py-2.5 sm:dark:border-border sm:dark:bg-card/80" onClick={(event) => event.stopPropagation()}>
                    <div className="grid grid-cols-[minmax(0,1fr)_auto] items-center gap-2 sm:gap-3">
                      <div className="flex min-w-0 flex-nowrap items-center gap-1.5 sm:gap-2">
                        {/* 渠道选择（替代模型选择的第一入口） */}
                        <div className="relative shrink-0">
                          <Select value={providerId} onValueChange={handleProviderChange}>
                            <SelectTrigger className="h-9 w-[min(128px,36vw)] rounded-full border-[#e5e7eb] bg-white text-xs font-medium text-[#45515e] dark:border-border dark:bg-background/70 dark:text-muted-foreground sm:h-8 sm:w-[150px]">
                              <Server className="size-4 shrink-0 opacity-70" />
                              <SelectValue placeholder="选择渠道" />
                            </SelectTrigger>
                            <SelectContent>{providers.map((provider) => <SelectItem key={provider.id} value={provider.id}>{provider.name}</SelectItem>)}</SelectContent>
                          </Select>
                        </div>

                        {/* 模型选择 */}
                        <div className="relative shrink-0">
                          <button
                            type="button"
                            className={cn(
                              "inline-flex size-9 items-center justify-center gap-1.5 rounded-full text-xs font-medium text-[#686b73] transition hover:bg-black/[0.05] dark:text-muted-foreground dark:hover:bg-accent/60 sm:h-8 sm:w-[170px] sm:border sm:border-[#e5e7eb] sm:bg-white sm:px-3 sm:text-[#45515e] sm:dark:border-border sm:dark:bg-background/70",
                              isModelMenuOpen && "bg-[#eef4ff] text-[#1456f0] sm:border-[#bfdbfe] sm:bg-[#eef4ff] sm:text-[#1456f0]",
                            )}
                            onClick={() => setIsModelMenuOpen((open) => !open)}
                            aria-expanded={isModelMenuOpen}
                            title={`模型：${model}`}
                          >
                            <Bot className="size-5 shrink-0 sm:hidden" />
                            <span className="hidden shrink-0 sm:inline">模型</span>
                            <span className="hidden min-w-0 flex-1 truncate text-left font-semibold sm:inline">{model}</span>
                            <ChevronDown className={cn("hidden size-4 shrink-0 opacity-60 transition sm:block", isModelMenuOpen && "rotate-180")} />
                          </button>
                          {isModelMenuOpen ? (
                            <div className="absolute bottom-[calc(100%+0.5rem)] left-0 z-[80] max-h-[45dvh] w-[min(14rem,calc(100vw-2rem))] overflow-y-auto rounded-[20px] border border-[#e5e7eb] bg-white p-1.5 shadow-[0_24px_80px_-32px_rgba(15,23,42,0.35)] dark:border-border dark:bg-card sm:w-[200px]">
                              {selectedProvider?.models.map((item) => {
                                const active = item === model;
                                return (
                                  <button
                                    key={item}
                                    type="button"
                                    className={cn(
                                      "flex w-full items-center justify-between rounded-lg px-3 py-2 text-left text-sm text-[#45515e] transition hover:bg-black/[0.05] dark:text-muted-foreground dark:hover:bg-accent/60",
                                      active && "bg-black/[0.05] font-medium text-[#18181b] dark:bg-accent dark:text-foreground",
                                    )}
                                    onClick={() => { setModel(item); setIsModelMenuOpen(false); }}
                                  >
                                    <span className="min-w-0 truncate">{item}</span>
                                    {active ? <Check className="size-4 shrink-0" /> : null}
                                  </button>
                                );
                              })}
                            </div>
                          ) : null}
                        </div>

                        {/* 提示词市场 */}
                        <button
                          type="button"
                          className="inline-flex size-9 shrink-0 items-center justify-center gap-1.5 rounded-full text-[#686b73] transition hover:bg-black/[0.05] dark:text-muted-foreground dark:hover:bg-accent/60 sm:h-8 sm:w-auto sm:border sm:border-[#e5e7eb] sm:bg-white sm:px-3 sm:text-xs sm:font-medium sm:text-[#45515e] sm:dark:border-border sm:dark:bg-background/70"
                          onClick={() => setPromptMarketOpen(true)}
                          title="提示词市场"
                        >
                          <Store className="size-5 sm:size-3.5" />
                          <span className="hidden sm:inline">市场</span>
                        </button>

                        {/* 参数 */}
                        <Popover open={isParamsOpen} onOpenChange={setIsParamsOpen}>
                          <PopoverTrigger asChild>
                            <button
                              type="button"
                              className={cn(
                                "inline-flex size-9 shrink-0 items-center justify-center gap-1.5 rounded-full text-[#686b73] transition hover:bg-black/[0.05] dark:text-muted-foreground dark:hover:bg-accent/60 sm:h-8 sm:w-auto sm:border sm:border-[#e5e7eb] sm:bg-white sm:px-3 sm:text-xs sm:font-medium sm:text-[#45515e] sm:dark:border-border sm:dark:bg-background/70",
                                isParamsOpen && "bg-[#eef4ff] text-[#1456f0] sm:border-[#bfdbfe] sm:bg-[#eef4ff] sm:text-[#1456f0]",
                              )}
                              aria-expanded={isParamsOpen}
                              title="更多参数"
                            >
                              <SlidersHorizontal className="size-5 sm:size-3.5" />
                              <span className="hidden sm:inline">参数</span>
                            </button>
                          </PopoverTrigger>
                          <PopoverContent
                            align="start"
                            side="top"
                            sideOffset={8}
                            className="z-[70] max-h-[min(calc(100dvh-2rem),34rem)] w-[min(calc(100vw-1rem),28rem)] overflow-y-auto overflow-x-hidden rounded-[20px] border-[#e5e7eb] bg-white p-2.5 shadow-[0_24px_80px_-32px_rgba(15,23,42,0.35)] dark:border-border dark:bg-card sm:w-[min(calc(100vw-2rem),28rem)]"
                            onOpenAutoFocus={(event) => event.preventDefault()}
                          >
                            <div className="grid grid-cols-2 gap-2 sm:grid-cols-3">
                              <div className={paramFieldClass}>
                                <span className="shrink-0 text-[11px] font-medium text-[#45515e] dark:text-muted-foreground">张数</span>
                                <Input type="number" inputMode="numeric" min="1" max={providerMaxImages} step="1" value={count} onChange={(event) => setCount(event.target.value)} className="h-7 w-[36px] border-0 bg-transparent px-0 text-center text-xs font-semibold text-[#18181b] shadow-none focus-visible:ring-0 dark:text-foreground" />
                              </div>
                              <div className={paramFieldClass}>
                                <span className="shrink-0 font-medium text-[#45515e] dark:text-muted-foreground">比例</span>
                                <ParamMenu label="比例" value={aspectRatio} valueLabel={aspectRatioLabel} options={IMAGE_ASPECT_RATIO_OPTIONS} open={isAspectRatioMenuOpen} onOpenChange={setIsAspectRatioMenuOpen} onValueChange={setAspectRatio} />
                              </div>
                              {aspectRatio === CUSTOM_IMAGE_ASPECT_RATIO ? (
                                <div className={cn("col-span-2 flex min-w-0 items-center justify-between gap-2 rounded-xl border bg-white px-3 py-1 dark:bg-background/70 sm:col-span-3", isCustomRatioInvalid ? "border-red-300 dark:border-red-500/60" : "border-[#e5e7eb] dark:border-border")}>
                                  <span className="shrink-0 text-[11px] font-medium text-[#45515e] dark:text-muted-foreground">自定义比例</span>
                                  <Input value={customRatio} onChange={(event) => setCustomRatio(event.target.value)} placeholder="例如 5:4 / 2.39:1" aria-invalid={isCustomRatioInvalid} className="h-8 min-w-0 border-0 bg-transparent px-0 text-right text-xs font-semibold text-[#18181b] shadow-none focus-visible:ring-0 dark:text-foreground" />
                                </div>
                              ) : null}
                            </div>
                          </PopoverContent>
                        </Popover>
                      </div>

                      <div className="flex shrink-0 items-center gap-2">
                        {supportsReferences ? (
                          <button
                            type="button"
                            onClick={() => fileInputRef.current?.click()}
                            className="inline-flex size-11 items-center justify-center rounded-full text-[#686b73] transition hover:bg-black/[0.05] dark:text-muted-foreground dark:hover:bg-accent/60 sm:size-10 sm:border sm:border-[#e5e7eb] sm:bg-white sm:text-[#45515e] sm:dark:border-border sm:dark:bg-background/70"
                            aria-label="上传参考图"
                            title={`上传参考图（最多 ${providerMaxReferences} 张）`}
                          >
                            <Plus className="size-6 sm:hidden" />
                            <ImagePlus className="hidden size-4 sm:block" />
                          </button>
                        ) : null}
                        <button
                          type="button"
                          onClick={() => void submit()}
                          disabled={!canSubmit || isSubmitting || !selectedProvider || !model || !prompt.trim() || (requiresReferences && references.length === 0)}
                          className="inline-flex size-11 shrink-0 items-center justify-center rounded-full bg-[#181e25] text-white shadow-[0_4px_10px_rgba(24,30,37,0.12)] transition hover:bg-[#2a323d] disabled:cursor-not-allowed disabled:bg-[#e1e2e4] disabled:text-[#73777f] dark:bg-foreground dark:text-background dark:hover:bg-foreground/90 dark:disabled:bg-muted dark:disabled:text-muted-foreground sm:size-10"
                          aria-label="生成图片"
                          title="生成图片"
                        >
                          {isSubmitting ? <ApiLoadingMark size="inline" label="正在提交生成任务" /> : <ArrowUp className="size-5 sm:size-4" />}
                        </button>
                      </div>
                    </div>
                    <div className="mt-1 flex flex-wrap items-center justify-between gap-2 px-2 text-[11px] leading-5 text-[#8e8e93] dark:text-muted-foreground">
                      <span>{selectedProvider?.protocol === "openai_chat_images" ? `Chat Completions 最多 ${providerMaxReferences} 张参考图` : selectedProvider?.protocol === "openai_image_edits" ? `Images Edits 需要 1-${providerMaxReferences} 张参考图` : "Images API 不支持参考图"}</span>
                      <span>{prompt.length} / 12000</span>
                    </div>
                  </div>
                </div>
              </>
            )}
          </div>
        </div>
      </div>

      <ImageLightbox images={lightboxItems} currentIndex={lightboxIndex} open={lightboxOpen} onOpenChange={setLightboxOpen} onIndexChange={setLightboxIndex} />
      <ImageLightbox images={referenceLightboxItems} currentIndex={referenceLightboxIndex} open={referenceLightboxOpen} onOpenChange={setReferenceLightboxOpen} onIndexChange={setReferenceLightboxIndex} />
      <ImageLightbox images={taskReferenceLightboxItems} currentIndex={taskReferenceLightboxIndex} open={taskReferenceLightboxOpen} onOpenChange={setTaskReferenceLightboxOpen} onIndexChange={setTaskReferenceLightboxIndex} />

      <Dialog open={Boolean(previewImage)} onOpenChange={(open) => !open && setPreviewImage(null)}>
        <DialogContent className="w-[min(94vw,1100px)] max-w-none">
          <DialogHeader>
            <DialogTitle>图片预览</DialogTitle>
            <DialogDescription>{previewImage?.provider_name} · {previewImage?.model}</DialogDescription>
          </DialogHeader>
          {previewImage ? <div className="max-h-[75vh] overflow-auto rounded-lg bg-muted"><AuthenticatedImage src={previewImage.url} alt={previewImage.prompt || "生成图片"} className="mx-auto max-h-[75vh] w-auto object-contain" /></div> : null}
          <DialogFooter>
            {previewImage ? <Button variant="outline" onClick={() => void downloadImage(previewImage).catch((error) => toast.error(error instanceof Error ? error.message : "下载失败"))}><Download className="size-4" />下载原图</Button> : null}
            <Button onClick={() => { setPreviewImage(null); navigate("/image-manager"); }}><Images className="size-4" />打开图片库</Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {deleteConfirm ? (
        <Dialog open onOpenChange={(open) => (!open && !busyTaskId ? setDeleteConfirm(null) : null)}>
          <DialogContent showCloseButton={false} className="rounded-2xl p-6">
            <DialogHeader className="gap-2">
              <DialogTitle>{deleteConfirmTitle}</DialogTitle>
              <DialogDescription className="text-sm leading-6">
                {deleteConfirmDescription}
              </DialogDescription>
            </DialogHeader>
            <DialogFooter>
              <Button variant="outline" onClick={() => setDeleteConfirm(null)} disabled={busyTaskId !== ""}>
                取消
              </Button>
              <Button className="bg-rose-600 text-white hover:bg-rose-700" onClick={() => void handleConfirmDelete()} disabled={busyTaskId !== ""}>
                确认删除
              </Button>
            </DialogFooter>
          </DialogContent>
        </Dialog>
      ) : null}

      <ImagePromptMarket open={promptMarketOpen} onOpenChange={setPromptMarketOpen} onApplyPrompt={applyMarketPrompt} />
    </section>
  );
}

export default function ExternalImagePage() {
  const { isCheckingAuth, session } = useAuthGuard(undefined, "/external-image");
  if (isCheckingAuth || !session) return <LoadingState />;
  return <ExternalImagePageContent session={session} />;
}
