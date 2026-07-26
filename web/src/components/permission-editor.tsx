"use client";

import { useMemo } from "react";

import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import type { ApiPermission, PermissionMenu } from "@/lib/api";
import { cn } from "@/lib/utils";

function flattenMenuPermissions(items: PermissionMenu[] | null | undefined): PermissionMenu[] {
  const out: PermissionMenu[] = [];
  (Array.isArray(items) ? items : []).forEach((item) => {
    out.push(item);
    out.push(...flattenMenuPermissions(item.children));
  });
  return out;
}

function groupApiPermissions(items: ApiPermission[]) {
  return items.reduce<Array<{ group: string; items: ApiPermission[] }>>((groups, item) => {
    const group = item.group || "其他";
    const existing = groups.find((entry) => entry.group === group);
    if (existing) {
      existing.items.push(item);
      return groups;
    }
    groups.push({ group, items: [item] });
    return groups;
  }, []);
}

function toggleListValue(values: string[], value: string, checked: boolean) {
  const current = new Set(values);
  if (checked) {
    current.add(value);
  } else {
    current.delete(value);
  }
  return Array.from(current).sort();
}

type PermissionBundle = {
  id: string;
  label: string;
  description: string;
  menuPaths: string[];
  apiPermissions: string[];
};

const permissionBundles: PermissionBundle[] = [
  {
    id: "external-image-use",
    label: "API 生图",
    description: "使用已启用渠道，提交、查看和取消自己的 API 生图任务。",
    menuPaths: ["/external-image"],
    apiPermissions: [
      "get/api/external-image-providers",
      "get/api/external-image-tasks",
      "post/api/external-image-tasks",
      "delete/api/external-image-tasks",
    ],
  },
  {
    id: "external-image-manage",
    label: "渠道配置",
    description: "管理渠道、API Key、连接测试和 API 生图运行限制。",
    menuPaths: ["/external-image/providers"],
    apiPermissions: [
      "get/api/admin/external-image-providers",
      "post/api/admin/external-image-providers",
      "patch/api/admin/external-image-providers",
      "delete/api/admin/external-image-providers",
    ],
  },
];

const bundledMenuPaths = new Set(permissionBundles.flatMap((bundle) => bundle.menuPaths));
const bundledApiPermissions = new Set(permissionBundles.flatMap((bundle) => bundle.apiPermissions));

function setListValues(values: string[], targets: string[], checked: boolean) {
  const next = new Set(values);
  targets.forEach((target) => {
    if (checked) next.add(target);
    else next.delete(target);
  });
  return Array.from(next).sort();
}

function apiMethodClass(method: string) {
  switch (method.toUpperCase()) {
    case "GET":
      return "border-sky-200 bg-sky-50 text-sky-700 dark:border-sky-900 dark:bg-sky-950/30 dark:text-sky-300";
    case "POST":
      return "border-emerald-200 bg-emerald-50 text-emerald-700 dark:border-emerald-900 dark:bg-emerald-950/30 dark:text-emerald-300";
    case "PATCH":
      return "border-amber-200 bg-amber-50 text-amber-700 dark:border-amber-900 dark:bg-amber-950/30 dark:text-amber-300";
    case "DELETE":
      return "border-rose-200 bg-rose-50 text-rose-700 dark:border-rose-900 dark:bg-rose-950/30 dark:text-rose-300";
    default:
      return "border-border bg-muted text-muted-foreground";
  }
}

type PermissionEditorProps = {
  menus: PermissionMenu[];
  apis: ApiPermission[];
  selectedMenuPaths: string[];
  selectedApiPermissions: string[];
  onMenuPathsChange: (paths: string[]) => void;
  onApiPermissionsChange: (permissions: string[]) => void;
  className?: string;
};

export function PermissionEditor({
  menus,
  apis,
  selectedMenuPaths,
  selectedApiPermissions,
  onMenuPathsChange,
  onApiPermissionsChange,
  className,
}: PermissionEditorProps) {
  const allMenuPermissions = useMemo(() => flattenMenuPermissions(menus), [menus]);
  const menuPermissions = useMemo(
    () => allMenuPermissions.filter((item) => !bundledMenuPaths.has(item.path)),
    [allMenuPermissions],
  );
  const visibleApiPermissions = useMemo(
    () => apis.filter((item) => !bundledApiPermissions.has(item.key)),
    [apis],
  );
  const apiPermissionGroups = useMemo(() => groupApiPermissions(visibleApiPermissions), [visibleApiPermissions]);
  const allMenuPaths = useMemo(() => allMenuPermissions.map((item) => item.path), [allMenuPermissions]);
  const allApiPermissionKeys = useMemo(() => apis.map((item) => item.key), [apis]);

  return (
    <div className={cn("grid min-h-0 gap-5 lg:grid-cols-[280px_1fr]", className)}>
      <section className="overflow-hidden rounded-xl border border-border lg:col-span-2">
        <div className="border-b border-border px-4 py-3">
          <h3 className="text-sm font-semibold text-foreground">功能权限</h3>
          <p className="mt-1 text-xs text-muted-foreground">一个开关会同步对应页面与接口权限。</p>
        </div>
        <div className="grid gap-3 p-4 md:grid-cols-2">
          {permissionBundles.map((bundle) => {
            const targets = [...bundle.menuPaths, ...bundle.apiPermissions];
            const selectedCount = bundle.menuPaths.filter((path) => selectedMenuPaths.includes(path)).length
              + bundle.apiPermissions.filter((key) => selectedApiPermissions.includes(key)).length;
            const checked = selectedCount === targets.length;
            const indeterminate = selectedCount > 0 && !checked;
            return (
              <label key={bundle.id} className="flex cursor-pointer items-start gap-3 rounded-lg border border-border px-4 py-4 transition hover:bg-muted/50">
                <Checkbox
                  checked={indeterminate ? "indeterminate" : checked}
                  onCheckedChange={(value) => {
                    const nextChecked = value === true;
                    onMenuPathsChange(setListValues(selectedMenuPaths, bundle.menuPaths, nextChecked));
                    onApiPermissionsChange(setListValues(selectedApiPermissions, bundle.apiPermissions, nextChecked));
                  }}
                  className="mt-0.5"
                />
                <span className="min-w-0">
                  <span className="block text-sm font-semibold text-foreground">{bundle.label}</span>
                  <span className="mt-1 block text-xs leading-5 text-muted-foreground">{bundle.description}</span>
                </span>
              </label>
            );
          })}
        </div>
      </section>

      <section className="flex min-h-0 min-w-0 flex-col overflow-hidden rounded-xl border border-border">
        <div className="flex items-center justify-between gap-3 border-b border-border px-4 py-3">
          <div className="min-w-0">
            <h3 className="text-sm font-semibold text-foreground">菜单</h3>
            <p className="text-xs text-muted-foreground">
              {selectedMenuPaths.length} / {allMenuPaths.length}
            </p>
          </div>
          <div className="flex shrink-0 items-center gap-1">
            <Button
              type="button"
              variant="ghost"
              className="h-7 rounded-md px-2 text-xs"
              onClick={() => onMenuPathsChange(allMenuPaths)}
            >
              全选
            </Button>
            <Button
              type="button"
              variant="ghost"
              className="h-7 rounded-md px-2 text-xs"
              onClick={() => onMenuPathsChange([])}
            >
              清空
            </Button>
          </div>
        </div>
        <div className="min-h-0 divide-y divide-border overflow-y-auto overscroll-contain [scrollbar-color:rgba(142,142,147,.45)_transparent] [scrollbar-gutter:stable] [scrollbar-width:thin] lg:max-h-[calc(100dvh-27rem)] [&::-webkit-scrollbar]:w-1.5 [&::-webkit-scrollbar-thumb]:rounded-full [&::-webkit-scrollbar-thumb]:bg-[#8e8e93]/45 [&::-webkit-scrollbar-track]:bg-transparent">
          {menuPermissions.length > 0 ? (
            menuPermissions.map((item) => (
              <label
                key={item.path}
                className="flex cursor-pointer items-center gap-3 px-4 py-3 transition hover:bg-muted/50"
              >
                <Checkbox
                  checked={selectedMenuPaths.includes(item.path)}
                  onCheckedChange={(checked) =>
                    onMenuPathsChange(toggleListValue(selectedMenuPaths, item.path, Boolean(checked)))
                  }
                />
                <div className="min-w-0">
                  <div className="truncate text-sm font-medium text-foreground">{item.label}</div>
                  <code className="block truncate font-mono text-xs text-muted-foreground">{item.path}</code>
                </div>
              </label>
            ))
          ) : (
            <div className="px-4 py-8 text-center text-sm text-muted-foreground">暂无菜单权限</div>
          )}
        </div>
      </section>

      <section className="flex min-h-0 min-w-0 flex-col overflow-hidden rounded-xl border border-border">
        <div className="flex items-center justify-between gap-3 border-b border-border px-4 py-3">
          <div className="min-w-0">
            <h3 className="text-sm font-semibold text-foreground">API</h3>
            <p className="text-xs text-muted-foreground">
              {selectedApiPermissions.length} / {allApiPermissionKeys.length}
            </p>
          </div>
          <div className="flex shrink-0 items-center gap-1">
            <Button
              type="button"
              variant="ghost"
              className="h-7 rounded-md px-2 text-xs"
              onClick={() => onApiPermissionsChange(allApiPermissionKeys)}
            >
              全选
            </Button>
            <Button
              type="button"
              variant="ghost"
              className="h-7 rounded-md px-2 text-xs"
              onClick={() => onApiPermissionsChange([])}
            >
              清空
            </Button>
          </div>
        </div>
        <div className="min-h-0 space-y-4 overflow-y-auto overscroll-contain p-4 [scrollbar-color:rgba(142,142,147,.45)_transparent] [scrollbar-gutter:stable] [scrollbar-width:thin] lg:max-h-[calc(100dvh-27rem)] [&::-webkit-scrollbar]:w-1.5 [&::-webkit-scrollbar-thumb]:rounded-full [&::-webkit-scrollbar-thumb]:bg-[#8e8e93]/45 [&::-webkit-scrollbar-track]:bg-transparent">
          {apiPermissionGroups.length > 0 ? (
            apiPermissionGroups.map((group) => (
              <article key={group.group} className="min-w-0">
                <div className="mb-2 flex items-center justify-between gap-3">
                  <h4 className="text-xs font-semibold uppercase text-muted-foreground">{group.group}</h4>
                  <span className="text-xs text-muted-foreground">{group.items.length}</span>
                </div>
                <div className="grid gap-2 md:grid-cols-2">
                  {group.items.map((permission) => (
                    <label
                      key={permission.key}
                      className="flex cursor-pointer items-start gap-3 rounded-lg border border-border px-3 py-3 transition hover:bg-muted/50"
                    >
                      <Checkbox
                        checked={selectedApiPermissions.includes(permission.key)}
                        onCheckedChange={(checked) =>
                          onApiPermissionsChange(
                            toggleListValue(selectedApiPermissions, permission.key, Boolean(checked)),
                          )
                        }
                        className="mt-0.5"
                      />
                      <div className="min-w-0 flex-1">
                        <div className="flex min-w-0 items-center gap-2">
                          <span
                            className={cn(
                              "shrink-0 rounded-md border px-1.5 py-0.5 font-mono text-[10px] font-semibold leading-none",
                              apiMethodClass(permission.method),
                            )}
                          >
                            {permission.method}
                          </span>
                          <span className="truncate text-sm font-medium text-foreground">{permission.label}</span>
                        </div>
                        <code className="mt-1 block truncate font-mono text-xs text-muted-foreground">
                          {permission.path}
                          {permission.subtree ? "/*" : ""}
                        </code>
                      </div>
                    </label>
                  ))}
                </div>
              </article>
            ))
          ) : (
            <div className="px-4 py-8 text-center text-sm text-muted-foreground">暂无 API 权限</div>
          )}
        </div>
      </section>
    </div>
  );
}
