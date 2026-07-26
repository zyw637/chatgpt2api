import type { ReactNode } from "react";

import {
  AccountsPage,
  ExternalChatPage,
  ExternalImagePage,
  ExternalImageProvidersPage,
  HomePage,
  ImageManagerPage,
  ImagePage,
  LinuxDoCallbackPage,
  LoginPage,
  LogsPage,
  ProfilePage,
  RBACPage,
  SettingsPage,
  UsersPage,
} from "@/app/lazy-pages";

export type AppRouteConfig = {
  path: string;
  element: ReactNode;
  requiredPath?: string;
};

export const appRoutes: AppRouteConfig[] = [
  { path: "/", element: <HomePage /> },
  { path: "/login", element: <LoginPage /> },
  { path: "/auth/linuxdo/callback", element: <LinuxDoCallbackPage /> },
  { path: "/external-chat", element: <ExternalChatPage />, requiredPath: "/external-chat" },
  { path: "/external-image", element: <ExternalImagePage />, requiredPath: "/external-image" },
  { path: "/external-image/providers", element: <ExternalImageProvidersPage />, requiredPath: "/external-image/providers" },
  { path: "/accounts", element: <AccountsPage />, requiredPath: "/accounts" },
  { path: "/image-manager", element: <ImageManagerPage />, requiredPath: "/image-manager" },
  { path: "/users", element: <UsersPage />, requiredPath: "/users" },
  { path: "/profile", element: <ProfilePage />, requiredPath: "/profile" },
  { path: "/rbac", element: <RBACPage />, requiredPath: "/rbac" },
  { path: "/logs", element: <LogsPage />, requiredPath: "/logs" },
  { path: "/settings", element: <SettingsPage />, requiredPath: "/settings" },
  { path: "/image", element: <ImagePage />, requiredPath: "/image" },
  { path: "*", element: <HomePage /> },
];
