import { useEffect } from "react";
import { useLocation } from "react-router-dom";

import { AnimatedRoutes } from "@/app/animated-routes";
import { TopNav } from "@/components/top-nav";
import { cn } from "@/lib/utils";

export function AppShell() {
  const location = useLocation();
  const isExternalChat = location.pathname === "/external-chat";

  useEffect(() => {
    if (!isExternalChat) return;
    const root = document.documentElement;
    const body = document.body;
    const viewport = window.visualViewport;
    const updateViewportHeight = () => {
      root.style.setProperty("--app-viewport-height", `${Math.round(viewport?.height || window.innerHeight)}px`);
    };
    updateViewportHeight();
    body.dataset.externalChatLayout = "true";
    window.scrollTo({ top: 0, left: 0 });
    window.addEventListener("resize", updateViewportHeight);
    viewport?.addEventListener("resize", updateViewportHeight);
    viewport?.addEventListener("scroll", updateViewportHeight);
    return () => {
      delete body.dataset.externalChatLayout;
      delete body.dataset.externalChatComposerFocused;
      root.style.removeProperty("--app-viewport-height");
      window.removeEventListener("resize", updateViewportHeight);
      viewport?.removeEventListener("resize", updateViewportHeight);
      viewport?.removeEventListener("scroll", updateViewportHeight);
    };
  }, [isExternalChat]);

  return (
    <main className={cn("bg-background text-foreground", isExternalChat ? "h-[var(--app-viewport-height,100dvh)] min-h-0 overflow-hidden" : "min-h-screen")}>
      <div className={cn("mx-auto flex max-w-[1440px] flex-col gap-2 px-3 py-3 sm:px-5 lg:px-6", isExternalChat ? "h-full min-h-0" : "min-h-screen")}>
        <TopNav />
        <div className={cn("min-w-0", isExternalChat && "min-h-0 flex-1")}>
          <AnimatedRoutes fill={isExternalChat} />
        </div>
      </div>
    </main>
  );
}
