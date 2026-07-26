import { lazy } from "react";

export const AccountsPage = lazy(() => import("@/app/accounts/page"));
export const LinuxDoCallbackPage = lazy(() => import("@/app/auth/linuxdo/callback/page"));
export const ImagePage = lazy(() => import("@/app/image/page"));
export const ImageManagerPage = lazy(() => import("@/app/image-manager/page"));
export const ExternalImagePage = lazy(() => import("@/app/external-image/page"));
export const ExternalImageProvidersPage = lazy(() => import("@/app/external-image/providers/page"));
export const ExternalChatPage = lazy(() => import("@/app/external-chat/page"));
export const HomePage = lazy(() => import("@/app/page"));
export const LoginPage = lazy(() => import("@/app/login/page"));
export const LogsPage = lazy(() => import("@/app/logs/page"));
export const ProfilePage = lazy(() => import("@/app/profile/page"));
export const RBACPage = lazy(() => import("@/app/rbac/page"));
export const SettingsPage = lazy(() => import("@/app/settings/page"));
export const UsersPage = lazy(() => import("@/app/users/page"));
