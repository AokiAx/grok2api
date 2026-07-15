import { useCallback, useEffect, useState, type FormEvent } from "react";
import { LoaderCircle, ShieldOff } from "lucide-react";
import { adminApi, type ClientKey } from "@/api/client";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { AnimatedDialog } from "@/components/ui/AnimatedDialog";
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
  const [catalogModelIds, setCatalogModelIds] = useState<string[]>([]);
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [unlimitedRPM, setUnlimitedRPM] = useState(false);
  const [unlimitedConcurrent, setUnlimitedConcurrent] = useState(false);

  const handleCatalogChange = useCallback((ids: string[]) => {
    setCatalogModelIds(ids);
  }, []);

  useEffect(() => {
    let active = true;
    setLoading(true);
    setError(null);
    setUnlimitedRPM(false);
    setUnlimitedConcurrent(false);
    void adminApi.clientKey(initial.id).then((loaded) => {
      if (!active) return;
      setDetail(loaded);
      setDraft(draftFromClientKey(loaded, catalogModelIds));
      setUnlimitedRPM(loaded.rpm_limit === 0);
      setUnlimitedConcurrent(loaded.max_concurrent === 0);
    }).catch((failure) => {
      if (active) setError(clientKeyErrorMessage(failure, "加载密钥详情失败"));
    }).finally(() => {
      if (active) setLoading(false);
    });
    return () => {
      active = false;
    };
    // catalog is applied separately when models finish loading
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [initial.id]);

  // Rehydrate "all" keys once model catalog is available.
  useEffect(() => {
    if (!catalogModelIds.length || detail.model_policy !== "all") return;
    setDraft((current) => {
      if (current.modelScopes) return current;
      return draftFromClientKey(detail, catalogModelIds);
    });
  }, [catalogModelIds, detail]);

  async function save(event: FormEvent) {
    event.preventDefault();
    const input = buildClientKeyInput(draft, {
      unlimitedRPM,
      unlimitedConcurrent,
      catalogModelIds,
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
      setDraft(draftFromClientKey(merged, catalogModelIds));
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
    <AnimatedDialog
      open
      onClose={onClose}
      title="密钥详情"
      description={
        <span className="inline-flex flex-wrap items-center gap-2">
          <Badge tone={detail.revoked_at ? "danger" : "success"}>
            {detail.revoked_at ? "已撤销" : "有效"}
          </Badge>
          <span className="font-mono text-[11px]">{detail.id} · {detail.key_prefix}</span>
        </span>
      }
      maxWidthClassName="max-w-2xl"
      contentKey={loading ? "loading" : detail.id}
    >
          {loading ? (
            <p className="inline-flex items-center gap-1.5 text-xs text-muted-foreground">
              <LoaderCircle className="size-3.5 animate-spin" />
              正在加载详情…
            </p>
          ) : (
            <form className="space-y-4" onSubmit={save}>
              <ClientKeyFields
                draft={draft}
                onChange={setDraft}
                nameLabel="密钥名称"
                disabled={Boolean(detail.revoked_at)}
                onCatalogChange={handleCatalogChange}
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
                <KeyFact label="创建时间" value={formatClientKeyDate(detail.created_at)} />
                <KeyFact label="最近使用" value={formatClientKeyDate(detail.last_used_at)} />
                <KeyFact label="更新时间" value={formatClientKeyDate(detail.updated_at)} />
                {detail.revoked_at ? (
                  <KeyFact label="撤销时间" value={formatClientKeyDate(detail.revoked_at)} />
                ) : null}
              </dl>
              {error ? <p className="text-xs text-destructive" role="alert">{error}</p> : null}
              <div className="flex flex-wrap justify-between gap-2 pt-1">
                <Button
                  type="button"
                  variant="destructive"
                  disabled={busy || Boolean(detail.revoked_at)}
                  onClick={() => void revoke()}
                >
                  <ShieldOff className="size-3.5" />
                  撤销密钥
                </Button>
                <div className="flex gap-2">
                  <Button type="button" variant="outline" onClick={onClose}>关闭</Button>
                  <Button type="submit" disabled={busy || Boolean(detail.revoked_at)}>
                    {busy ? "保存中…" : "保存修改"}
                  </Button>
                </div>
              </div>
            </form>
          )}
    </AnimatedDialog>
  );
}

function KeyFact({ label, value }: { label: string; value: string }) {
  return (
    <div className="min-w-0">
      <dt className="text-muted-foreground">{label}</dt>
      <dd className="mt-0.5 truncate font-mono">{value}</dd>
    </div>
  );
}
