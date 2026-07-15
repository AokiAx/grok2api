import { useCallback, useEffect, useState } from "react";
import { adminApi, AdminApiError, type ManagedModel } from "@/api/client";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";

export function ModelsPage() {
  const [items, setItems] = useState<ManagedModel[]>([]);
  const [selected, setSelected] = useState<ManagedModel | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [message, setMessage] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [draftId, setDraftId] = useState("");

  const load = useCallback(async () => {
    setError(null);
    try {
      const data = await adminApi.models(true);
      setItems(data.models || []);
      if (selected) {
        const next = (data.models || []).find((m) => m.id === selected.id) || null;
        setSelected(next);
      }
    } catch (err) {
      setError(err instanceof AdminApiError ? err.message : "加载失败");
    }
  }, [selected?.id]);

  useEffect(() => {
    void load();
  }, []);

  function startCreate() {
    const id = draftId.trim();
    if (!id) {
      setError("请先填写模型 ID");
      return;
    }
    setSelected({
      id,
      upstream_id: id,
      name: id,
      api_backend: "responses",
      context_window: 128000,
      supports_reasoning_effort: false,
      reasoning_efforts: [],
      supports_backend_search: false,
      owned_by: "xai",
      enabled: true,
      aliases: [],
      source: "managed",
    });
    setMessage(null);
    setError(null);
  }

  async function save() {
    if (!selected) return;
    setBusy(true);
    setError(null);
    setMessage(null);
    try {
      const body = {
        upstream_id: selected.upstream_id,
        name: selected.name,
        api_backend: selected.api_backend,
        context_window: selected.context_window,
        supports_reasoning_effort: selected.supports_reasoning_effort,
        reasoning_efforts: selected.reasoning_efforts,
        supports_backend_search: selected.supports_backend_search,
        owned_by: selected.owned_by,
        enabled: selected.enabled,
        aliases: selected.aliases,
      };
      const saved = await adminApi.putModel(selected.id, body);
      setSelected(saved);
      setMessage("已保存");
      await load();
    } catch (err) {
      setError(err instanceof AdminApiError ? err.message : "保存失败");
    } finally {
      setBusy(false);
    }
  }

  async function toggleEnabled(item: ManagedModel) {
    setBusy(true);
    setError(null);
    try {
      await adminApi.patchModel(item.id, { enabled: !item.enabled });
      await load();
    } catch (err) {
      setError(err instanceof AdminApiError ? err.message : "更新失败");
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="space-y-8">
      <div className="flex flex-col gap-3 sm:flex-row sm:items-end sm:justify-between">
        <div>
          <h1 className="text-xl font-medium">模型注册表</h1>
          <p className="mt-1.5 text-xs text-muted-foreground">
            公共模型 ID、别名、上游映射与启停。协议桥继续走 Catalog facade。
          </p>
        </div>
        <Button variant="outline" size="sm" onClick={() => void load()} disabled={busy}>
          刷新
        </Button>
      </div>

      {error ? <p className="text-sm text-destructive">{error}</p> : null}
      {message ? <p className="text-sm text-emerald-600 dark:text-emerald-400">{message}</p> : null}

      <div className="grid gap-4 lg:grid-cols-[1fr_1.1fr]">
        <Card className="p-4 sm:p-5">
          <CardHeader>
            <CardTitle>模型列表</CardTitle>
            <CardDescription>共 {items.length} 个</CardDescription>
          </CardHeader>
          <CardContent className="mt-3 space-y-3">
            <div className="flex gap-2">
              <Input
                placeholder="新模型 ID，如 grok-4.5-custom"
                value={draftId}
                onChange={(e) => setDraftId(e.target.value)}
              />
              <Button variant="outline" onClick={startCreate}>
                新建
              </Button>
            </div>
            <ul className="divide-y divide-border/70">
              {items.map((item) => (
                <li key={item.id} className="flex items-center justify-between gap-3 py-3 first:pt-0">
                  <button
                    type="button"
                    className="min-w-0 flex-1 text-left"
                    onClick={() => setSelected(item)}
                  >
                    <div className="truncate text-sm font-medium">{item.name || item.id}</div>
                    <div className="truncate font-mono text-[11px] text-muted-foreground">
                      {item.id}
                      {item.aliases?.length ? ` · alias ${item.aliases.join(", ")}` : ""}
                    </div>
                  </button>
                  <div className="flex items-center gap-2">
                    <Badge tone={item.enabled ? "success" : "danger"}>{item.enabled ? "on" : "off"}</Badge>
                    <Button size="sm" variant="outline" disabled={busy} onClick={() => void toggleEnabled(item)}>
                      {item.enabled ? "停用" : "启用"}
                    </Button>
                  </div>
                </li>
              ))}
            </ul>
          </CardContent>
        </Card>

        <Card className="p-4 sm:p-5">
          <CardHeader>
            <CardTitle>{selected ? `编辑 ${selected.id}` : "选择模型"}</CardTitle>
            <CardDescription>PUT 全量保存当前草稿</CardDescription>
          </CardHeader>
          <CardContent className="mt-3">
            {!selected ? (
              <p className="text-sm text-muted-foreground">从左侧选择，或新建模型。</p>
            ) : (
              <div className="grid gap-3 sm:grid-cols-2">
                <Field label="显示名">
                  <Input
                    value={selected.name}
                    onChange={(e) => setSelected({ ...selected, name: e.target.value })}
                  />
                </Field>
                <Field label="上游 ID">
                  <Input
                    value={selected.upstream_id}
                    onChange={(e) => setSelected({ ...selected, upstream_id: e.target.value })}
                  />
                </Field>
                <Field label="API Backend">
                  <select
                    className="flex h-9 w-full rounded-md border border-input bg-transparent px-3 text-sm"
                    value={selected.api_backend}
                    onChange={(e) => setSelected({ ...selected, api_backend: e.target.value })}
                  >
                    <option value="responses">responses</option>
                    <option value="chat_completions">chat_completions</option>
                  </select>
                </Field>
                <Field label="Context Window">
                  <Input
                    type="number"
                    value={selected.context_window}
                    onChange={(e) =>
                      setSelected({ ...selected, context_window: Number(e.target.value) || 0 })
                    }
                  />
                </Field>
                <Field label="Owned By">
                  <Input
                    value={selected.owned_by}
                    onChange={(e) => setSelected({ ...selected, owned_by: e.target.value })}
                  />
                </Field>
                <Field label="别名 (逗号分隔)">
                  <Input
                    value={(selected.aliases || []).join(", ")}
                    onChange={(e) =>
                      setSelected({
                        ...selected,
                        aliases: e.target.value
                          .split(",")
                          .map((s) => s.trim())
                          .filter(Boolean),
                      })
                    }
                  />
                </Field>
                <Field label="Reasoning efforts">
                  <Input
                    value={(selected.reasoning_efforts || []).join(", ")}
                    onChange={(e) =>
                      setSelected({
                        ...selected,
                        reasoning_efforts: e.target.value
                          .split(",")
                          .map((s) => s.trim())
                          .filter(Boolean),
                      })
                    }
                  />
                </Field>
                <div className="flex flex-col gap-2 pt-6 text-xs">
                  <label className="flex items-center gap-2">
                    <input
                      type="checkbox"
                      checked={selected.enabled}
                      onChange={(e) => setSelected({ ...selected, enabled: e.target.checked })}
                    />
                    启用
                  </label>
                  <label className="flex items-center gap-2">
                    <input
                      type="checkbox"
                      checked={selected.supports_reasoning_effort}
                      onChange={(e) =>
                        setSelected({ ...selected, supports_reasoning_effort: e.target.checked })
                      }
                    />
                    支持 reasoning effort
                  </label>
                  <label className="flex items-center gap-2">
                    <input
                      type="checkbox"
                      checked={selected.supports_backend_search}
                      onChange={(e) =>
                        setSelected({ ...selected, supports_backend_search: e.target.checked })
                      }
                    />
                    支持 backend search
                  </label>
                </div>
                <div className="sm:col-span-2">
                  <Button onClick={() => void save()} disabled={busy}>
                    {busy ? "保存中…" : "保存模型"}
                  </Button>
                </div>
              </div>
            )}
          </CardContent>
        </Card>
      </div>
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
