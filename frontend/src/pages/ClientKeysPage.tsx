import { useCallback, useEffect, useState } from "react";
import { CircleAlert, Plus, RefreshCw } from "lucide-react";
import {
  adminApi,
  type ClientKey,
  type ClientKeysPage as PageData,
} from "@/api/client";
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/cn";
import { ClientKeyDetailDialog } from "@/pages/client-keys/ClientKeyDetailDialog";
import { ClientKeySecretDialog } from "@/pages/client-keys/ClientKeySecretDialog";
import { ClientKeysTable } from "@/pages/client-keys/ClientKeysTable";
import { CreateClientKeyDialog } from "@/pages/client-keys/CreateClientKeyDialog";
import { clientKeyErrorMessage } from "@/pages/client-keys/clientKeyDraft";

export function ClientKeysPage() {
  const [q, setQ] = useState("");
  const [origin, setOrigin] = useState("");
  const [page, setPage] = useState(1);
  const [data, setData] = useState<PageData | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [createOpen, setCreateOpen] = useState(false);
  const [createdSecret, setCreatedSecret] = useState<string | null>(null);
  const [selected, setSelected] = useState<ClientKey | null>(null);

  const load = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      setData(await adminApi.clientKeys({ q, origin, page, page_size: 20 }));
    } catch (failure) {
      setError(clientKeyErrorMessage(failure, "加载客户端密钥失败"));
    } finally {
      setLoading(false);
    }
  }, [origin, page, q]);

  useEffect(() => {
    void load();
  }, [load]);

  function created(key: ClientKey & { secret: string }) {
    const { secret, ...safeKey } = key;
    setData((current) => current ? {
      ...current,
      items: [safeKey, ...current.items.filter((item) => item.id !== safeKey.id)],
      total: current.total + (current.items.some((item) => item.id === safeKey.id) ? 0 : 1),
    } : current);
    setCreateOpen(false);
    setCreatedSecret(secret);
  }

  function changed(key: ClientKey) {
    setSelected(key);
    setData((current) => current ? {
      ...current,
      items: current.items.map((item) => item.id === key.id ? key : item),
    } : current);
  }

  const items = data?.items || [];
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
          <Button size="sm" onClick={() => setCreateOpen(true)}>
            <Plus className="size-3.5" />
            创建客户端密钥
          </Button>
        </div>
      </header>

      {error ? (
        <div
          className="flex items-center gap-2 rounded-lg bg-destructive/8 px-3 py-2 text-xs text-destructive"
          role="alert"
        >
          <CircleAlert className="size-4 shrink-0" />
          {error}
        </div>
      ) : null}

      <ClientKeysTable
        items={items}
        total={data?.total || 0}
        page={page}
        pageSize={data?.page_size || 20}
        q={q}
        origin={origin}
        loading={loading}
        onSearch={(value) => { setQ(value); setPage(1); }}
        onOrigin={(value) => { setOrigin(value); setPage(1); }}
        onPage={setPage}
        onOpenDetail={setSelected}
      />

      {createOpen ? (
        <CreateClientKeyDialog onClose={() => setCreateOpen(false)} onCreated={created} />
      ) : null}
      {createdSecret ? (
        <ClientKeySecretDialog secret={createdSecret} onClose={() => setCreatedSecret(null)} />
      ) : null}
      {selected ? (
        <ClientKeyDetailDialog
          initial={selected}
          onClose={() => setSelected(null)}
          onChanged={changed}
        />
      ) : null}
    </div>
  );
}
