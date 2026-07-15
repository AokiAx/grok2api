import { useState } from "react";
import { Clipboard, KeyRound } from "lucide-react";
import { Button } from "@/components/ui/button";
import { AnimatedDialog } from "@/components/ui/AnimatedDialog";

export function ClientKeySecretDialog({
  secret,
  onClose,
}: {
  secret: string;
  onClose: () => void;
}) {
  const [copied, setCopied] = useState(false);

  async function copySecret() {
    try {
      await navigator.clipboard.writeText(secret);
      setCopied(true);
    } catch {
      setCopied(false);
    }
  }

  return (
    <AnimatedDialog
      open
      onClose={onClose}
      title="客户端密钥已创建"
      description="请立即保存，关闭后无法再次查看此 secret。"
      maxWidthClassName="max-w-lg"
      showCloseButton={false}
      closeOnBackdrop={false}
    >
      <div className="flex size-9 items-center justify-center rounded-full bg-emerald-500/10 text-emerald-600">
        <KeyRound className="size-4" />
      </div>
      <code className="mt-4 block break-all rounded-lg bg-background p-3 font-mono text-xs">{secret}</code>
      <div className="mt-4 flex justify-end gap-2">
        <Button variant="outline" onClick={() => void copySecret()}>
          <Clipboard className="size-3.5" />
          {copied ? "已复制" : "复制密钥"}
        </Button>
        <Button onClick={onClose}>完成</Button>
      </div>
    </AnimatedDialog>
  );
}
