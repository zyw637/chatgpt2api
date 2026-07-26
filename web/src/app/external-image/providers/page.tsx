"use client";

import { useCallback, useEffect, useState } from "react";
import { Ban, CheckCircle2, Pencil, Plus, RefreshCw, ServerCog, Trash2 } from "lucide-react";
import { toast } from "sonner";

import { PageHeader } from "@/components/page-header";
import { ApiLoadingMark } from "@/components/api-loading-mark";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Checkbox } from "@/components/ui/checkbox";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Textarea } from "@/components/ui/textarea";
import {
  createExternalImageProvider,
  deleteExternalImageProvider,
  fetchAdminExternalImageProviders,
  testExternalImageProvider,
  updateExternalImageProvider,
  updateExternalImageSettings,
} from "@/lib/external-api";
import {
  type ExternalImageProtocol,
  type ExternalImageProvider,
  type ExternalImageProviderInput,
  type ExternalImageSettings,
} from "@/lib/api";
import { cn } from "@/lib/utils";
import { useAuthGuard } from "@/lib/use-auth-guard";

type ProviderForm = {
  name: string;
  enabled: boolean;
  sortOrder: string;
  imageEnabled: boolean;
  chatEnabled: boolean;
  protocol: ExternalImageProtocol;
  baseUrl: string;
  apiKey: string;
  models: string;
  defaultModel: string;
  defaultSize: string;
  defaultQuality: string;
  temperature: string;
  maxImages: string;
  maxReferenceImages: string;
  timeoutSeconds: string;
  concurrencyLimit: string;
  chatModels: string;
  chatDefaultModel: string;
  chatTemperature: string;
  chatConcurrencyLimit: string;
};

const emptyProviderForm: ProviderForm = {
  name: "",
  enabled: true,
  sortOrder: "0",
  imageEnabled: true,
  chatEnabled: false,
  protocol: "openai_images",
  baseUrl: "",
  apiKey: "",
  models: "gpt-image-2",
  defaultModel: "gpt-image-2",
  defaultSize: "auto",
  defaultQuality: "auto",
  temperature: "0.7",
  maxImages: "4",
  maxReferenceImages: "4",
  timeoutSeconds: "300",
  concurrencyLimit: "2",
  chatModels: "gpt-5-mini",
  chatDefaultModel: "gpt-5-mini",
  chatTemperature: "",
  chatConcurrencyLimit: "2",
};

function providerFormFromItem(item: ExternalImageProvider): ProviderForm {
  const imageModels = Array.isArray(item.models) ? item.models : [];
  const chatModels = Array.isArray(item.chat_models) ? item.chat_models : [];
  return {
    name: item.name,
    enabled: item.enabled,
    sortOrder: String(item.sort_order ?? 0),
    imageEnabled: item.image_enabled,
    chatEnabled: item.chat_enabled,
    protocol: item.protocol || "openai_images",
    baseUrl: item.base_url || "",
    apiKey: "",
    models: imageModels.join(", "),
    defaultModel: item.default_model,
    defaultSize: item.default_size || "auto",
    defaultQuality: item.default_quality || "auto",
    temperature: String(item.temperature ?? 0.7),
    maxImages: String(item.max_images ?? 4),
    maxReferenceImages: String(item.max_reference_images || 4),
    timeoutSeconds: String(item.timeout_seconds ?? 300),
    concurrencyLimit: String(item.concurrency_limit ?? 2),
    chatModels: chatModels.join(", "),
    chatDefaultModel: item.chat_default_model || chatModels[0] || "",
    chatTemperature: item.chat_temperature == null ? "" : String(item.chat_temperature),
    chatConcurrencyLimit: String(item.chat_concurrency_limit ?? 2),
  };
}

function providerPayload(form: ProviderForm): ExternalImageProviderInput {
  const models = Array.from(new Set(form.models.split(/[,\n]/).map((item) => item.trim()).filter(Boolean)));
  const chatModels = Array.from(new Set(form.chatModels.split(/[,\n]/).map((item) => item.trim()).filter(Boolean)));
  return {
    name: form.name.trim(),
    enabled: form.enabled,
    sort_order: Number(form.sortOrder),
    image_enabled: form.imageEnabled,
    chat_enabled: form.chatEnabled,
    protocol: form.protocol,
    base_url: form.baseUrl.trim().replace(/\/+$/, ""),
    ...(form.apiKey.trim() ? { api_key: form.apiKey.trim() } : {}),
    models,
    default_model: form.defaultModel.trim(),
    default_size: form.defaultSize,
    default_quality: form.defaultQuality,
    temperature: Number(form.temperature),
    max_images: Number(form.maxImages),
    max_reference_images: form.protocol === "openai_images" ? 0 : Number(form.maxReferenceImages),
    timeout_seconds: Number(form.timeoutSeconds),
    concurrency_limit: Number(form.concurrencyLimit),
    chat_protocol: "openai_chat",
    chat_models: chatModels,
    chat_default_model: form.chatDefaultModel.trim(),
    chat_temperature: form.chatTemperature.trim() ? Number(form.chatTemperature) : null,
    chat_concurrency_limit: Number(form.chatConcurrencyLimit),
  };
}

function LoadingState() {
  return <div className="flex min-h-[50vh] items-center justify-center"><ApiLoadingMark size="page" label="正在加载渠道" /></div>;
}

function ExternalImageProvidersContent() {
  const [providers, setProviders] = useState<ExternalImageProvider[]>([]);
  const [settings, setSettings] = useState<ExternalImageSettings>({ user_concurrent_limit: 2, user_rpm_limit: 10, chat_user_concurrent_limit: 2, chat_user_rpm_limit: 20 });
  const [settingsDraft, setSettingsDraft] = useState({ imageConcurrent: "2", imageRPM: "10", chatConcurrent: "2", chatRPM: "20" });
  const [isLoading, setIsLoading] = useState(true);
  const [isRefreshing, setIsRefreshing] = useState(false);
  const [isSavingSettings, setIsSavingSettings] = useState(false);
  const [busyProviderId, setBusyProviderId] = useState("");
  const [dialogOpen, setDialogOpen] = useState(false);
  const [editingProvider, setEditingProvider] = useState<ExternalImageProvider | null>(null);
  const [form, setForm] = useState<ProviderForm>(emptyProviderForm);
  const [isSavingProvider, setIsSavingProvider] = useState(false);

  const load = useCallback(async (quiet = false) => {
    if (!quiet) setIsRefreshing(true);
    try {
      const data = await fetchAdminExternalImageProviders();
      setProviders(data.items);
      setSettings(data.settings);
      setSettingsDraft({
        imageConcurrent: String(data.settings.user_concurrent_limit),
        imageRPM: String(data.settings.user_rpm_limit),
        chatConcurrent: String(data.settings.chat_user_concurrent_limit),
        chatRPM: String(data.settings.chat_user_rpm_limit),
      });
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "加载渠道失败");
    } finally {
      setIsLoading(false);
      setIsRefreshing(false);
    }
  }, []);

  useEffect(() => { void load(); }, [load]);

  const openCreate = () => {
    setEditingProvider(null);
    setForm(emptyProviderForm);
    setDialogOpen(true);
  };

  const openEdit = (provider: ExternalImageProvider) => {
    setEditingProvider(provider);
    setForm(providerFormFromItem(provider));
    setDialogOpen(true);
  };

  const saveProvider = async () => {
    const payload = providerPayload(form);
    if (!payload.name || !payload.base_url) {
      toast.error("请填写渠道名称和 Base URL");
      return;
    }
    if (!payload.image_enabled && !payload.chat_enabled) {
      toast.error("请至少启用聊天或生图能力");
      return;
    }
    if (payload.image_enabled && (payload.models.length === 0 || !payload.default_model)) {
      toast.error("请填写生图模型和默认模型");
      return;
    }
    if (payload.chat_enabled && (payload.chat_models.length === 0 || !payload.chat_default_model)) {
      toast.error("请填写聊天模型和默认模型");
      return;
    }
    if (!editingProvider && !payload.api_key) {
      toast.error("新建渠道必须填写 API Key");
      return;
    }
    setIsSavingProvider(true);
    try {
      const data = editingProvider
        ? await updateExternalImageProvider(editingProvider.id, payload)
        : await createExternalImageProvider(payload);
      setProviders(data.items);
      setDialogOpen(false);
      toast.success(editingProvider ? "渠道已更新" : "渠道已创建");
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "保存渠道失败");
    } finally {
      setIsSavingProvider(false);
    }
  };

  const toggleProvider = async (provider: ExternalImageProvider) => {
    setBusyProviderId(provider.id);
    try {
      const data = await updateExternalImageProvider(provider.id, { enabled: !provider.enabled });
      setProviders(data.items);
      toast.success(provider.enabled ? "渠道已停用" : "渠道已启用");
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "修改渠道状态失败");
    } finally {
      setBusyProviderId("");
    }
  };

  const removeProvider = async (provider: ExternalImageProvider) => {
    if (!window.confirm(`确认删除渠道“${provider.name}”？已有任务和图片元数据会保留。`)) return;
    setBusyProviderId(provider.id);
    try {
      const data = await deleteExternalImageProvider(provider.id);
      setProviders(data.items);
      toast.success("渠道已删除");
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "删除渠道失败");
    } finally {
      setBusyProviderId("");
    }
  };

  const testProvider = async (provider: ExternalImageProvider) => {
    setBusyProviderId(provider.id);
    try {
      const data = await testExternalImageProvider(provider.id);
      if (data.result.missing_models.length > 0) {
        toast.warning(`连接正常，但 /models 未返回：${data.result.missing_models.join(", ")}`);
      } else {
        toast.success(`连接正常，耗时 ${data.result.latency_ms} ms`);
      }
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "测试连接失败");
    } finally {
      setBusyProviderId("");
    }
  };

  const saveSettings = async () => {
    setIsSavingSettings(true);
    try {
      const data = await updateExternalImageSettings({
        user_concurrent_limit: Number(settingsDraft.imageConcurrent),
        user_rpm_limit: Number(settingsDraft.imageRPM),
        chat_user_concurrent_limit: Number(settingsDraft.chatConcurrent),
        chat_user_rpm_limit: Number(settingsDraft.chatRPM),
      });
      setSettings(data.settings);
      setSettingsDraft({
        imageConcurrent: String(data.settings.user_concurrent_limit),
        imageRPM: String(data.settings.user_rpm_limit),
        chatConcurrent: String(data.settings.chat_user_concurrent_limit),
        chatRPM: String(data.settings.chat_user_rpm_limit),
      });
      toast.success("独立限流设置已保存");
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "保存限流设置失败");
    } finally {
      setIsSavingSettings(false);
    }
  };

  const settingsUnchanged =
    Number(settingsDraft.imageConcurrent) === settings.user_concurrent_limit &&
    Number(settingsDraft.imageRPM) === settings.user_rpm_limit &&
    Number(settingsDraft.chatConcurrent) === settings.chat_user_concurrent_limit &&
    Number(settingsDraft.chatRPM) === settings.chat_user_rpm_limit;

  if (isLoading) return <LoadingState />;

  return (
    <div className="space-y-6">
      <PageHeader
        eyebrow="External API"
        title="API 渠道管理"
        actions={<><Button onClick={openCreate}><Plus className="size-4" />新增渠道</Button><Button variant="outline" onClick={() => void load()} disabled={isRefreshing}><RefreshCw className={cn("size-4", isRefreshing && "animate-spin")} />刷新</Button></>}
      />

      <Card>
        <CardHeader><CardTitle className="text-lg">访问限制</CardTitle><CardDescription>聊天和生图分别限流，不影响创作台。</CardDescription></CardHeader>
        <CardContent className="grid gap-4 sm:grid-cols-2 lg:grid-cols-[1fr_1fr_1fr_1fr_auto] lg:items-end">
          <label className="space-y-2 text-sm font-medium">生图每用户并发<Input type="number" min="1" max="20" value={settingsDraft.imageConcurrent} onChange={(event) => setSettingsDraft((current) => ({ ...current, imageConcurrent: event.target.value }))} /></label>
          <label className="space-y-2 text-sm font-medium">生图每用户 RPM<Input type="number" min="1" max="600" value={settingsDraft.imageRPM} onChange={(event) => setSettingsDraft((current) => ({ ...current, imageRPM: event.target.value }))} /></label>
          <label className="space-y-2 text-sm font-medium">聊天每用户并发<Input type="number" min="1" max="20" value={settingsDraft.chatConcurrent} onChange={(event) => setSettingsDraft((current) => ({ ...current, chatConcurrent: event.target.value }))} /></label>
          <label className="space-y-2 text-sm font-medium">聊天每用户 RPM<Input type="number" min="1" max="600" value={settingsDraft.chatRPM} onChange={(event) => setSettingsDraft((current) => ({ ...current, chatRPM: event.target.value }))} /></label>
          <Button onClick={() => void saveSettings()} disabled={isSavingSettings || settingsUnchanged}>{isSavingSettings ? <ApiLoadingMark size="inline" label="正在保存限制" /> : null}保存限制</Button>
        </CardContent>
      </Card>

      {providers.length === 0 ? (
        <Card><CardContent className="py-14 text-center text-sm text-muted-foreground">尚未配置 API 渠道</CardContent></Card>
      ) : (
        <div className="grid gap-4 lg:grid-cols-2">
          {providers.map((provider) => (
            <Card key={provider.id}>
              <CardContent className="space-y-4 p-5">
                <div className="flex items-start justify-between gap-4">
                  <div className="min-w-0"><div className="flex items-center gap-2"><ServerCog className="size-5 text-[#1456f0]" /><span className="truncate font-semibold">{provider.name}</span></div><div className="mt-1 truncate text-xs text-muted-foreground">{provider.base_url}</div></div>
                  <div className="flex shrink-0 flex-wrap justify-end gap-1.5"><Badge variant={provider.enabled ? "success" : "secondary"}>{provider.enabled ? "已启用" : "已停用"}</Badge>{provider.chat_enabled ? <Badge variant="info">聊天</Badge> : null}{provider.image_enabled ? <Badge variant="warning">生图</Badge> : null}</div>
                </div>
                <div className="grid grid-cols-2 gap-3 rounded-lg bg-muted/50 p-3 text-xs">
                  <div><span className="text-muted-foreground">密钥</span><div className="mt-1 font-medium">{provider.has_api_key ? "已配置" : "未配置"}</div></div>
                  <div><span className="text-muted-foreground">排序</span><div className="mt-1 font-medium">{provider.sort_order ?? 0}</div></div>
                  {provider.chat_enabled ? <><div><span className="text-muted-foreground">聊天模型</span><div className="mt-1 truncate font-medium">{provider.chat_default_model}</div></div><div><span className="text-muted-foreground">聊天并发</span><div className="mt-1 font-medium">{provider.chat_concurrency_limit ?? 2}</div></div></> : null}
                  {provider.image_enabled ? <><div><span className="text-muted-foreground">生图模型</span><div className="mt-1 truncate font-medium">{provider.default_model}</div></div><div><span className="text-muted-foreground">生图并发</span><div className="mt-1 font-medium">{provider.concurrency_limit ?? 2}</div></div></> : null}
                </div>
                <div className="flex flex-wrap gap-2">
                  <Button size="sm" variant="outline" onClick={() => void testProvider(provider)} disabled={busyProviderId === provider.id}>{busyProviderId === provider.id ? <ApiLoadingMark size="inline" label="正在测试渠道" /> : <CheckCircle2 className="size-4" />}测试连接</Button>
                  <Button size="sm" variant="outline" onClick={() => openEdit(provider)}><Pencil className="size-4" />编辑</Button>
                  <Button size="sm" variant="outline" onClick={() => void toggleProvider(provider)} disabled={busyProviderId === provider.id}>{provider.enabled ? <Ban className="size-4" /> : <CheckCircle2 className="size-4" />}{provider.enabled ? "停用" : "启用"}</Button>
                  <Button size="sm" variant="outline" className="text-rose-600" onClick={() => void removeProvider(provider)} disabled={busyProviderId === provider.id}><Trash2 className="size-4" />删除</Button>
                </div>
              </CardContent>
            </Card>
          ))}
        </div>
      )}

      <Dialog open={dialogOpen} onOpenChange={setDialogOpen}>
        <DialogContent className="max-h-[92vh] overflow-y-auto sm:w-[min(92vw,720px)]">
          <DialogHeader><DialogTitle>{editingProvider ? "编辑渠道" : "新增渠道"}</DialogTitle><DialogDescription>连接信息由聊天和生图能力共用。Base URL 必须填写到 /v1。</DialogDescription></DialogHeader>
          <div className="grid gap-4 sm:grid-cols-2">
            <label className="space-y-2 text-sm font-medium">渠道名称<Input value={form.name} onChange={(event) => setForm((current) => ({ ...current, name: event.target.value }))} placeholder="例如 ARK717" /></label>
            <label className="space-y-2 text-sm font-medium">显示排序（0 - 999）<Input type="number" min="0" max="999" value={form.sortOrder} onChange={(event) => setForm((current) => ({ ...current, sortOrder: event.target.value }))} /></label>
            <label className="space-y-2 text-sm font-medium sm:col-span-2">Base URL<Input value={form.baseUrl} onChange={(event) => setForm((current) => ({ ...current, baseUrl: event.target.value }))} placeholder="https://api.example.com/v1" /></label>
            <label className="space-y-2 text-sm font-medium sm:col-span-2">API Key<Input type="password" autoComplete="new-password" value={form.apiKey} onChange={(event) => setForm((current) => ({ ...current, apiKey: event.target.value }))} placeholder={editingProvider?.has_api_key ? "留空保留原密钥" : "sk-..."} /></label>
            <div className="flex flex-wrap gap-3 sm:col-span-2"><label className="flex items-center gap-3 rounded-lg border border-border px-3 py-2.5 text-sm font-medium"><Checkbox checked={form.enabled} onCheckedChange={(checked) => setForm((current) => ({ ...current, enabled: checked === true }))} />启用渠道</label><label className="flex items-center gap-3 rounded-lg border border-border px-3 py-2.5 text-sm font-medium"><Checkbox checked={form.chatEnabled} onCheckedChange={(checked) => setForm((current) => ({ ...current, chatEnabled: checked === true }))} />聊天能力</label><label className="flex items-center gap-3 rounded-lg border border-border px-3 py-2.5 text-sm font-medium"><Checkbox checked={form.imageEnabled} onCheckedChange={(checked) => setForm((current) => ({ ...current, imageEnabled: checked === true }))} />生图能力</label></div>

            {form.chatEnabled ? <div className="grid gap-4 border-t border-border pt-4 sm:col-span-2 sm:grid-cols-2"><h3 className="font-semibold sm:col-span-2">聊天能力</h3><label className="space-y-2 text-sm font-medium sm:col-span-2">聊天模型（逗号或换行分隔）<Textarea value={form.chatModels} onChange={(event) => setForm((current) => ({ ...current, chatModels: event.target.value }))} placeholder="gpt-5-mini" /></label><label className="space-y-2 text-sm font-medium sm:col-span-2">默认聊天模型<Input value={form.chatDefaultModel} onChange={(event) => setForm((current) => ({ ...current, chatDefaultModel: event.target.value }))} /></label><label className="space-y-2 text-sm font-medium">默认 Temperature<Input type="number" min="0" max="2" step="0.1" value={form.chatTemperature} onChange={(event) => setForm((current) => ({ ...current, chatTemperature: event.target.value }))} placeholder="模型默认" /></label><label className="space-y-2 text-sm font-medium">聊天渠道并发（1 - 20）<Input type="number" min="1" max="20" value={form.chatConcurrencyLimit} onChange={(event) => setForm((current) => ({ ...current, chatConcurrencyLimit: event.target.value }))} /></label></div> : null}

            {form.imageEnabled ? <div className="grid gap-4 border-t border-border pt-4 sm:col-span-2 sm:grid-cols-2"><h3 className="font-semibold sm:col-span-2">生图能力</h3><label className="space-y-2 text-sm font-medium">生图协议<Select value={form.protocol} onValueChange={(value: ExternalImageProtocol) => setForm((current) => ({ ...current, protocol: value }))}><SelectTrigger><SelectValue /></SelectTrigger><SelectContent><SelectItem value="openai_images">Images API</SelectItem><SelectItem value="openai_image_edits">Images Edits</SelectItem><SelectItem value="openai_chat_images">Chat Completions 生图</SelectItem></SelectContent></Select></label><label className="space-y-2 text-sm font-medium">单次最大张数（1 - 4）<Input type="number" min="1" max="4" value={form.maxImages} onChange={(event) => setForm((current) => ({ ...current, maxImages: event.target.value }))} /></label><label className="space-y-2 text-sm font-medium sm:col-span-2">生图模型（逗号或换行分隔）<Textarea value={form.models} onChange={(event) => setForm((current) => ({ ...current, models: event.target.value }))} placeholder="gpt-image-2" /></label><label className="space-y-2 text-sm font-medium sm:col-span-2">默认生图模型<Input value={form.defaultModel} onChange={(event) => setForm((current) => ({ ...current, defaultModel: event.target.value }))} /></label>{form.protocol !== "openai_chat_images" ? <><label className="space-y-2 text-sm font-medium">默认尺寸<Select value={form.defaultSize} onValueChange={(value) => setForm((current) => ({ ...current, defaultSize: value }))}><SelectTrigger><SelectValue /></SelectTrigger><SelectContent>{["auto", "1024x1024", "1536x1024", "1024x1536"].map((value) => <SelectItem key={value} value={value}>{value}</SelectItem>)}</SelectContent></Select></label><label className="space-y-2 text-sm font-medium">默认质量<Select value={form.defaultQuality} onValueChange={(value) => setForm((current) => ({ ...current, defaultQuality: value }))}><SelectTrigger><SelectValue /></SelectTrigger><SelectContent>{["auto", "low", "medium", "high"].map((value) => <SelectItem key={value} value={value}>{value}</SelectItem>)}</SelectContent></Select></label></> : <label className="space-y-2 text-sm font-medium">生图 Temperature<Input type="number" min="0" max="2" step="0.1" value={form.temperature} onChange={(event) => setForm((current) => ({ ...current, temperature: event.target.value }))} /></label>}{form.protocol !== "openai_images" ? <label className="space-y-2 text-sm font-medium">最大参考图（1 - 4）<Input type="number" min="1" max="4" value={form.maxReferenceImages} onChange={(event) => setForm((current) => ({ ...current, maxReferenceImages: event.target.value }))} /></label> : null}<label className="space-y-2 text-sm font-medium">生图渠道并发（1 - 20）<Input type="number" min="1" max="20" value={form.concurrencyLimit} onChange={(event) => setForm((current) => ({ ...current, concurrencyLimit: event.target.value }))} /></label></div> : null}

            <label className="space-y-2 border-t border-border pt-4 text-sm font-medium sm:col-span-2">请求超时秒数（30 - 600）<Input type="number" min="30" max="600" value={form.timeoutSeconds} onChange={(event) => setForm((current) => ({ ...current, timeoutSeconds: event.target.value }))} /></label>
          </div>
          <DialogFooter><Button variant="outline" onClick={() => setDialogOpen(false)}>取消</Button><Button onClick={() => void saveProvider()} disabled={isSavingProvider}>{isSavingProvider ? <ApiLoadingMark size="inline" label="正在保存渠道" /> : null}保存渠道</Button></DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}

export default function ExternalImageProvidersPage() {
  const { isCheckingAuth, session } = useAuthGuard(undefined, "/external-image/providers");
  if (isCheckingAuth || !session) return <LoadingState />;
  return <ExternalImageProvidersContent />;
}
