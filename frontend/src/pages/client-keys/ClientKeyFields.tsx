import { useEffect, useMemo, useState } from "react";
import { adminApi, type ManagedModel } from "@/api/client";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { cn } from "@/lib/cn";
import { parseModelScopes, type KeyDraft } from "@/pages/client-keys/clientKeyDraft";

export function ClientKeyFields({
  draft,
  onChange,
  nameLabel,
  disabled = false,
  onCatalogChange,
}: {
  draft: KeyDraft;
  onChange: (next: KeyDraft) => void;
  nameLabel: string;
  disabled?: boolean;
  onCatalogChange?: (modelIds: string[]) => void;
}) {
  const prefix = nameLabel === "名称" ? "create" : "detail";
  const [models, setModels] = useState<ManagedModel[]>([]);
  const [modelsError, setModelsError] = useState<string | null>(null);
  const [modelsLoading, setModelsLoading] = useState(false);

  useEffect(() => {
    let active = true;
    setModelsLoading(true);
    setModelsError(null);
    void adminApi
      .models(true)
      .then((result) => {
        if (!active) return;
        setModels(result.models || []);
      })
      .catch(() => {
        if (active) setModelsError("模型列表加载失败");
      })
      .finally(() => {
        if (active) setModelsLoading(false);
      });
    return () => {
      active = false;
    };
  }, []);

  const selected = useMemo(() => new Set(parseModelScopes(draft.modelScopes)), [draft.modelScopes]);

  const catalog = useMemo(() => {
    const seen = new Set<string>();
    const items: Array<{ id: string; label: string; enabled: boolean; orphan?: boolean }> = [];
    for (const model of models) {
      const id = String(model.id || "").trim().toLowerCase();
      if (!id || seen.has(id)) continue;
      seen.add(id);
      items.push({
        id,
        label: model.name && model.name !== model.id ? model.name : model.id,
        enabled: model.enabled !== false,
      });
      for (const alias of model.aliases || []) {
        const aliasId = String(alias || "").trim().toLowerCase();
        if (!aliasId || seen.has(aliasId)) continue;
        seen.add(aliasId);
        items.push({
          id: aliasId,
          label: `别名 → ${model.id}`,
          enabled: model.enabled !== false,
        });
      }
    }
    for (const id of selected) {
      if (seen.has(id)) continue;
      items.push({ id, label: "未在注册表", enabled: true, orphan: true });
    }
    items.sort((a, b) => {
      if (a.enabled !== b.enabled) return a.enabled ? -1 : 1;
      return a.id.localeCompare(b.id);
    });
    return items;
  }, [models, selected]);

  const registryIds = useMemo(
    () =>
      catalog
        .filter((item) => !item.orphan)
        .map((item) => item.id),
    [catalog],
  );

  useEffect(() => {
    onCatalogChange?.(registryIds);
  }, [registryIds, onCatalogChange]);

  const selectableIds = useMemo(
    () => catalog.filter((item) => item.enabled || selected.has(item.id)).map((item) => item.id),
    [catalog, selected],
  );
  const allSelectableSelected =
    selectableIds.length > 0 && selectableIds.every((id) => selected.has(id));

  function applyScopes(ids: string[]) {
    const unique = Array.from(new Set(ids.map((id) => id.trim().toLowerCase()).filter(Boolean)));
    onChange({
      ...draft,
      modelPolicy: unique.length ? "allowlist" : "",
      modelScopes: unique.join(", "),
    });
  }

  function toggleModel(id: string) {
    const next = new Set(selected);
    if (next.has(id)) next.delete(id);
    else next.add(id);
    applyScopes(Array.from(next));
  }

  function toggleAll() {
    if (allSelectableSelected) applyScopes([]);
    else applyScopes(selectableIds);
  }

  return (
    <>
      <div className="space-y-2">
        <Label htmlFor={`${prefix}-key-name`}>{nameLabel}</Label>
        <Input
          id={`${prefix}-key-name`}
          value={draft.name}
          disabled={disabled}
          onChange={(event) => onChange({ ...draft, name: event.target.value })}
        />
      </div>

      <div className="space-y-2">
        <div className="flex items-center justify-between gap-3">
          <Label id={`${prefix}-model-scopes-label`}>可用模型</Label>
          <div className="flex items-center gap-2 text-[11px] text-muted-foreground">
            <span>
              已选 {selected.size}
              {modelsLoading ? " · 加载中…" : ""}
            </span>
            <button
              type="button"
              className="rounded-full border border-input bg-background px-2 py-0.5 text-[11px] hover:bg-secondary disabled:opacity-50"
              disabled={disabled || selectableIds.length === 0}
              onClick={toggleAll}
            >
              {allSelectableSelected ? "清空" : "全选"}
            </button>
          </div>
        </div>
        {modelsError ? <p className="text-[11px] text-destructive">{modelsError}</p> : null}
        <div
          role="group"
          aria-labelledby={`${prefix}-model-scopes-label`}
          className="max-h-56 space-y-1 overflow-y-auto rounded-md border border-input bg-secondary/40 p-2"
        >
          {catalog.length === 0 && !modelsLoading ? (
            <p className="px-1 py-3 text-center text-[11px] text-muted-foreground">暂无可用模型</p>
          ) : (
            catalog.map((item) => {
              const checked = selected.has(item.id);
              return (
                <label
                  key={item.id}
                  className={cn(
                    "flex cursor-pointer items-start gap-2 rounded-md px-2 py-1.5 text-xs transition-colors hover:bg-background/70",
                    checked && "bg-background/80",
                    !item.enabled && "opacity-60",
                    disabled && "cursor-not-allowed",
                  )}
                >
                  <input
                    type="checkbox"
                    className="mt-0.5"
                    checked={checked}
                    disabled={disabled}
                    onChange={() => toggleModel(item.id)}
                  />
                  <span className="min-w-0">
                    <span className="block truncate font-mono text-[11px]">{item.id}</span>
                    <span className="block truncate text-[10px] text-muted-foreground">
                      {item.label}
                      {!item.enabled ? " · 已禁用" : ""}
                      {item.orphan ? " · 未注册" : ""}
                    </span>
                  </span>
                </label>
              );
            })
          )}
        </div>
        <p className="text-[11px] text-muted-foreground">勾选此密钥可调用的模型；全选等同允许全部模型。</p>
      </div>

      <div className="space-y-2">
        <Label htmlFor={`${prefix}-expires-at`}>到期时间（可选）</Label>
        <Input
          id={`${prefix}-expires-at`}
          type="datetime-local"
          value={draft.expiresAt}
          disabled={disabled}
          onChange={(event) => onChange({ ...draft, expiresAt: event.target.value })}
        />
      </div>
    </>
  );
}

export function LimitDecision({
  id,
  label,
  unlimitedLabel,
  value,
  unlimited,
  disabled = false,
  min = 1,
  onValue,
  onUnlimited,
}: {
  id: string;
  label: string;
  unlimitedLabel: string;
  value: string;
  unlimited: boolean;
  disabled?: boolean;
  min?: number;
  onValue: (value: string) => void;
  onUnlimited: (value: boolean) => void;
}) {
  return (
    <div className="space-y-2">
      <Label htmlFor={id}>{label}</Label>
      <div className="flex items-center gap-3">
        <Input
          id={id}
          type="number"
          min={min}
          value={value}
          disabled={disabled || unlimited}
          onChange={(event) => onValue(event.target.value)}
        />
        <label className="flex shrink-0 items-center gap-2 text-xs text-muted-foreground">
          <input
            type="checkbox"
            checked={unlimited}
            disabled={disabled}
            onChange={(event) => onUnlimited(event.target.checked)}
          />
          {unlimitedLabel}
        </label>
      </div>
    </div>
  );
}
