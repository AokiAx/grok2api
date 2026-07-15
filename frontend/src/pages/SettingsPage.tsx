import { useCallback, useEffect, useState } from "react";
import { adminApi, AdminApiError, type SettingsDocument, type SettingsSnapshot } from "@/api/client";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { cn } from "@/lib/cn";
import { SystemPage } from "@/pages/SystemPage";

function num(v: string, fallback: number) {
  const n = Number(v);
  return Number.isFinite(n) ? n : fallback;
}

type SettingsTab = "runtime" | "system";

export function SettingsPage() {
  const [tab, setTab] = useState<SettingsTab>("runtime");
  const [doc, setDoc] = useState<SettingsDocument | null>(null);
  const [snapshots, setSnapshots] = useState<SettingsSnapshot[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [message, setMessage] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const load = useCallback(async () => {
    setError(null);
    try {
      const [settings, snap] = await Promise.all([
        adminApi.settings(),
        adminApi.settingsSnapshots(12),
      ]);
      setDoc(settings);
      setSnapshots(snap.snapshots || []);
    } catch (err) {
      setError(err instanceof AdminApiError ? err.message : "加载失败");
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  async function save() {
    if (!doc) return;
    setBusy(true);
    setMessage(null);
    setError(null);
    try {
      const next = await adminApi.putSettings({
        expected_revision: doc.revision,
        pool: doc.pool,
        timeouts: doc.timeouts,
        audit: doc.audit,
        proxy: doc.proxy,
      });
      setDoc(next);
      setMessage(`已保存 revision ${next.revision}`);
      const snap = await adminApi.settingsSnapshots(12);
      setSnapshots(snap.snapshots || []);
    } catch (err) {
      setError(err instanceof AdminApiError ? err.message : "保存失败");
    } finally {
      setBusy(false);
    }
  }

  async function rollback(target: number) {
    if (!doc) return;
    if (!window.confirm(`回滚到 revision ${target}？会生成新的 revision。`)) return;
    setBusy(true);
    setError(null);
    try {
      const next = await adminApi.rollbackSettings(doc.revision, target);
      setDoc(next);
      setMessage(`已回滚到内容 revision ${target}，当前 revision ${next.revision}`);
      const snap = await adminApi.settingsSnapshots(12);
      setSnapshots(snap.snapshots || []);
    } catch (err) {
      setError(err instanceof AdminApiError ? err.message : "回滚失败");
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="space-y-8">
      <div className="flex flex-col gap-3 sm:flex-row sm:items-end sm:justify-between">
        <div>
          <h1 className="text-xl font-medium">设置</h1>
          <p className="mt-1.5 text-xs text-muted-foreground">
            {tab === "system"
              ? "运行信息、公开配置与 API 文档入口"
              : doc
                ? `版本化运行配置 · revision ${doc.revision}${doc.updated_at ? ` · 更新于 ${new Date(doc.updated_at).toLocaleString()}` : ""}`
                : "版本化运行配置"}
          </p>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <div className="flex shrink-0 rounded-full bg-secondary/60 p-0.5" aria-label="设置分区">
            <button
              type="button"
              className={cn(
                "h-7 rounded-full px-3 text-[11px] font-medium text-muted-foreground transition-colors",
                tab === "runtime" && "bg-primary text-primary-foreground",
              )}
              onClick={() => setTab("runtime")}
            >
              运行配置
            </button>
            <button
              type="button"
              className={cn(
                "h-7 rounded-full px-3 text-[11px] font-medium text-muted-foreground transition-colors",
                tab === "system" && "bg-primary text-primary-foreground",
              )}
              onClick={() => setTab("system")}
            >
              系统
            </button>
          </div>
          {tab === "runtime" ? (
            <>
              <Button variant="outline" size="sm" onClick={() => void load()} disabled={busy}>
                刷新
              </Button>
              <Button size="sm" onClick={() => void save()} disabled={busy || !doc}>
                {busy ? "保存中…" : "保存"}
              </Button>
            </>
          ) : null}
        </div>
      </div>

      {tab === "system" ? <SystemPage embedded /> : null}

      {tab === "runtime" && !doc ? (
        error ? <p className="text-sm text-destructive">{error}</p> : <p className="text-sm text-muted-foreground">加载中…</p>
      ) : null}

      {tab === "runtime" && doc ? (
        <>
      {error ? <p className="text-sm text-destructive">{error}</p> : null}
      {message ? <p className="text-sm text-emerald-600 dark:text-emerald-400">{message}</p> : null}

      <div className="grid gap-4 lg:grid-cols-2">
        <Card className="p-4 sm:p-5">
          <CardHeader>
            <CardTitle>账号池</CardTitle>
            <CardDescription>调度策略与重试窗口</CardDescription>
          </CardHeader>
          <CardContent className="mt-3 grid gap-3 sm:grid-cols-2">
            <Field label="每账号并发">
              <Input
                type="number"
                value={doc.pool.max_concurrent}
                onChange={(e) =>
                  setDoc({ ...doc, pool: { ...doc.pool, max_concurrent: num(e.target.value, 1) } })
                }
              />
            </Field>
            <Field label="单请求最大尝试">
              <Input
                type="number"
                value={doc.pool.max_attempts}
                onChange={(e) =>
                  setDoc({ ...doc, pool: { ...doc.pool, max_attempts: num(e.target.value, 3) } })
                }
              />
            </Field>
            <Field label="策略">
              <select
                className="flex h-9 w-full rounded-md border border-input bg-transparent px-3 text-sm"
                value={doc.pool.strategy}
                onChange={(e) => setDoc({ ...doc, pool: { ...doc.pool, strategy: e.target.value } })}
              >
                <option value="round-robin">round-robin</option>
                <option value="fill-first">fill-first</option>
              </select>
            </Field>
            <Field label="热集大小 (0=全量)">
              <Input
                type="number"
                value={doc.pool.active_size}
                onChange={(e) =>
                  setDoc({ ...doc, pool: { ...doc.pool, active_size: num(e.target.value, 0) } })
                }
              />
            </Field>
            <Field label="Sticky TTL (分钟)">
              <Input
                type="number"
                value={doc.pool.sticky_ttl_minutes}
                onChange={(e) =>
                  setDoc({ ...doc, pool: { ...doc.pool, sticky_ttl_minutes: num(e.target.value, 30) } })
                }
              />
            </Field>
            <label className="flex items-center gap-2 text-xs pt-6">
              <input
                type="checkbox"
                checked={doc.pool.sticky}
                onChange={(e) => setDoc({ ...doc, pool: { ...doc.pool, sticky: e.target.checked } })}
              />
              启用会话 sticky
            </label>
            <Field label="Quota 重试 (分钟)">
              <Input
                type="number"
                value={doc.pool.quota_retry_minutes}
                onChange={(e) =>
                  setDoc({ ...doc, pool: { ...doc.pool, quota_retry_minutes: num(e.target.value, 1440) } })
                }
              />
            </Field>
            <Field label="Rate 重试 (秒)">
              <Input
                type="number"
                value={doc.pool.rate_retry_seconds}
                onChange={(e) =>
                  setDoc({ ...doc, pool: { ...doc.pool, rate_retry_seconds: num(e.target.value, 45) } })
                }
              />
            </Field>
          </CardContent>
        </Card>

        <Card className="p-4 sm:p-5">
          <CardHeader>
            <CardTitle>超时与审计</CardTitle>
            <CardDescription>请求超时与审计保留</CardDescription>
          </CardHeader>
          <CardContent className="mt-3 grid gap-3 sm:grid-cols-2">
            <Field label="请求超时 (秒)">
              <Input
                type="number"
                value={doc.timeouts.request_timeout_sec}
                onChange={(e) =>
                  setDoc({
                    ...doc,
                    timeouts: { ...doc.timeouts, request_timeout_sec: num(e.target.value, 600) },
                  })
                }
              />
            </Field>
            <Field label="获取租约超时 (秒)">
              <Input
                type="number"
                value={doc.timeouts.acquire_timeout_sec}
                onChange={(e) =>
                  setDoc({
                    ...doc,
                    timeouts: { ...doc.timeouts, acquire_timeout_sec: num(e.target.value, 60) },
                  })
                }
              />
            </Field>
            <Field label="审计保留 (天)">
              <Input
                type="number"
                value={doc.audit.retention_days}
                onChange={(e) =>
                  setDoc({ ...doc, audit: { retention_days: num(e.target.value, 30) } })
                }
              />
            </Field>
          </CardContent>
        </Card>

        <Card className="p-4 sm:p-5 lg:col-span-2">
          <CardHeader>
            <CardTitle>代理（未接入运行时）</CardTitle>
            <CardDescription>
              {doc.proxy.note || "可保存与版本化，但不会影响当前出站流量。"}
            </CardDescription>
          </CardHeader>
          <CardContent className="mt-3 grid gap-3 sm:grid-cols-[1fr_auto_auto]">
            <Field label="Proxy URL">
              <Input
                value={doc.proxy.url}
                placeholder="http://127.0.0.1:8118"
                onChange={(e) => setDoc({ ...doc, proxy: { ...doc.proxy, url: e.target.value } })}
              />
            </Field>
            <label className="flex items-center gap-2 text-xs pt-6">
              <input
                type="checkbox"
                checked={doc.proxy.enabled}
                onChange={(e) => setDoc({ ...doc, proxy: { ...doc.proxy, enabled: e.target.checked } })}
              />
              标记启用
            </label>
            <div className="pt-6 text-xs text-muted-foreground mono">{doc.proxy.runtime_status}</div>
          </CardContent>
        </Card>
      </div>

      <Card className="p-4 sm:p-5">
        <CardHeader>
          <CardTitle>版本快照</CardTitle>
          <CardDescription>乐观锁 + 回滚到历史内容（生成新 revision）</CardDescription>
        </CardHeader>
        <CardContent className="mt-3">
          <ul className="divide-y divide-border/70">
            {snapshots.map((snap) => (
              <li key={snap.revision} className="flex flex-wrap items-center justify-between gap-3 py-3 first:pt-0">
                <div className="min-w-0 text-xs">
                  <div className="font-medium">revision {snap.revision}</div>
                  <div className="text-muted-foreground">
                    {snap.reason || "update"} · {snap.created_by || "—"} ·{" "}
                    {snap.created_at ? new Date(snap.created_at).toLocaleString() : ""}
                  </div>
                </div>
                <Button
                  variant="outline"
                  size="sm"
                  disabled={busy || snap.revision === doc.revision}
                  onClick={() => void rollback(snap.revision)}
                >
                  回滚到此内容
                </Button>
              </li>
            ))}
          </ul>
        </CardContent>
      </Card>
        </>
      ) : null}
    </div>
  );
}

function Field({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="space-y-1.5">
      <Label className="text-xs text-muted-foreground">{label}</Label>
      {children}
    </div>
  );
}
