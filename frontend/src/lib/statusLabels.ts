/** UI labels for account pool / unavailable reason codes (API stays English). */

const POOL_LABELS: Record<string, string> = {
  ready: "可用",
  unavailable: "不可用",
  all: "全部",
};

const REASON_LABELS: Record<string, string> = {
  auth: "认证失败",
  quota: "额度耗尽",
  cooldown: "冷却中",
  validating: "校验中",
  disabled: "已禁用",
};

export function poolLabel(pool?: string | null): string {
  const key = String(pool || "").trim();
  if (!key) return "—";
  return POOL_LABELS[key] || key;
}

export function unavailableReasonLabel(reason?: string | null): string {
  const key = String(reason || "").trim();
  if (!key) return "—";
  return REASON_LABELS[key] || key;
}

/** Prefer Chinese label for known pool/reason codes; leave opaque error codes as-is. */
export function statusCodeLabel(code?: string | null): string {
  const key = String(code || "").trim();
  if (!key) return "—";
  return POOL_LABELS[key] || REASON_LABELS[key] || key;
}
