import { useCallback, useEffect, useState, type FormEvent } from "react";
import { adminApi, type ClientKey } from "@/api/client";
import { Button } from "@/components/ui/button";
import { AnimatedDialog } from "@/components/ui/AnimatedDialog";
import { ClientKeyFields, LimitDecision } from "@/pages/client-keys/ClientKeyFields";
import {
  buildClientKeyInput,
  clientKeyErrorMessage,
  emptyKeyDraft,
  type KeyDraft,
} from "@/pages/client-keys/clientKeyDraft";

const FALLBACK_DEFAULTS = { default_rpm_limit: 120, default_max_concurrent: 4 };

function draftWithDefaults(rpm: number, concurrent: number): KeyDraft {
  const draft = emptyKeyDraft();
  return {
    ...draft,
    rpmLimit: rpm > 0 ? String(rpm) : "",
    maxConcurrent: concurrent > 0 ? String(concurrent) : "",
  };
}

export function CreateClientKeyDialog({
  onClose,
  onCreated,
}: {
  onClose: () => void;
  onCreated: (created: ClientKey & { secret: string }) => void;
}) {
  const [draft, setDraft] = useState(() =>
    draftWithDefaults(FALLBACK_DEFAULTS.default_rpm_limit, FALLBACK_DEFAULTS.default_max_concurrent),
  );
  const [catalogModelIds, setCatalogModelIds] = useState<string[]>([]);
  const [unlimitedRPM, setUnlimitedRPM] = useState(false);
  const [unlimitedConcurrent, setUnlimitedConcurrent] = useState(false);
  const [defaultsReady, setDefaultsReady] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [defaultsHint, setDefaultsHint] = useState("默认 RPM 120 · 并发 4（可在设置中修改）");

  const handleCatalogChange = useCallback((ids: string[]) => {
    setCatalogModelIds(ids);
  }, []);

  useEffect(() => {
    let active = true;
    void adminApi
      .settings()
      .then((settings) => {
        if (!active) return;
        const rpm = settings.client_keys?.default_rpm_limit ?? FALLBACK_DEFAULTS.default_rpm_limit;
        const concurrent =
          settings.client_keys?.default_max_concurrent ?? FALLBACK_DEFAULTS.default_max_concurrent;
        setDraft(draftWithDefaults(rpm, concurrent));
        setUnlimitedRPM(rpm === 0);
        setUnlimitedConcurrent(concurrent === 0);
        setDefaultsHint(
          `默认来自设置：RPM ${rpm === 0 ? "不限" : rpm} · 并发 ${concurrent === 0 ? "不限" : concurrent}`,
        );
      })
      .catch(() => {
        /* keep hardcoded product defaults */
      })
      .finally(() => {
        if (active) setDefaultsReady(true);
      });
    return () => {
      active = false;
    };
  }, []);

  async function submit(event: FormEvent) {
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
    <AnimatedDialog
      open
      onClose={onClose}
      title="创建密钥"
      description={<>选择可用模型并确认限额。{defaultsHint}</>}
      maxWidthClassName="max-w-xl"
    >
          <form className="space-y-4" onSubmit={submit}>
            <ClientKeyFields
              draft={draft}
              onChange={setDraft}
              nameLabel="名称"
              onCatalogChange={handleCatalogChange}
            />
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
              <Button type="submit" disabled={busy || !defaultsReady}>
                {busy ? "生成中…" : "生成密钥"}
              </Button>
            </div>
          </form>
    </AnimatedDialog>
  );
}
