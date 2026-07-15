import { useCallback, useEffect, useState, type FormEvent } from "react";
import {
  ChevronLeft,
  ChevronRight,
  CircleAlert,
  Clipboard,
  KeyRound,
  Plus,
  RefreshCw,
  Search,
  ShieldOff,
  X,
} from "lucide-react";
import {
  adminApi,
  AdminApiError,
  type ClientKey,
  type ClientKeyInput,
  type ClientKeyModelPolicy,
  type ClientKeysPage as PageData,
} from "@/api/client";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { cn } from "@/lib/cn";

type KeyDraft = {
  name: string;
  modelPolicy: ClientKeyModelPolicy | "";
  modelScopes: string;
  rpmLimit: string;
  maxConcurrent: string;
  expiresAt: string;
};

const emptyDraft: KeyDraft = {
  name: "",
  modelPolicy: "",
  modelScopes: "",
  rpmLimit: "",
  maxConcurrent: "",
  expiresAt: "",
};

function formatDate(value?: string | null) {
  if (!value) return "—";
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? value : date.toLocaleString();
}

function parseScopes(value: string): string[] {
  return Array.from(new Set(
    value
      .split(/[\n,]/)
      .map((item) => item.trim().toLowerCase())
      .filter(Boolean),
  ));
}

function expiresPayload(value: string): string | null {
  if (!value) return null;
  const parsed = new Date(value);
  return Number.isNaN(parsed.getTime()) ? null : parsed.toISOString();
}

function localDateTime(value?: string | null): string {
  if (!value) return "";
  const parsed = new Date(value);
  if (Number.isNaN(parsed.getTime())) return "";
  const local = new Date(parsed.getTime() - parsed.getTimezoneOffset() * 60_000);
  return local.toISOString().slice(0, 16);
}

function apiMessage(error: unknown, fallback: string): string {
  return error instanceof AdminApiError ? error.message || fallback : fallback;
}

export function ClientKeysPage() {
  const [qDraft, setQDraft] = useState("");
  const [q, setQ] = useState("");
  const [origin, setOrigin] = useState("");
  const [page, setPage] = useState(1);
  const [data, setData] = useState<PageData | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [createOpen, setCreateOpen] = useState(false);
  const [createDraft, setCreateDraft] = useState<KeyDraft>(emptyDraft);
  const [createAllConfirmed, setCreateAllConfirmed] = useState(false);
  const [unlimitedRPM, setUnlimitedRPM] = useState(false);
  const [unlimitedConcurrent, setUnlimitedConcurrent] = useState(false);
  const [createError, setCreateError] = useState<string | null>(null);
  const [createBusy, setCreateBusy] = useState(false);
  const [createdSecret, setCreatedSecret] = useState<{ name: string; secret: string } | null>(null);
  const [copied, setCopied] = useState(false);
  const [detail, setDetail] = useState<ClientKey | null>(null);
  const [detailDraft, setDetailDraft] = useState<KeyDraft>(emptyDraft);
  const [detailLoading, setDetailLoading] = useState(false);
  const [detailBusy, setDetailBusy] = useState(false);
  const [detailError, setDetailError] = useState<string | null>(null);

  const load = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      setData(await adminApi.clientKeys({ q, origin, page, page_size: 20 }));
    } catch (loadError) {
      setError(apiMessage(loadError, "加载客户端密钥失败"));
    } finally {
      setLoading(false);
    }
  }, [origin, page, q]);

  useEffect(() => {
    void load();
  }, [load]);

  function resetCreate() {
    setCreateDraft(emptyDraft);
    setCreateAllConfirmed(false);
    setUnlimitedRPM(false);
    setUnlimitedConcurrent(false);
    setCreateError(null);
  }

  function inputFromDraft(
    draft: KeyDraft,
    rpmUnlimited: boolean,
    concurrentUnlimited: boolean,
    requireAllConfirmation: boolean,
  ): ClientKeyInput | string {
    if (!draft.modelPolicy) return "请选择模型权限";
    if (!draft.name.trim()) return "请输入密钥名称";
    const scopes = parseScopes(draft.modelScopes);
    if (draft.modelPolicy === "all" && !requireAllConfirmation) return "请确认允许访问全部模型";
    if (draft.modelPolicy === "allowlist" && scopes.length === 0) return "请至少填写一个允许的模型";
    const rpm = Number(draft.rpmLimit);
    if (!rpmUnlimited && (!draft.rpmLimit || !Number.isInteger(rpm) || rpm < 1)) {
      return "请输入大于 0 的 RPM，或主动选择 RPM 不限";
    }
    const concurrent = Number(draft.maxConcurrent);
    if (!concurrentUnlimited && (!draft.maxConcurrent || !Number.isInteger(concurrent) || concurrent < 1)) {
      return "请输入大于 0 的最大并发，或主动选择并发不限";
    }
    return {
      name: draft.name.trim(),
      model_policy: draft.modelPolicy,
      model_scopes: draft.modelPolicy === "allowlist" ? scopes : [],
      rpm_limit: rpmUnlimited ? 0 : rpm,
      max_concurrent: concurrentUnlimited ? 0 : concurrent,
      expires_at: expiresPayload(draft.expiresAt),
    };
  }

  async function createKey(event: FormEvent) {
    event.preventDefault();
    const input = inputFromDraft(
      createDraft,
      unlimitedRPM,
      unlimitedConcurrent,
      createDraft.modelPolicy !== "all" || createAllConfirmed,
    );
    if (typeof input === "string") {
      setCreateError(input);
      return;
    }
    setCreateBusy(true);
    setCreateError(null);
    try {
      const created = await adminApi.createClientKey(input);
      if (!created.secret) throw new Error("创建响应未返回一次性密钥");
      const { secret, ...safeKey } = created;
      setData((current) => current ? {
        ...current,
        items: [safeKey, ...current.items.filter((item) => item.id !== safeKey.id)],
        total: current.total + (current.items.some((item) => item.id === safeKey.id) ? 0 : 1),
      } : current);
      setCreateOpen(false);
      setCreatedSecret({ name: created.name, secret });
      setCopied(false);
      resetCreate();
    } catch (createFailure) {
      setCreateError(apiMessage(createFailure, "创建客户端密钥失败"));
    } finally {
      setCreateBusy(false);
    }
  }

  async function openDetail(item: ClientKey) {
    setDetailLoading(true);
    setDetailError(null);
    setDetail(item);
    try {
      const loaded = await adminApi.clientKey(item.id);
      setDetail(loaded);
      setDetailDraft({
        name: loaded.name,
        modelPolicy: loaded.model_policy,
        modelScopes: loaded.model_scopes.join(", "),
        rpmLimit: String(loaded.rpm_limit),
        maxConcurrent: String(loaded.max_concurrent),
        expiresAt: localDateTime(loaded.expires_at),
      });
    } catch (detailFailure) {
      setDetailError(apiMessage(detailFailure, "加载密钥详情失败"));
    } finally {
      setDetailLoading(false);
    }
  }

  async function saveDetail(event: FormEvent) {
    event.preventDefault();
    if (!detail) return;
    const input = inputFromDraft(
      detailDraft,
      detailDraft.rpmLimit === "0",
      detailDraft.maxConcurrent === "0",
      true,
    );
    if (typeof input === "string") {
      setDetailError(input);
      return;
    }
    setDetailBusy(true);
    setDetailError(null);
    try {
      const updated = await adminApi.updateClientKey(detail.id, input);
      const merged = { ...detail, ...updated, secret: undefined };
      setDetail(merged);
      setData((current) => current ? {
        ...current,
        items: current.items.map((item) => item.id === merged.id ? merged : item),
      } : current);
    } catch (updateFailure) {
      setDetailError(apiMessage(updateFailure, "保存密钥设置失败"));
    } finally {
      setDetailBusy(false);
    }
  }

  async function revokeDetail() {
    if (!detail || !window.confirm(`确认撤销客户端密钥 ${detail.name}？撤销后无法恢复。`)) return;
    setDetailBusy(true);
    setDetailError(null);
    try {
      const revoked = await adminApi.revokeClientKey(detail.id);
      const merged = { ...detail, ...revoked, secret: undefined };
      setDetail(merged);
      setData((current) => current ? {
        ...current,
        items: current.items.map((item) => item.id === merged.id ? merged : item),
      } : current);
    } catch (revokeFailure) {
      setDetailError(apiMessage(revokeFailure, "撤销客户端密钥失败"));
    } finally {
      setDetailBusy(false);
    }
  }

  async function copySecret() {
    if (!createdSecret) return;
    try {
      await navigator.clipboard.writeText(createdSecret.secret);
      setCopied(true);
    } catch {
      setCopied(false);
    }
  }

  const items = data?.items || [];
  const totalPages = Math.max(1, Math.ceil((data?.total || 0) / (data?.page_size || 20)));

  return (
    <div className="space-y-5">
      <header className="flex flex-col gap-3 sm:flex-row sm:items-start sm:justify-between">
        <div>
          <h1 className="text-xl font-medium tracking-tight">客户端密钥</h1>
          <p className="mt-1 text-xs text-muted-foreground">
            为客户端分配模型权限、RPM 和并发边界；secret 仅在创建时展示一次。
          </p>
        </div>
        <div className="flex gap-2">
          <Button variant="outline" size="sm" onClick={() => void load()} disabled={loading}>
            <RefreshCw className={cn("size-3.5", loading && "animate-spin")} />
            刷新
          </Button>
          <Button size="sm" onClick={() => { resetCreate(); setCreateOpen(true); }}>
            <Plus className="size-3.5" />
            创建客户端密钥
          </Button>
        </div>
      </header>

      <Card>
        <CardContent className="p-3">
          <form
            className="flex flex-col gap-2 sm:flex-row"
            onSubmit={(event) => {
              event.preventDefault();
              setPage(1);
              setQ(qDraft.trim());
            }}
          >
            <div className="relative min-w-0 flex-1">
              <Label className="sr-only" htmlFor="client-key-search">搜索客户端密钥</Label>
              <Search className="pointer-events-none absolute left-3 top-1/2 size-3.5 -translate-y-1/2 text-muted-foreground" />
              <Input
                id="client-key-search"
                className="pl-8"
                value={qDraft}
                onChange={(event) => setQDraft(event.target.value)}
                placeholder="搜索名称或密钥前缀"
              />
            </div>
            <Label className="sr-only" htmlFor="client-key-origin">来源</Label>
            <select
              id="client-key-origin"
              className="h-8 rounded-md border border-input bg-secondary/55 px-3 text-xs outline-none focus-visible:ring-1 focus-visible:ring-ring"
              value={origin}
              onChange={(event) => { setOrigin(event.target.value); setPage(1); }}
            >
              <option value="">全部来源</option>
              <option value="managed">面板创建</option>
              <option value="config_api_key">历史配置</option>
            </select>
            <Button type="submit" size="sm">查询</Button>
          </form>
        </CardContent>
      </Card>

      {error ? (
        <div className="flex items-center gap-2 rounded-lg bg-destructive/8 px-3 py-2 text-xs text-destructive" role="alert">
          <CircleAlert className="size-4 shrink-0" />
          {error}
        </div>
      ) : null}

      <Card className="overflow-hidden">
        <div className="overflow-x-auto">
          <table className="w-full min-w-[900px] text-left text-xs">
            <thead className="border-b border-border/80 bg-background text-[11px] text-muted-foreground">
              <tr>
                <th className="px-3 py-2.5 font-medium">名称</th>
                <th className="px-3 py-2.5 font-medium">状态</th>
                <th className="px-3 py-2.5 font-medium">模型权限</th>
                <th className="px-3 py-2.5 text-right font-medium">RPM</th>
                <th className="px-3 py-2.5 text-right font-medium">并发</th>
                <th className="px-3 py-2.5 font-medium">最后使用</th>
                <th className="px-3 py-2.5 text-right font-medium">操作</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-border/70">
              {items.map((item) => (
                <tr key={item.id} className="transition-colors hover:bg-background">
                  <td className="px-3 py-3">
                    <div className="font-medium text-foreground">{item.name}</div>
                    <div className="mt-0.5 font-mono text-[11px] text-muted-foreground">{item.key_prefix}</div>
                  </td>
                  <td className="px-3 py-3">
                    <Badge tone={item.revoked_at ? "danger" : "success"}>{item.revoked_at ? "已撤销" : "有效"}</Badge>
                  </td>
                  <td className="px-3 py-3">
                    {item.model_policy === "all" ? "全部模型" : `${item.model_scopes.length} 个模型`}
                  </td>
                  <td className="px-3 py-3 text-right tabular-nums">{item.rpm_limit === 0 ? "不限" : item.rpm_limit}</td>
                  <td className="px-3 py-3 text-right tabular-nums">{item.max_concurrent === 0 ? "不限" : item.max_concurrent}</td>
                  <td className="px-3 py-3 text-muted-foreground">{formatDate(item.last_used_at)}</td>
                  <td className="px-3 py-3 text-right">
                    <Button
                      variant="ghost"
                      size="sm"
                      aria-label={`查看 ${item.name}`}
                      onClick={() => void openDetail(item)}
                    >
                      查看
                    </Button>
                  </td>
                </tr>
              ))}
              {!loading && items.length === 0 ? (
                <tr><td colSpan={7} className="px-3 py-12 text-center text-muted-foreground">暂无客户端密钥</td></tr>
              ) : null}
              {loading && items.length === 0 ? (
                <tr><td colSpan={7} className="px-3 py-12 text-center text-muted-foreground">正在加载客户端密钥…</td></tr>
              ) : null}
            </tbody>
          </table>
        </div>
        <div className="flex items-center justify-between border-t border-border/70 bg-background px-3 py-2.5">
          <span className="text-[11px] text-muted-foreground">共 {data?.total ?? "—"} 条 · 第 {page}/{totalPages} 页</span>
          <div className="flex gap-1">
            <Button variant="outline" size="sm" disabled={page <= 1 || loading} onClick={() => setPage((value) => Math.max(1, value - 1))}>
              <ChevronLeft className="size-3.5" />上一页
            </Button>
            <Button variant="outline" size="sm" disabled={page >= totalPages || loading} onClick={() => setPage((value) => value + 1)}>
              下一页<ChevronRight className="size-3.5" />
            </Button>
          </div>
        </div>
      </Card>

      {createOpen ? (
        <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/35 p-4" role="dialog" aria-modal="true" aria-labelledby="create-client-key-title">
          <Card className="max-h-[calc(100vh-2rem)] w-full max-w-xl overflow-y-auto shadow-2xl">
            <CardContent className="p-5">
              <div className="mb-5 flex items-start justify-between gap-4">
                <div>
                  <h2 id="create-client-key-title" className="text-base font-medium">创建客户端密钥</h2>
                  <p className="mt-1 text-xs text-muted-foreground">所有权限与限额都必须明确选择，避免意外生成无限权限密钥。</p>
                </div>
                <Button size="icon" variant="ghost" aria-label="关闭创建密钥" onClick={() => setCreateOpen(false)}><X className="size-4" /></Button>
              </div>
              <form className="space-y-4" onSubmit={createKey}>
                <KeyFields draft={createDraft} onChange={setCreateDraft} nameLabel="名称" />
                {createDraft.modelPolicy === "all" ? (
                  <label className="flex items-start gap-2 rounded-md border border-amber-500/20 bg-amber-500/8 p-3 text-xs leading-5">
                    <input type="checkbox" checked={createAllConfirmed} onChange={(event) => setCreateAllConfirmed(event.target.checked)} />
                    我确认此密钥可访问全部模型
                  </label>
                ) : null}
                <LimitDecision
                  id="create-rpm"
                  label="每分钟请求数"
                  unlimitedLabel="RPM 不限"
                  value={createDraft.rpmLimit}
                  unlimited={unlimitedRPM}
                  onValue={(value) => setCreateDraft((current) => ({ ...current, rpmLimit: value }))}
                  onUnlimited={setUnlimitedRPM}
                />
                <LimitDecision
                  id="create-concurrency"
                  label="最大并发"
                  unlimitedLabel="并发不限"
                  value={createDraft.maxConcurrent}
                  unlimited={unlimitedConcurrent}
                  onValue={(value) => setCreateDraft((current) => ({ ...current, maxConcurrent: value }))}
                  onUnlimited={setUnlimitedConcurrent}
                />
                {createError ? <p className="text-xs text-destructive" role="alert">{createError}</p> : null}
                <div className="flex justify-end gap-2 pt-2">
                  <Button type="button" variant="outline" onClick={() => setCreateOpen(false)}>取消</Button>
                  <Button type="submit" disabled={createBusy}>{createBusy ? "生成中…" : "生成密钥"}</Button>
                </div>
              </form>
            </CardContent>
          </Card>
        </div>
      ) : null}

      {createdSecret ? (
        <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/35 p-4" role="dialog" aria-modal="true" aria-labelledby="created-secret-title">
          <Card className="w-full max-w-lg shadow-2xl">
            <CardContent className="p-5">
              <div className="flex size-9 items-center justify-center rounded-full bg-emerald-500/10 text-emerald-600"><KeyRound className="size-4" /></div>
              <h2 id="created-secret-title" className="mt-4 text-base font-medium">客户端密钥已创建</h2>
              <p className="mt-1 text-xs leading-5 text-muted-foreground">请立即保存，关闭后无法再次查看此 secret。</p>
              <code className="mt-4 block break-all rounded-lg bg-background p-3 font-mono text-xs">{createdSecret.secret}</code>
              <div className="mt-4 flex justify-end gap-2">
                <Button variant="outline" onClick={() => void copySecret()}><Clipboard className="size-3.5" />{copied ? "已复制" : "复制密钥"}</Button>
                <Button onClick={() => { setCreatedSecret(null); setCopied(false); }}>完成</Button>
              </div>
            </CardContent>
          </Card>
        </div>
      ) : null}

      {detail ? (
        <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/35 p-4" role="dialog" aria-modal="true" aria-labelledby="client-key-detail-title">
          <Card className="max-h-[calc(100vh-2rem)] w-full max-w-2xl overflow-y-auto shadow-2xl">
            <CardContent className="p-5">
              <div className="mb-5 flex items-start justify-between gap-4">
                <div>
                  <div className="flex items-center gap-2">
                    <h2 id="client-key-detail-title" className="text-base font-medium">密钥详情</h2>
                    <Badge tone={detail.revoked_at ? "danger" : "success"}>{detail.revoked_at ? "已撤销" : "有效"}</Badge>
                  </div>
                  <p className="mt-1 font-mono text-[11px] text-muted-foreground">{detail.id} · {detail.key_prefix}</p>
                </div>
                <Button size="icon" variant="ghost" aria-label="关闭密钥详情" onClick={() => setDetail(null)}><X className="size-4" /></Button>
              </div>
              {detailLoading ? <p className="text-xs text-muted-foreground">正在加载详情…</p> : (
                <form className="space-y-4" onSubmit={saveDetail}>
                  <KeyFields draft={detailDraft} onChange={setDetailDraft} nameLabel="密钥名称" disabled={Boolean(detail.revoked_at)} />
                  <div className="grid gap-3 sm:grid-cols-2">
                    <div className="space-y-2">
                      <Label htmlFor="detail-rpm">每分钟请求数</Label>
                      <Input id="detail-rpm" type="number" min={0} value={detailDraft.rpmLimit} disabled={Boolean(detail.revoked_at)} onChange={(event) => setDetailDraft((current) => ({ ...current, rpmLimit: event.target.value }))} />
                      <p className="text-[11px] text-muted-foreground">0 表示不限</p>
                    </div>
                    <div className="space-y-2">
                      <Label htmlFor="detail-concurrency">最大并发</Label>
                      <Input id="detail-concurrency" type="number" min={0} value={detailDraft.maxConcurrent} disabled={Boolean(detail.revoked_at)} onChange={(event) => setDetailDraft((current) => ({ ...current, maxConcurrent: event.target.value }))} />
                      <p className="text-[11px] text-muted-foreground">0 表示不限</p>
                    </div>
                  </div>
                  <dl className="grid gap-2 rounded-lg bg-background p-3 text-[11px] sm:grid-cols-2">
                    <div><dt className="text-muted-foreground">来源</dt><dd className="mt-0.5">{detail.origin}</dd></div>
                    <div><dt className="text-muted-foreground">最后使用</dt><dd className="mt-0.5">{formatDate(detail.last_used_at)}</dd></div>
                    <div><dt className="text-muted-foreground">创建时间</dt><dd className="mt-0.5">{formatDate(detail.created_at)}</dd></div>
                    <div><dt className="text-muted-foreground">到期时间</dt><dd className="mt-0.5">{formatDate(detail.expires_at)}</dd></div>
                  </dl>
                  {detailError ? <p className="text-xs text-destructive" role="alert">{detailError}</p> : null}
                  <div className="flex flex-col-reverse justify-between gap-2 border-t border-border/70 pt-4 sm:flex-row">
                    <Button type="button" variant="destructive" disabled={detailBusy || Boolean(detail.revoked_at)} onClick={() => void revokeDetail()}>
                      <ShieldOff className="size-3.5" />撤销密钥
                    </Button>
                    <Button type="submit" disabled={detailBusy || Boolean(detail.revoked_at)}>{detailBusy ? "保存中…" : "保存修改"}</Button>
                  </div>
                </form>
              )}
            </CardContent>
          </Card>
        </div>
      ) : null}
    </div>
  );
}

function KeyFields({
  draft,
  onChange,
  nameLabel,
  disabled = false,
}: {
  draft: KeyDraft;
  onChange: (next: KeyDraft) => void;
  nameLabel: string;
  disabled?: boolean;
}) {
  const prefix = nameLabel === "名称" ? "create" : "detail";
  return (
    <>
      <div className="space-y-2">
        <Label htmlFor={`${prefix}-key-name`}>{nameLabel}</Label>
        <Input id={`${prefix}-key-name`} value={draft.name} disabled={disabled} onChange={(event) => onChange({ ...draft, name: event.target.value })} />
      </div>
      <div className="space-y-2">
        <Label htmlFor={`${prefix}-model-policy`}>模型权限</Label>
        <select
          id={`${prefix}-model-policy`}
          className="h-8 w-full rounded-md border border-input bg-secondary/55 px-3 text-xs outline-none focus-visible:ring-1 focus-visible:ring-ring disabled:opacity-50"
          value={draft.modelPolicy}
          disabled={disabled}
          onChange={(event) => onChange({
            ...draft,
            modelPolicy: event.target.value as ClientKeyModelPolicy | "",
            modelScopes: event.target.value === "all" ? "" : draft.modelScopes,
          })}
        >
          <option value="">请选择模型权限</option>
          <option value="all">全部模型</option>
          <option value="allowlist">指定模型白名单</option>
        </select>
      </div>
      {draft.modelPolicy === "allowlist" ? (
        <div className="space-y-2">
          <Label htmlFor={`${prefix}-model-scopes`}>允许的模型</Label>
          <textarea
            id={`${prefix}-model-scopes`}
            className="min-h-20 w-full rounded-md border border-input bg-secondary/55 px-3 py-2 text-xs outline-none placeholder:text-muted-foreground focus-visible:ring-1 focus-visible:ring-ring disabled:opacity-50"
            value={draft.modelScopes}
            disabled={disabled}
            onChange={(event) => onChange({ ...draft, modelScopes: event.target.value })}
            placeholder="grok-4, grok-code"
          />
          <p className="text-[11px] text-muted-foreground">使用逗号或换行分隔模型 ID。</p>
        </div>
      ) : null}
      <div className="space-y-2">
        <Label htmlFor={`${prefix}-expires-at`}>到期时间（可选）</Label>
        <Input id={`${prefix}-expires-at`} type="datetime-local" value={draft.expiresAt} disabled={disabled} onChange={(event) => onChange({ ...draft, expiresAt: event.target.value })} />
      </div>
    </>
  );
}

function LimitDecision({
  id,
  label,
  unlimitedLabel,
  value,
  unlimited,
  onValue,
  onUnlimited,
}: {
  id: string;
  label: string;
  unlimitedLabel: string;
  value: string;
  unlimited: boolean;
  onValue: (value: string) => void;
  onUnlimited: (value: boolean) => void;
}) {
  return (
    <div className="space-y-2">
      <Label htmlFor={id}>{label}</Label>
      <div className="flex items-center gap-3">
        <Input id={id} type="number" min={1} value={value} disabled={unlimited} onChange={(event) => onValue(event.target.value)} />
        <label className="flex shrink-0 items-center gap-2 text-xs text-muted-foreground">
          <input type="checkbox" checked={unlimited} onChange={(event) => onUnlimited(event.target.checked)} />
          {unlimitedLabel}
        </label>
      </div>
    </div>
  );
}
