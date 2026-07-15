import {
  AdminApiError,
  type ClientKey,
  type ClientKeyInput,
  type ClientKeyModelPolicy,
} from "@/api/client";

export type KeyDraft = {
  name: string;
  modelPolicy: ClientKeyModelPolicy | "";
  modelScopes: string;
  rpmLimit: string;
  maxConcurrent: string;
  expiresAt: string;
};

export type LimitDecisions = {
  unlimitedRPM: boolean;
  unlimitedConcurrent: boolean;
  allModelsConfirmed: boolean;
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
  if (!draft.modelPolicy) return "请选择模型权限";
  if (!draft.name.trim()) return "请输入密钥名称";
  const scopes = parseModelScopes(draft.modelScopes);
  if (draft.modelPolicy === "all" && !decisions.allModelsConfirmed) {
    return "请确认允许访问全部模型";
  }
  if (draft.modelPolicy === "allowlist" && scopes.length === 0) {
    return "请至少填写一个允许的模型";
  }
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
    model_policy: draft.modelPolicy,
    model_scopes: draft.modelPolicy === "allowlist" ? scopes : [],
    rpm_limit: decisions.unlimitedRPM ? 0 : rpm,
    max_concurrent: decisions.unlimitedConcurrent ? 0 : concurrent,
    expires_at: expiresPayload(draft.expiresAt),
  };
}

export function draftFromClientKey(key: ClientKey): KeyDraft {
  return {
    name: key.name,
    modelPolicy: key.model_policy,
    modelScopes: key.model_scopes.join(", "),
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
