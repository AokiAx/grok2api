import { useEffect, useState, type FormEvent } from "react";
import { ShieldOff, X } from "lucide-react";
import { adminApi, type ClientKey } from "@/api/client";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { ClientKeyFields, LimitDecision } from "@/pages/client-keys/ClientKeyFields";
import {
  buildClientKeyInput,
  clientKeyErrorMessage,
  draftFromClientKey,
  emptyKeyDraft,
  formatClientKeyDate,
} from "@/pages/client-keys/clientKeyDraft";

export function ClientKeyDetailDialog({
  initial,
  onClose,
  onChanged,
}: {
  initial: ClientKey;
  onClose: () => void;
  onChanged: (key: ClientKey) => void;
}) {
  const [detail, setDetail] = useState(initial);
  const [draft, setDraft] = useState(emptyKeyDraft);
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [unlimitedRPM, setUnlimitedRPM] = useState(false);
  const [unlimitedConcurrent, setUnlimitedConcurrent] = useState(false);

  useEffect(() => {
    let active = true;
    setLoading(true);
    setError(null);
    setUnlimitedRPM(false);
    setUnlimitedConcurrent(false);
    void adminApi.clientKey(initial.id).then((loaded) => {
      if (!active) return;
      setDetail(loaded);
      setDraft(draftFromClientKey(loaded));
    }).catch((failure) => {
      if (active) setError(clientKeyErrorMessage(failure, "加载密钥详情失败"));
    }).finally(() => {
      if (active) setLoading(false);
    });
    return () => {
      active = false;
    };
  }, [initial.id]);

  async function save(event: FormEvent) {
    event.preventDefault();
    const input = buildClientKeyInput(draft, {
      unlimitedRPM,
      unlimitedConcurrent,
      allModelsConfirmed: true,
    });
    if (typeof input === "string") {
      setError(input);
      return;
    }
    setBusy(true);
    setError(null);
    try {
      const updated = await adminApi.updateClientKey(detail.id, input);
      const merged = { ...detail, ...updated, secret: undefined };
      setDetail(merged);
      setDraft(draftFromClientKey(merged));
      setUnlimitedRPM(merged.rpm_limit === 0);
      setUnlimitedConcurrent(merged.max_concurrent === 0);
      onChanged(merged);
    } catch (failure) {
      setError(clientKeyErrorMessage(failure, "保存密钥设置失败"));
    } finally {
      setBusy(false);
    }
  }

  async function revoke() {
    if (!window.confirm(`确认撤销客户端密钥 ${detail.name}？撤销后无法恢复。`)) return;
    setBusy(true);
    setError(null);
    try {
      const revoked = await adminApi.revokeClientKey(detail.id);
      const merged = { ...detail, ...revoked, secret: undefined };
      setDetail(merged);
      onChanged(merged);
    } catch (failure) {
      setError(clientKeyErrorMessage(failure, "撤销客户端密钥失败"));
    } finally {
      setBusy(false);
    }
  }

  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/35 p-4"
      role="dialog"
      aria-modal="true"
      aria-labelledby="client-key-detail-title"
    >
      <Card className="max-h-[calc(100vh-2rem)] w-full max-w-2xl overflow-y-auto shadow-2xl">
        <CardContent className="p-5">
          <div className="mb-5 flex items-start justify-between gap-4">
            <div>
              <div className="flex items-center gap-2">
                <h2 id="client-key-detail-title" className="text-base font-medium">密钥详情</h2>
                <Badge tone={detail.revoked_at ? "danger" : "success"}>
                  {detail.revoked_at ? "已撤销" : "有效"}
                </Badge>
              </div>
              <p className="mt-1 font-mono text-[11px] text-muted-foreground">
                {detail.id} · {detail.key_prefix}
              </p>
            </div>
            <Button size="icon" variant="ghost" aria-label="关闭密钥详情" onClick={onClose}>
              <X className="size-4" />
            </Button>
          </div>
          {loading ? <p className="text-xs text-muted-foreground">正在加载详情…</p> : (
            <form className="space-y-4" onSubmit={save}>
              <ClientKeyFields
                draft={draft}
                onChange={setDraft}
                nameLabel="密钥名称"
                disabled={Boolean(detail.revoked_at)}
              />
              <div className="grid gap-3 sm:grid-cols-2">
                <LimitDecision
                  id="detail-rpm"
                  label="每分钟请求数"
                  unlimitedLabel="RPM 不限"
                  value={draft.rpmLimit}
                  unlimited={unlimitedRPM}
                  disabled={Boolean(detail.revoked_at)}
                  min={0}
                  onValue={(value) => setDraft((current) => ({ ...current, rpmLimit: value }))}
                  onUnlimited={setUnlimitedRPM}
                />
                <LimitDecision
                  id="detail-concurrency"
                  label="最大并发"
                  unlimitedLabel="并发不限"
                  value={draft.maxConcurrent}
                  unlimited={unlimitedConcurrent}
                  disabled={Boolean(detail.revoked_at)}
                  min={0}
                  onValue={(value) => setDraft((current) => ({ ...current, maxConcurrent: value }))}
                  onUnlimited={setUnlimitedConcurrent}
                />
              </div>
              <dl className="grid gap-2 rounded-lg bg-background p-3 text-[11px] sm:grid-cols-2">
                <KeyFact label="来源" value={detail.origin} />
                <KeyFact label="最后使用" value={formatClientKeyDate(detail.last_used_at)} />
                <KeyFact label="创建时间" value={formatClientKeyDate(detail.created_at)} />
                <KeyFact label="到期时间" value={formatClientKeyDate(detail.expires_at)} />
              </dl>
              {error ? <p className="text-xs text-destructive" role="alert">{error}</p> : null}
              <div className="flex flex-col-reverse justify-between gap-2 border-t border-border/70 pt-4 sm:flex-row">
                <Button
                  type="button"
                  variant="destructive"
                  disabled={busy || Boolean(detail.revoked_at)}
                  onClick={() => void revoke()}
                >
                  <ShieldOff className="size-3.5" />撤销密钥
                </Button>
                <Button type="submit" disabled={busy || Boolean(detail.revoked_at)}>
                  {busy ? "保存中…" : "保存修改"}
                </Button>
              </div>
            </form>
          )}
        </CardContent>
      </Card>
    </div>
  );
}

function KeyFact({ label, value }: { label: string; value: string }) {
  return <div><dt className="text-muted-foreground">{label}</dt><dd className="mt-0.5">{value}</dd></div>;
}
