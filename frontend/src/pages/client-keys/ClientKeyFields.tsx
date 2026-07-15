import type { ClientKeyModelPolicy } from "@/api/client";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import type { KeyDraft } from "@/pages/client-keys/clientKeyDraft";

export function ClientKeyFields({
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
        <Input
          id={`${prefix}-key-name`}
          value={draft.name}
          disabled={disabled}
          onChange={(event) => onChange({ ...draft, name: event.target.value })}
        />
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
        <Input
          id={id}
          type="number"
          min={1}
          value={value}
          disabled={unlimited}
          onChange={(event) => onValue(event.target.value)}
        />
        <label className="flex shrink-0 items-center gap-2 text-xs text-muted-foreground">
          <input
            type="checkbox"
            checked={unlimited}
            onChange={(event) => onUnlimited(event.target.checked)}
          />
          {unlimitedLabel}
        </label>
      </div>
    </div>
  );
}
