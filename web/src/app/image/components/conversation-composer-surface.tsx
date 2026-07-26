import { forwardRef, type HTMLAttributes } from "react";

import { cn } from "@/lib/utils";

export const ConversationComposerSurface = forwardRef<HTMLDivElement, HTMLAttributes<HTMLDivElement>>(
  function ConversationComposerSurface({ className, ...props }, ref) {
    return (
      <div
        ref={ref}
        className={cn(
          "relative overflow-visible rounded-[30px] border border-[#dedee3] bg-[#fffcff]/95 shadow-[0_20px_70px_-42px_rgba(15,23,42,0.5)] backdrop-blur-xl transition-colors dark:border-border dark:bg-card/95 dark:shadow-[0_24px_80px_-38px_rgba(0,0,0,0.78)] sm:rounded-[24px] sm:border-[#f2f3f5] sm:bg-white/95 sm:shadow-[0_24px_80px_-34px_rgba(15,23,42,0.42)] sm:dark:border-border sm:dark:bg-card/95",
          className,
        )}
        {...props}
      />
    );
  },
);

export const ConversationComposerToolbar = forwardRef<HTMLDivElement, HTMLAttributes<HTMLDivElement>>(
  function ConversationComposerToolbar({ className, ...props }, ref) {
    return (
      <div
        ref={ref}
        className={cn(
          "rounded-b-[30px] bg-transparent px-3 pt-1 pb-3 sm:rounded-b-[24px] sm:border-t sm:border-[#f2f3f5] sm:bg-white/80 sm:px-4 sm:py-2.5 sm:dark:border-border sm:dark:bg-card/80",
          className,
        )}
        {...props}
      />
    );
  },
);
