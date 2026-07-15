import { useState, type FormEvent } from "react";
import { X } from "lucide-react";
import { adminApi, type ClientKey } from "@/api/client";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { ClientKeyFields, LimitDecision } from "@/pages/client-keys/ClientKeyFields";
import {
  buildClientKeyInput,
  clientKeyErrorMessage,
  emptyKeyDraft,
} from "@/pages/client-keys/clientKeyDraft";

export function CreateClientKeyDialog({
  onClose,
  onCreated,
}: {
  onClose: () => void;
  onCreated: (created: ClientKey & { secret: string }) => void;
}) {
  const [draft, setDraft] = useState(emptyKeyDraft);
  const [allModelsConfirmed, setAllModelsConfirmed] = useState(false);
  const [unlimitedRPM, setUnlimitedRPM] = useState(false);
  const [unlimitedConcurrent, setUnlimitedConcurrent] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  async function submit(event: FormEvent) {
    event.preventDefault();
    const input = buildClientKeyInput(draft, {
      unlimitedRPM,
      unlimitedConcurrent,
      allModelsConfirmed: draft.modelPolicy !== "all" || allModelsConfirmed,
    });
    if (typeof input === "string") {
      setError(input);
      return;
    }
    setBusy(true);
    setError(null);
    try {
      const created = await adminApi.createClientKey(input);
      if (!created.secret) throw new Error("创建响应未返回一次性密钥");
      onCreated(created as ClientKey & { secret: string });
    } catch (failure) {
      setError(clientKeyErrorMessage(failure, "创建客户端密钥失败"));
    } finally {
      setBusy(false);
    }
  }

  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/35 p-4"
      role="dialog"
      aria-modal="true"
      aria-labelledby="create-client-key-title"
    >
      <Card className="max-h-[calc(100vh-2rem)] w-full max-w-xl overflow-y-auto shadow-2xl">
        <CardContent className="p-5">
          <div className="mb-5 flex items-start justify-between gap-4">
            <div>
              <h2 id="create-client-key-title" className="text-base font-medium">创建客户端密钥</h2>
              <p className="mt-1 text-xs text-muted-foreground">
                所有权限与限额都必须明确选择，避免意外生成无限权限密钥。
              </p>
            </div>
            <Button size="icon" variant="ghost" aria-label="关闭创建密钥" onClick={onClose}>
              <X className="size-4" />
            </Button>
          </div>
          <form className="space-y-4" onSubmit={submit}>
            <ClientKeyFields draft={draft} onChange={setDraft} nameLabel="名称" />
            {draft.modelPolicy === "all" ? (
              <label className="flex items-start gap-2 rounded-md border border-amber-500/20 bg-amber-500/8 p-3 text-xs leading-5">
                <input
                  type="checkbox"
                  checked={allModelsConfirmed}
                  onChange={(event) => setAllModelsConfirmed(event.target.checked)}
                />
                我确认此密钥可访问全部模型
              </label>
            ) : null}
            <LimitDecision
              id="create-rpm"
              label="每分钟请求数"
              unlimitedLabel="RPM 不限"
              value={draft.rpmLimit}
              unlimited={unlimitedRPM}
              onValue={(value) => setDraft((current) => ({ ...current, rpmLimit: value }))}
              onUnlimited={setUnlimitedRPM}
            />
            <LimitDecision
              id="create-concurrency"
              label="最大并发"
              unlimitedLabel="并发不限"
              value={draft.maxConcurrent}
              unlimited={unlimitedConcurrent}
              onValue={(value) => setDraft((current) => ({ ...current, maxConcurrent: value }))}
              onUnlimited={setUnlimitedConcurrent}
            />
            {error ? <p className="text-xs text-destructive" role="alert">{error}</p> : null}
            <div className="flex justify-end gap-2 pt-2">
              <Button type="button" variant="outline" onClick={onClose}>取消</Button>
              <Button type="submit" disabled={busy}>{busy ? "生成中…" : "生成密钥"}</Button>
            </div>
          </form>
        </CardContent>
      </Card>
    </div>
  );
}
