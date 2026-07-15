import { useState } from "react";
import { Clipboard, KeyRound } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";

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
    <div
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/35 p-4"
      role="dialog"
      aria-modal="true"
      aria-labelledby="created-secret-title"
    >
      <Card className="w-full max-w-lg shadow-2xl">
        <CardContent className="p-5">
          <div className="flex size-9 items-center justify-center rounded-full bg-emerald-500/10 text-emerald-600">
            <KeyRound className="size-4" />
          </div>
          <h2 id="created-secret-title" className="mt-4 text-base font-medium">客户端密钥已创建</h2>
          <p className="mt-1 text-xs leading-5 text-muted-foreground">
            请立即保存，关闭后无法再次查看此 secret。
          </p>
          <code className="mt-4 block break-all rounded-lg bg-background p-3 font-mono text-xs">{secret}</code>
          <div className="mt-4 flex justify-end gap-2">
            <Button variant="outline" onClick={() => void copySecret()}>
              <Clipboard className="size-3.5" />
              {copied ? "已复制" : "复制密钥"}
            </Button>
            <Button onClick={onClose}>完成</Button>
          </div>
        </CardContent>
      </Card>
    </div>
  );
}
