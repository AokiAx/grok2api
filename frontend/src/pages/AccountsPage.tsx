import { useCallback, useEffect, useState, type FormEvent } from "react";
import {
  ChevronLeft,
  ChevronRight,
  CircleAlert,
  RefreshCw,
  Search,
  Trash2,
  Undo2,
  X,
} from "lucide-react";
import {
  adminApi,
  AdminApiError,
  type AccountsPage as PageData,
  type AccountEvent,
  type PublicAccount,
} from "@/api/client";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { cn } from "@/lib/cn";

function formatDate(value?: string) {
  if (!value) return "—";
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? value : date.toLocaleString();
}

function quotaText(item: PublicAccount) {
  return item.quota_limit > 0 ? `${item.quota_actual}/${item.quota_limit}` : "—";
}

export function AccountsPage() {
  const [pool, setPool] = useState("all");
  const [qDraft, setQDraft] = useState("");
  const [q, setQ] = useState("");
  const [page, setPage] = useState(1);
  const [data, setData] = useState<PageData | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const [busyId, setBusyId] = useState<string | null>(null);
  const [selected, setSelected] = useState<PublicAccount | null>(null);
  const [selectedIds, setSelectedIds] = useState<Set<string>>(() => new Set());
  const [batchBusy, setBatchBusy] = useState(false);
  const [priorityDraft, setPriorityDraft] = useState(0);
  const [maxActiveDraft, setMaxActiveDraft] = useState(1);
  const [events, setEvents] = useState<AccountEvent[]>([]);
  const [settingsBusy, setSettingsBusy] = useState(false);

  const load = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const result = await adminApi.accounts({
        pool: pool === "all" ? "" : pool,
        q,
        page,
        page_size: 20,
      });
      setData(result);
      setSelected((prev) => (prev ? result.accounts.find((a) => a.id === prev.id) || null : null));
    } catch (err) {
      setError(err instanceof AdminApiError ? err.message : "加载失败");
    } finally {
      setLoading(false);
    }
  }, [pool, q, page]);

  useEffect(() => {
    void load();
  }, [load]);

  useEffect(() => {
    if (!selected) {
      setEvents([]);
      return;
    }
    setPriorityDraft(selected.priority || 0);
    setMaxActiveDraft(selected.max_active || 1);
    let active = true;
    void adminApi.accountEvents(selected.id).then((result) => {
      if (active) setEvents(result.items);
    }).catch(() => {
      if (active) setEvents([]);
    });
    return () => {
      active = false;
    };
  }, [selected]);

  async function recover(id: string) {
    setBusyId(id);
    try {
      const item = await adminApi.recoverAccount(id);
      await load();
      setSelected(item);
    } catch (err) {
      setError(err instanceof AdminApiError ? err.message : "恢复失败");
    } finally {
      setBusyId(null);
    }
  }

  async function remove(id: string) {
    if (!window.confirm(`删除 ${id}？`)) return;
    setBusyId(id);
    try {
      await adminApi.deleteAccount(id);
      if (selected?.id === id) setSelected(null);
      await load();
    } catch (err) {
      setError(err instanceof AdminApiError ? err.message : "删除失败");
    } finally {
      setBusyId(null);
    }
  }

  async function runBatch(action: "enable" | "disable" | "recover" | "delete") {
    const ids = Array.from(selectedIds);
    if (!ids.length) return;
    const labels = { enable: "启用", disable: "禁用", recover: "恢复", delete: "删除" } as const;
    if (!window.confirm(`确认批量${labels[action]} ${ids.length} 个账号？`)) return;
    setBatchBusy(true);
    setError(null);
    try {
      await adminApi.batchAccounts(ids, action);
      setSelectedIds(new Set());
      if (action === "delete" && selected && ids.includes(selected.id)) setSelected(null);
      await load();
    } catch (err) {
      setError(err instanceof AdminApiError ? err.message : `批量${labels[action]}失败`);
    } finally {
      setBatchBusy(false);
    }
  }

  async function saveSettings() {
    if (!selected) return;
    setSettingsBusy(true);
    setError(null);
    try {
      const updated = await adminApi.updateAccount(selected.id, {
        priority: priorityDraft,
        max_active: maxActiveDraft,
      });
      setSelected(updated);
      await load();
    } catch (err) {
      setError(err instanceof AdminApiError ? err.message : "保存账号设置失败");
    } finally {
      setSettingsBusy(false);
    }
  }

  const totalPages = data?.total_pages || 1;
  const accounts = data?.accounts || [];

  return (
    <div className="space-y-5">
      <header className="flex flex-col gap-3 sm:flex-row sm:items-start sm:justify-between">
        <div>
          <h1 className="text-xl font-medium tracking-tight">账号</h1>
          <p className="mt-1 text-xs text-muted-foreground">
            共 <span className="tabular-nums">{data?.total ?? "—"}</span> 条
            {q ? ` · 正在搜索“${q}”` : " · 管理账号状态、额度与并发"}
          </p>
        </div>
        <Button variant="outline" size="sm" onClick={() => void load()} disabled={loading}>
          <RefreshCw className={cn("h-3.5 w-3.5", loading && "animate-spin")} />
          {loading ? "刷新中" : "刷新"}
        </Button>
      </header>

      <Card>
        <CardContent className="p-3">
          <form
            className="flex flex-col gap-2 sm:flex-row sm:items-center"
            onSubmit={(e: FormEvent) => {
              e.preventDefault();
              setPage(1);
              setQ(qDraft.trim());
            }}
          >
            <div className="flex shrink-0 rounded-full bg-background p-0.5" aria-label="账号池筛选">
              {(["all", "ready", "unavailable"] as const).map((value) => (
                <button
                  key={value}
                  type="button"
                  className={cn(
                    "h-7 rounded-full px-3 text-[11px] font-medium text-muted-foreground transition-colors",
                    pool === value && "bg-primary text-primary-foreground",
                  )}
                  onClick={() => {
                    setPool(value);
                    setPage(1);
                  }}
                >
                  {value === "all" ? "全部" : value}
                </button>
              ))}
            </div>

            <div className="relative min-w-0 flex-1">
              <Label htmlFor="q" className="sr-only">搜索账号</Label>
              <Search className="pointer-events-none absolute left-3 top-1/2 h-3.5 w-3.5 -translate-y-1/2 text-muted-foreground" />
              <Input
                id="q"
                className="pl-8 pr-8"
                value={qDraft}
                onChange={(e) => setQDraft(e.target.value)}
                placeholder="搜索 ID、邮箱或不可用原因"
              />
              {qDraft ? (
                <button
                  type="button"
                  aria-label="清空搜索"
                  className="absolute right-1 top-1/2 flex h-6 w-6 -translate-y-1/2 items-center justify-center rounded-full text-muted-foreground hover:bg-accent hover:text-foreground"
                  onClick={() => {
                    setQDraft("");
                    if (q) {
                      setQ("");
                      setPage(1);
                    }
                  }}
                >
                  <X className="h-3.5 w-3.5" />
                </button>
              ) : null}
            </div>
            <Button type="submit" size="sm" className="sm:w-auto">
              查询
            </Button>
          </form>
        </CardContent>
      </Card>

      {error ? (
        <div className="flex items-center gap-2 rounded-lg bg-destructive/8 px-3 py-2 text-xs text-destructive" role="alert">
          <CircleAlert className="h-4 w-4 shrink-0" />
          {error}
        </div>
      ) : null}

      {selectedIds.size > 0 ? (
        <div className="flex flex-col gap-2 rounded-[10px] bg-card px-3 py-2.5 sm:flex-row sm:items-center sm:justify-between">
          <span className="text-xs text-muted-foreground">已选择 <strong className="font-medium text-foreground">{selectedIds.size}</strong> 个账号</span>
          <div className="flex flex-wrap gap-1.5">
            <Button size="sm" variant="outline" disabled={batchBusy} onClick={() => void runBatch("enable")}>批量启用</Button>
            <Button size="sm" variant="outline" disabled={batchBusy} onClick={() => void runBatch("disable")}>批量禁用</Button>
            <Button size="sm" variant="outline" disabled={batchBusy} onClick={() => void runBatch("recover")}>批量恢复</Button>
            <Button size="sm" variant="destructive" disabled={batchBusy} onClick={() => void runBatch("delete")}>批量删除</Button>
          </div>
        </div>
      ) : null}

      <div className="grid min-w-0 gap-3 xl:grid-cols-[minmax(0,1fr)_300px]">
        <Card className="min-w-0 overflow-hidden">
          <div className="overflow-x-auto">
            <table className="w-full min-w-[960px] text-left text-xs">
              <thead className="border-b border-border/80 bg-background text-[11px] text-muted-foreground">
                <tr>
                  <th className="w-10 px-3 py-2.5">
                    <input
                      type="checkbox"
                      aria-label="选择全部账号"
                      checked={accounts.length > 0 && accounts.every((item) => selectedIds.has(item.id))}
                      onChange={(event) => {
                        setSelectedIds(event.target.checked ? new Set(accounts.map((item) => item.id)) : new Set());
                      }}
                    />
                  </th>
                  <th className="w-[250px] px-3 py-2.5 font-medium">账号</th>
                  <th className="px-3 py-2.5 font-medium">状态</th>
                  <th className="px-3 py-2.5 font-medium">不可用原因</th>
                  <th className="px-3 py-2.5 text-right font-medium">额度</th>
                  <th className="px-3 py-2.5 text-right font-medium">并发</th>
                  <th className="px-3 py-2.5 text-right font-medium">请求数</th>
                  <th className="w-[148px] px-3 py-2.5 text-right font-medium">操作</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-border/70">
                {accounts.map((item) => (
                  <tr
                    key={item.id}
                    tabIndex={0}
                    aria-selected={selected?.id === item.id}
                    className={cn(
                      "cursor-pointer outline-none transition-colors hover:bg-background focus-visible:bg-accent/60",
                      selected?.id === item.id && "bg-accent/60",
                    )}
                    onClick={() => setSelected(item)}
                    onKeyDown={(event) => {
                      if (event.key === "Enter" || event.key === " ") {
                        event.preventDefault();
                        setSelected(item);
                      }
                    }}
                  >
                    <td className="px-3 py-2.5 align-middle" onClick={(event) => event.stopPropagation()}>
                      <input
                        type="checkbox"
                        aria-label={`选择账号 ${item.id}`}
                        checked={selectedIds.has(item.id)}
                        onChange={(event) => {
                          setSelectedIds((current) => {
                            const next = new Set(current);
                            if (event.target.checked) next.add(item.id);
                            else next.delete(item.id);
                            return next;
                          });
                        }}
                      />
                    </td>
                    <td className="px-3 py-2.5 align-middle">
                      <div className="mono max-w-[230px] truncate text-xs text-foreground" title={item.id}>{item.id}</div>
                      <div className="mt-0.5 max-w-[230px] truncate text-[11px] text-muted-foreground" title={item.email || undefined}>
                        {item.email || "未提供邮箱"}
                      </div>
                    </td>
                    <td className="px-3 py-2.5 align-middle">
                      <Badge tone={item.pool === "ready" ? "success" : "warning"}>{item.pool}</Badge>
                    </td>
                    <td className="px-3 py-2.5 align-middle">
                      {item.unavailable_reason ? (
                        <Badge tone={item.unavailable_reason === "auth" ? "danger" : "warning"}>
                          {item.unavailable_reason}
                        </Badge>
                      ) : (
                        <span className="text-muted-foreground">—</span>
                      )}
                    </td>
                    <td className="mono px-3 py-2.5 text-right align-middle tabular-nums">{quotaText(item)}</td>
                    <td className="mono px-3 py-2.5 text-right align-middle tabular-nums">
                      {item.active}/{item.max_active || 1}
                    </td>
                    <td className="px-3 py-2.5 text-right align-middle tabular-nums">{item.request_count}</td>
                    <td className="px-3 py-2 align-middle" onClick={(e) => e.stopPropagation()}>
                      <div className="flex justify-end gap-1">
                        <Button
                          size="sm"
                          variant="ghost"
                          className="px-2.5"
                          disabled={busyId === item.id}
                          onClick={() => void recover(item.id)}
                        >
                          <Undo2 className="h-3.5 w-3.5" />
                          恢复
                        </Button>
                        <Button
                          size="sm"
                          variant="ghost"
                          className="px-2.5 text-destructive hover:bg-destructive/10 hover:text-destructive"
                          disabled={busyId === item.id}
                          onClick={() => void remove(item.id)}
                        >
                          <Trash2 className="h-3.5 w-3.5" />
                          删除
                        </Button>
                      </div>
                    </td>
                  </tr>
                ))}
                {!loading && accounts.length === 0 ? (
                  <tr>
                    <td colSpan={8} className="px-3 py-12 text-center text-xs text-muted-foreground">
                      {q || pool !== "all" ? "没有符合当前筛选条件的账号" : "暂无账号"}
                    </td>
                  </tr>
                ) : null}
                {loading && accounts.length === 0 ? (
                  <tr>
                    <td colSpan={8} className="px-3 py-12 text-center text-xs text-muted-foreground">正在加载账号…</td>
                  </tr>
                ) : null}
              </tbody>
            </table>
          </div>
          <div className="flex flex-col gap-2 border-t border-border/70 bg-background px-3 py-2.5 sm:flex-row sm:items-center sm:justify-between">
            <p className="text-[11px] text-muted-foreground">
              本页 <span className="tabular-nums text-foreground">{accounts.length}</span> 条 · 第
              <span className="tabular-nums text-foreground"> {data?.page ?? page} / {totalPages} </span>页
            </p>
            <div className="flex items-center gap-1">
              <Button
                size="sm"
                variant="outline"
                className="px-2.5"
                disabled={page <= 1 || loading}
                onClick={() => setPage((p) => Math.max(1, p - 1))}
              >
                <ChevronLeft className="h-3.5 w-3.5" />
                上一页
              </Button>
              <Button
                size="sm"
                variant="outline"
                className="px-2.5"
                disabled={loading || page >= totalPages}
                onClick={() => setPage((p) => p + 1)}
              >
                下一页
                <ChevronRight className="h-3.5 w-3.5" />
              </Button>
            </div>
          </div>
        </Card>

        <Card className="h-fit xl:sticky xl:top-5">
          <CardContent className="p-4">
            <div className="mb-4 flex items-center justify-between gap-3">
              <div>
                <h2 className="text-sm font-medium">账号详情</h2>
                <p className="mt-0.5 text-[11px] text-muted-foreground">选择表格行查看完整信息</p>
              </div>
              {selected ? (
                <Button size="icon" variant="ghost" aria-label="关闭详情" onClick={() => setSelected(null)}>
                  <X className="h-3.5 w-3.5" />
                </Button>
              ) : null}
            </div>
            {selected ? (
              <>
                <div className="mb-4 flex items-center gap-2">
                  <Badge tone={selected.pool === "ready" ? "success" : "warning"}>{selected.pool}</Badge>
                  {selected.unavailable_reason ? (
                    <Badge tone={selected.unavailable_reason === "auth" ? "danger" : "warning"}>
                      {selected.unavailable_reason}
                    </Badge>
                  ) : null}
                </div>
                <dl className="divide-y divide-border/70 text-xs">
                  {(
                    [
                      ["ID", selected.id],
                      ["邮箱", selected.email || "—"],
                      ["用户 ID", selected.user_id || "—"],
                      ["团队 ID", selected.team_id || "—"],
                      ["错误码", selected.last_error_code || "—"],
                      ["恢复时间", formatDate(selected.retry_at)],
                      ["额度", quotaText(selected)],
                      ["并发", `${selected.active}/${selected.max_active || 1}`],
                      ["请求数", String(selected.request_count)],
                      ["Refresh Token", selected.has_refresh_token ? "有" : "无"],
                    ] as const
                  ).map(([key, value]) => (
                    <div key={key} className="grid grid-cols-[78px_minmax(0,1fr)] gap-3 py-2.5 first:pt-0">
                      <dt className="text-muted-foreground">{key}</dt>
                      <dd className="mono break-all text-right tabular-nums">{value}</dd>
                    </div>
                  ))}
                </dl>
                <div className="mt-4 grid grid-cols-2 gap-2 border-t border-border/70 pt-4">
                  <div className="space-y-1.5">
                    <Label htmlFor="account-priority">优先级</Label>
                    <Input
                      id="account-priority"
                      type="number"
                      min={0}
                      value={priorityDraft}
                      onChange={(event) => setPriorityDraft(Number(event.target.value))}
                    />
                  </div>
                  <div className="space-y-1.5">
                    <Label htmlFor="account-max-active">最大并发</Label>
                    <Input
                      id="account-max-active"
                      type="number"
                      min={1}
                      value={maxActiveDraft}
                      onChange={(event) => setMaxActiveDraft(Number(event.target.value))}
                    />
                  </div>
                  <Button className="col-span-2" size="sm" disabled={settingsBusy} onClick={() => void saveSettings()}>
                    {settingsBusy ? "保存中…" : "保存账号设置"}
                  </Button>
                </div>
                <div className="mt-4 border-t border-border/70 pt-4">
                  <h3 className="text-xs font-medium">状态时间线</h3>
                  {events.length ? (
                    <ul className="mt-2 space-y-2">
                      {events.slice(0, 5).map((event) => (
                        <li key={event.id} className="rounded-md bg-background px-2.5 py-2 text-[11px]">
                          <div className="flex items-center justify-between gap-2">
                            <span className="font-medium">{event.event_type}</span>
                            <time className="text-muted-foreground">{formatDate(event.created_at)}</time>
                          </div>
                          <p className="mt-1 text-muted-foreground">{event.reason || `${event.from_pool || "new"} → ${event.to_pool}`}</p>
                        </li>
                      ))}
                    </ul>
                  ) : <p className="mt-2 text-[11px] text-muted-foreground">暂无状态事件</p>}
                </div>
                <div className="mt-4 grid grid-cols-2 gap-2">
                  <Button
                    size="sm"
                    variant="outline"
                    disabled={busyId === selected.id}
                    onClick={() => void recover(selected.id)}
                  >
                    <Undo2 className="h-3.5 w-3.5" />
                    恢复
                  </Button>
                  <Button
                    size="sm"
                    variant="destructive"
                    disabled={busyId === selected.id}
                    onClick={() => void remove(selected.id)}
                  >
                    <Trash2 className="h-3.5 w-3.5" />
                    删除
                  </Button>
                </div>
              </>
            ) : (
              <div className="rounded-lg bg-background px-4 py-10 text-center text-xs text-muted-foreground">
                点击任意账号行查看详情
              </div>
            )}
          </CardContent>
        </Card>
      </div>
    </div>
  );
}
