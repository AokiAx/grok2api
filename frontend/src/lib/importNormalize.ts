import type { ImportAccount } from "@/api/client";

function isAccountLike(value: unknown): value is Record<string, unknown> {
  if (!value || typeof value !== "object" || Array.isArray(value)) return false;
  const obj = value as Record<string, unknown>;
  return (
    "key" in obj ||
    "access_token" in obj ||
    "refresh_token" in obj ||
    "user_id" in obj
  );
}

function stripEmpty<T extends Record<string, unknown>>(obj: T): T {
  const out = { ...obj };
  for (const key of Object.keys(out)) {
    const value = out[key];
    if (value === undefined || value === "") delete out[key];
  }
  return out;
}

function normalizeOne(item: Record<string, unknown>, index: number): ImportAccount {
  if (!item || typeof item !== "object" || Array.isArray(item)) {
    throw new Error(`accounts[${index}] is not an object`);
  }
  const key = String(item.key || item.access_token || "").trim();
  const expiresInRaw = Number(item.expires_in);
  const out: ImportAccount = {
    key: key || undefined,
    access_token: key || undefined,
    refresh_token: item.refresh_token ? String(item.refresh_token).trim() : undefined,
    email: item.email ? String(item.email).trim().toLowerCase() : undefined,
    user_id: item.user_id ? String(item.user_id).trim() : undefined,
    team_id: item.team_id ? String(item.team_id).trim() : undefined,
    oidc_issuer: item.oidc_issuer ? String(item.oidc_issuer).trim() : undefined,
    oidc_client_id: item.oidc_client_id ? String(item.oidc_client_id).trim() : undefined,
    expires_in: Number.isFinite(expiresInRaw) && expiresInRaw > 0 ? expiresInRaw : undefined,
    expires_at: item.expires_at ? String(item.expires_at).trim() : undefined,
    id: item.id ? String(item.id).trim() : undefined,
  };
  return stripEmpty(out as Record<string, unknown>) as ImportAccount;
}

export function normalizeImportAccounts(raw: string): ImportAccount[] {
  const cleaned = raw.replace(/^﻿/, "").trim();
  if (!cleaned) throw new Error("empty content");
  let parsed: unknown;
  try {
    parsed = JSON.parse(cleaned);
  } catch (error) {
    throw new Error(
      `JSON parse failed: ${error instanceof Error ? error.message : String(error)}`,
    );
  }

  let accounts: unknown[];
  if (Array.isArray(parsed)) {
    accounts = parsed;
  } else if (
    parsed &&
    typeof parsed === "object" &&
    Array.isArray((parsed as { accounts?: unknown }).accounts)
  ) {
    accounts = (parsed as { accounts: unknown[] }).accounts;
  } else if (parsed && typeof parsed === "object") {
    const entries = Object.entries(parsed as Record<string, unknown>);
    if (entries.length && entries.every(([, v]) => isAccountLike(v))) {
      accounts = entries.map(([mapKey, item]) => {
        const out = { ...(item as Record<string, unknown>) };
        const parts = String(mapKey || "").split("::");
        const mapUserID =
          parts.length === 3 && parts[0].includes("://") && parts[2]
            ? parts[2].trim()
            : "";
        if (!out.email && mapKey.includes("@") && !mapUserID) out.email = mapKey;
        if (!out.user_id && mapUserID) out.user_id = mapUserID;
        if (!out.id && mapUserID) out.id = mapUserID;
        else if (!out.id && mapKey.includes("@") && !mapUserID) out.id = mapKey;
        return out;
      });
    } else if (isAccountLike(parsed)) {
      accounts = [parsed];
    } else {
      throw new Error("unsupported object: need array, {accounts:[]}, or auth.json map");
    }
  } else {
    throw new Error("JSON must be object, array, {accounts:[]}, or auth.json map");
  }
  return accounts.map((item, index) =>
    normalizeOne(item as Record<string, unknown>, index),
  );
}

export function summarizeAccounts(accounts: ImportAccount[]) {
  return {
    total: accounts.length,
    withRefresh: accounts.filter((a) => !!a.refresh_token).length,
    withUser: accounts.filter((a) => !!a.user_id).length,
    withEmail: accounts.filter((a) => !!a.email).length,
  };
}

export function formatImportResult(
  data: {
    added: number;
    updated: number;
    invalid: number;
    applied: boolean;
    items: Array<{ index: number; status: string; account_id?: string; message?: string }>;
  },
  preview: boolean,
): string {
  const items = Array.isArray(data.items) ? data.items : [];
  const lines = [
    preview
      ? "Preview (no write, no upstream validate)"
      : data.applied
        ? "Import done (validated + saved)"
        : "Import result",
    "added=" +
      String(data.added || 0) +
      "  updated=" +
      String(data.updated || 0) +
      "  invalid=" +
      String(data.invalid || 0) +
      "  applied=" +
      String(Boolean(data.applied)) +
      "  total_items=" +
      String(items.length),
  ];
  const bad = items.filter((item) => item && (item.status === "invalid" || item.message));
  if (bad.length) {
    lines.push("Issues:");
    for (const item of bad.slice(0, 40)) {
      lines.push(
        "- #" +
          String(item.index) +
          ": " +
          (item.status || "unknown") +
          (item.message ? " · " + item.message : "") +
          (item.account_id ? " · " + item.account_id : ""),
      );
    }
    if (bad.length > 40) lines.push("... +" + String(bad.length - 40) + " more");
  } else if (preview) {
    lines.push("No format invalid items. Commit will still validate upstream per account.");
  }
  if (items.length && items.length <= 20) {
    lines.push("", JSON.stringify(data, null, 2));
  } else if (items.length) {
    lines.push("", "(omitted " + String(items.length) + " item details)");
  }
  return lines.join(String.fromCharCode(10));
}
