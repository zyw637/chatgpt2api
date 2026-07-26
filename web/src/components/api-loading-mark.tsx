import { cn } from "@/lib/utils";

type ApiLoadingMarkProps = {
  className?: string;
  label?: string;
  size?: "page" | "section" | "media" | "inline";
};

export function ApiLoadingMark({ className, label, size = "inline" }: ApiLoadingMarkProps) {
  return (
    <span
      className={cn("api-loading-mark", `api-loading-mark-${size}`, className)}
      role="status"
      aria-label={label || "请求处理中"}
    >
      <span className="api-loading-mark-stage" aria-hidden="true">
        <span className="api-loading-mark-segment api-loading-mark-segment-one" />
        <span className="api-loading-mark-segment api-loading-mark-segment-two" />
        <span className="api-loading-mark-segment api-loading-mark-segment-three" />
      </span>
    </span>
  );
}
