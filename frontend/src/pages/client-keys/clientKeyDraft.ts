import {
  AdminApiError,
  type ClientKey,
  type ClientKeyInput,
  type ClientKeyModelPolicy,
} from "@/api/client";

export type KeyDraft = {
  name: string;
  /** Kept for compatibility; final policy is derived from selected scopes + catalog. */
  modelPolicy: ClientKeyModelPolicy | "";
  modelScopes: string;
  rpmLimit: string;
  maxConcurrent: string;
  expiresAt: string;
};

export type LimitDecisions = {
  unlimitedRPM: boolean;
  unlimitedConcurrent: boolean;
  /** Optional enabled model ids from registry; used to detect "select all". */
  catalogModelIds?: string[];
};

export function emptyKeyDraft(): KeyDraft {
  return {
    name: "",
    modelPolicy: "",
    modelScopes: "",
    rpmLimit: "",
    maxConcurrent: "",
    expiresAt: "",
  };
}

export function parseModelScopes(value: string): string[] {
  return Array.from(new Set(
    value
      .split(/[\n,]/)
      .map((item) => item.trim().toLowerCase())
      .filter(Boolean),
  ));
}

export function buildClientKeyInput(
  draft: KeyDraft,
  decisions: LimitDecisions,
): ClientKeyInput | string {
  if (!draft.name.trim()) return "请输入密钥名称";
  const scopes = parseModelScopes(draft.modelScopes);
  if (scopes.length === 0) return "请至少选择一个模型";

  const catalog = (decisions.catalogModelIds || [])
    .map((id) => id.trim().toLowerCase())
    .filter(Boolean);
  const catalogSet = new Set(catalog);
  const selectsAllCatalog =
    catalog.length > 0
    && catalog.every((id) => scopes.includes(id))
    && scopes.every((id) => catalogSet.has(id));

  // Full catalog selection maps to backend "all"; otherwise explicit allowlist.
  // Legacy keys that already used model_policy=all are rehydrated by selecting
  // the full catalog in the detail dialog.
  const modelPolicy: ClientKeyModelPolicy = selectsAllCatalog ? "all" : "allowlist";

  const rpm = Number(draft.rpmLimit);
  if (!decisions.unlimitedRPM && (!draft.rpmLimit || !Number.isInteger(rpm) || rpm < 1)) {
    return "请输入大于 0 的 RPM，或主动选择 RPM 不限";
  }
  const concurrent = Number(draft.maxConcurrent);
  if (
    !decisions.unlimitedConcurrent
    && (!draft.maxConcurrent || !Number.isInteger(concurrent) || concurrent < 1)
  ) {
    return "请输入大于 0 的最大并发，或主动选择并发不限";
  }
  return {
    name: draft.name.trim(),
    model_policy: modelPolicy,
    model_scopes: modelPolicy === "allowlist" ? scopes : [],
    rpm_limit: decisions.unlimitedRPM ? 0 : rpm,
    max_concurrent: decisions.unlimitedConcurrent ? 0 : concurrent,
    expires_at: expiresPayload(draft.expiresAt),
  };
}

export function draftFromClientKey(key: ClientKey, catalogModelIds: string[] = []): KeyDraft {
  const catalog = catalogModelIds.map((id) => id.trim().toLowerCase()).filter(Boolean);
  // "all" keys have empty scopes in API; rehydrate UI as full catalog selection.
  const scopes =
    key.model_policy === "all" && catalog.length > 0
      ? catalog
      : key.model_scopes;
  return {
    name: key.name,
    modelPolicy: key.model_policy === "all" ? "all" : scopes.length ? "allowlist" : "",
    modelScopes: scopes.join(", "),
    rpmLimit: String(key.rpm_limit),
    maxConcurrent: String(key.max_concurrent),
    expiresAt: localDateTime(key.expires_at),
  };
}

export function formatClientKeyDate(value?: string | null): string {
  if (!value) return "—";
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? value : date.toLocaleString();
}

export function clientKeyErrorMessage(error: unknown, fallback: string): string {
  return error instanceof AdminApiError ? error.message || fallback : fallback;
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
