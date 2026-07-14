import { type HTMLAttributes } from "react";
import { cn } from "@/lib/cn";

export function Badge({
  className,
  tone = "default",
  ...props
}: HTMLAttributes<HTMLSpanElement> & { tone?: "default" | "success" | "warning" | "danger" }) {
  return (
    <span
      className={cn(
        "inline-flex h-5 items-center rounded-full border px-2 text-[11px] font-normal tabular-nums",
        tone === "default" && "border-transparent bg-primary/8 text-foreground",
        tone === "success" && "border-transparent bg-emerald-500/10 text-emerald-700 dark:text-emerald-400",
        tone === "warning" && "border-transparent bg-amber-500/10 text-amber-700 dark:text-amber-400",
        tone === "danger" && "border-transparent bg-destructive/10 text-destructive",
        className,
      )}
      {...props}
    />
  );
}
