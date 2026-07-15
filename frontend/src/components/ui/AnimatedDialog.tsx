import { useEffect, useId, useState, type ReactNode } from "react";
import { X } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { cn } from "@/lib/cn";

const CLOSE_MS = 150;

type AnimatedDialogProps = {
  open: boolean;
  onClose: () => void;
  title: string;
  description?: ReactNode;
  children: ReactNode;
  maxWidthClassName?: string;
  labelledBy?: string;
  closeOnBackdrop?: boolean;
  showCloseButton?: boolean;
  contentKey?: string | number;
};

export function AnimatedDialog({
  open,
  onClose,
  title,
  description,
  children,
  maxWidthClassName = "max-w-xl",
  labelledBy,
  closeOnBackdrop = true,
  showCloseButton = true,
  contentKey,
}: AnimatedDialogProps) {
  const reactId = useId();
  const titleId = labelledBy || `dialog-title-${reactId}`;
  const [mounted, setMounted] = useState(open);
  const [visible, setVisible] = useState(open);

  useEffect(() => {
    if (open) {
      setMounted(true);
      // Next frame so enter animation can start from initial state.
      const id = window.requestAnimationFrame(() => setVisible(true));
      return () => window.cancelAnimationFrame(id);
    }
    setVisible(false);
    const timer = window.setTimeout(() => setMounted(false), CLOSE_MS);
    return () => window.clearTimeout(timer);
  }, [open]);

  useEffect(() => {
    if (!mounted) return;
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === "Escape") onClose();
    };
    window.addEventListener("keydown", onKeyDown);
    const previousOverflow = document.body.style.overflow;
    document.body.style.overflow = "hidden";
    return () => {
      window.removeEventListener("keydown", onKeyDown);
      document.body.style.overflow = previousOverflow;
    };
  }, [mounted, onClose]);

  if (!mounted) return null;

  const state = visible ? "open" : "closed";

  return (
    <div className="dialog-shell" role="dialog" aria-modal="true" aria-labelledby={titleId}>
      <button
        type="button"
        className="dialog-backdrop"
        data-state={state}
        aria-label="关闭对话框"
        onClick={closeOnBackdrop ? onClose : undefined}
      />
      <Card
        className={cn("dialog-panel shadow-2xl", maxWidthClassName)}
        data-state={state}
      >
        <CardContent className="p-5">
          <div className="mb-4 flex items-start justify-between gap-4">
            <div className="min-w-0">
              <h2 id={titleId} className="text-base font-medium">
                {title}
              </h2>
              {description ? (
                <div className="mt-1 text-xs text-muted-foreground">{description}</div>
              ) : null}
            </div>
            {showCloseButton ? (
              <Button size="icon" variant="ghost" aria-label="关闭" onClick={onClose}>
                <X className="size-4" />
              </Button>
            ) : null}
          </div>
          <div key={contentKey} className="dialog-content-enter">
            {children}
          </div>
        </CardContent>
      </Card>
    </div>
  );
}
