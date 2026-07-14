/** Admin API v1 client — see docs/ADMIN_API_V1.md */

export type AdminEnvelope<T> = {
  ok: boolean;
  data: T | null;
  error: { code: string; message: string } | null;
};

export class AdminApiError extends Error {
  code: string;
  status: number;

  constructor(status: number, code: string, message: string) {
    super(message);
    this.name = "AdminApiError";
    this.status = status;
    this.code = code;
  }
}

const TOKEN_KEY = "grok2api.admin.token";
const REMEMBER_KEY = "grok2api.admin.remember";

export function getStoredToken(): string {
  try {
    const remember = localStorage.getItem(REMEMBER_KEY) !== "0";
    if (remember) {
      return localStorage.getItem(TOKEN_KEY) || sessionStorage.getItem(TOKEN_KEY) || "";
    }
    return sessionStorage.getItem(TOKEN_KEY) || "";
  } catch {
    return "";
  }
}

export function setStoredToken(token: string, remember: boolean): void {
  try {
    localStorage.setItem(REMEMBER_KEY, remember ? "1" : "0");
    sessionStorage.removeItem(TOKEN_KEY);
    localStorage.removeItem(TOKEN_KEY);
    if (!token) return;
    if (remember) localStorage.setItem(TOKEN_KEY, token);
    else sessionStorage.setItem(TOKEN_KEY, token);
  } catch {
    /* ignore */
  }
}

export function clearStoredToken(): void {
  setStoredToken("", true);
  try {
    sessionStorage.removeItem(TOKEN_KEY);
    localStorage.removeItem(TOKEN_KEY);
  } catch {
    /* ignore */
  }
}

async function request<T>(
  path: string,
  init: RequestInit = {},
  token?: string,
): Promise<T> {
  const headers = new Headers(init.headers);
  if (!headers.has("Content-Type") && init.body) {
    headers.set("Content-Type", "application/json");
  }
  const auth = token ?? getStoredToken();
  if (auth) headers.set("Authorization", `Bearer ${auth}`);

  const response = await fetch(path, { ...init, headers });
  const text = await response.text();
  let body: AdminEnvelope<T> | null = null;
  try {
    body = text ? (JSON.parse(text) as AdminEnvelope<T>) : null;
  } catch {
    throw new AdminApiError(response.status, "invalid_json", text || response.statusText);
  }
  if (!body) {
    throw new AdminApiError(response.status, "empty", "Empty response");
  }
  if (!body.ok || body.error) {
    throw new AdminApiError(
      response.status,
      body.error?.code || "error",
      body.error?.message || "Request failed",
    );
  }
  return body.data as T;
}

async function downloadAttachment(path: string, fallbackName: string): Promise<void> {
  const headers = new Headers();
  const auth = getStoredToken();
  if (auth) headers.set("Authorization", `Bearer ${auth}`);

  const response = await fetch(path, { headers });
  if (!response.ok) {
    const text = await response.text();
    try {
      const body = JSON.parse(text) as AdminEnvelope<unknown>;
      throw new AdminApiError(
        response.status,
        body.error?.code || "download_failed",
        body.error?.message || "Download failed",
      );
    } catch (error) {
      if (error instanceof AdminApiError) throw error;
      throw new AdminApiError(response.status, "download_failed", text || response.statusText);
    }
  }

  const disposition = response.headers.get("Content-Disposition") || "";
  const encodedName = disposition.match(/filename\*=UTF-8''([^;]+)/i)?.[1];
  const quotedName = disposition.match(/filename="([^"]+)"/i)?.[1];
  const filename = encodedName ? decodeURIComponent(encodedName) : quotedName || fallbackName;
  const objectURL = URL.createObjectURL(await response.blob());
  const anchor = document.createElement("a");
  anchor.href = objectURL;
  anchor.download = filename;
  anchor.style.display = "none";
  document.body.appendChild(anchor);
  try {
    anchor.click();
  } finally {
    anchor.remove();
    URL.revokeObjectURL(objectURL);
  }
}

export type SystemMeta = {
  auth_required: boolean;
  version: string;
  api_version: string;
  panel_paths?: string[];
};

export type LoginResult = {
  auth_required?: boolean;
  token?: string;
  token_type?: string;
  // Extended login response shape.
  admin?: { id: string; username: string };
  tokens?: {
    accessToken: string;
    accessTokenExpiresAt?: string;
    refreshTokenExpiresAt?: string;
  };
};

export type AccountStats = {
  total_accounts: number;
  ready_accounts: number;
  unavailable_accounts: number;
  active_leases: number;
  max_active: number;
  total_requests: number;
  refreshable_accounts: number;
  quota_actual: number;
  quota_limit: number;
  quota_remaining: number;
  ready_quota_remaining: number;
  quota_observed_accounts: number;
  ready_quota_observed_accounts: number;
  auth_fail_accounts: number;
  total_auth_fails: number;
  access_expired: number;
  access_expiring_soon: number;
  retry_due: number;
  no_refresh_token: number;
  reasons: Record<string, number>;
  error_codes: Record<string, number>;
};

export type PoolStatus = {
  ready: number;
  unavailable: number;
  reasons: Record<string, number>;
};

export type Dashboard = {
  summary: AccountStats;
  account_pool: PoolStatus;
  quota_circuit?: { open?: boolean; retry_at?: string; revision?: number } | null;
  generated_at: string;
};

export type PublicAccount = {
  id: string;
  email: string;
  user_id: string;
  team_id: string;
  pool: string;
  unavailable_reason: string;
  retry_at?: string;
  last_error_code: string;
  quota_actual: number;
  quota_limit: number;
  request_count: number;
  active: number;
  max_active: number;
  priority: number;
  has_refresh_token: boolean;
};

export type AccountsPage = {
  count: number;
  total: number;
  page: number;
  page_size: number;
  total_pages: number;
  pool: string;
  q: string;
  accounts: PublicAccount[];
  summary: AccountStats;
};

export type AccountEvent = {
  id: number;
  account_id: string;
  event_type: "state_transition" | "configuration" | "deletion";
  from_pool: string;
  to_pool: string;
  reason: string;
  error_code: string;
  details: Record<string, unknown>;
  created_at: string;
};

export type QuotaRefreshResult = {
  account_id: string;
  reason: string;
  error_code: string;
  actual: number;
  limit: number;
  observed: boolean;
};


function normalizeAccountItem(item: any): PublicAccount {
  // Normalize alternate AccountDTO fields into PublicAccount.
  if (item && item.pool) return item as PublicAccount;
  const quota = item?.quota || {};
  return {
    id: String(item?.id || ""),
    email: String(item?.email || ""),
    user_id: String(item?.userId || item?.user_id || ""),
    team_id: String(item?.teamId || item?.team_id || ""),
    pool: item?.enabled === false ? "unavailable" : "ready",
    unavailable_reason: item?.authStatus === "reauthRequired" ? "auth" : "",
    last_error_code: String(item?.lastError || item?.last_error_code || ""),
    quota_actual: Number(quota.used ?? item?.quota_actual ?? 0),
    quota_limit: Number(quota.limit ?? item?.quota_limit ?? 0),
    request_count: Number(item?.request_count ?? 0),
    active: Number(item?.active ?? 0),
    max_active: Number(item?.maxConcurrent ?? item?.max_active ?? 1),
    priority: Number(item?.priority ?? 0),
    has_refresh_token: Boolean(item?.refreshable ?? item?.has_refresh_token),
  };
}

export const adminApi = {
  meta: () => request<SystemMeta>("/api/admin/v1/system/meta", {}, ""),
  login: async (password: string) => {
    const data = await request<LoginResult>(
      "/api/admin/v1/auth/login",
      { method: "POST", body: JSON.stringify({ password, username: "admin" }) },
      "",
    );
    const token = data.tokens?.accessToken || data.token || password;
    return { ...data, token, auth_required: data.auth_required ?? true };
  },
  me: () => request<Record<string, unknown>>("/api/admin/v1/auth/me"),
  dashboard: () => request<Dashboard>("/api/admin/v1/dashboard"),
  pool: () =>
    request<{
      ready: number;
      unavailable: number;
      reasons: Record<string, number>;
      quota_circuit?: unknown;
    }>("/api/admin/v1/pool"),
  system: () =>
    request<{
      version: string;
      api_version: string;
      default_model: string;
      auth_required: boolean;
    }>("/api/admin/v1/system"),
  accounts: async (params: { pool?: string; q?: string; page?: number; page_size?: number }) => {
    const qs = new URLSearchParams();
    if (params.pool) qs.set("pool", params.pool);
    if (params.q) qs.set("q", params.q);
    if (params.page) qs.set("page", String(params.page));
    if (params.page_size) qs.set("page_size", String(params.page_size));
    const query = qs.toString();
    const data = await request<AccountsPage & {
      items?: PublicAccount[];
      pageSize?: number;
    }>(`/api/admin/v1/accounts${query ? `?${query}` : ""}`);
    // Normalize {items,pageSize} and {accounts,page_size} response variants.
    const accounts = data.accounts?.length ? data.accounts : (data.items || []).map(normalizeAccountItem);
    return {
      ...data,
      accounts,
      page: data.page || 1,
      page_size: data.page_size || data.pageSize || 20,
      total: data.total ?? accounts.length,
      total_pages: data.total_pages || Math.max(1, Math.ceil((data.total || accounts.length) / (data.page_size || data.pageSize || 20))),
      count: data.count ?? data.total ?? accounts.length,
      pool: data.pool || "",
      q: data.q || "",
      summary: data.summary,
    } as AccountsPage;
  },
  deleteAccount: (id: string) =>
    request<{ deleted: boolean; id: string }>(`/api/admin/v1/accounts/${encodeURIComponent(id)}`, {
      method: "DELETE",
    }),
  recoverAccount: (id: string) =>
    request<PublicAccount>(`/api/admin/v1/accounts/${encodeURIComponent(id)}/recover`, {
      method: "POST",
    }),
  batchAccounts: (ids: string[], action: "enable" | "disable" | "recover" | "delete") =>
    request<{ updated: number; deleted: number; ids: string[] }>("/api/admin/v1/accounts/batch", {
      method: "POST",
      body: JSON.stringify({ ids, action }),
    }),
  updateAccount: (id: string, update: { enabled?: boolean; priority?: number; max_active?: number }) =>
    request<PublicAccount>(`/api/admin/v1/accounts/${encodeURIComponent(id)}`, {
      method: "PATCH",
      body: JSON.stringify(update),
    }),
  accountEvents: (id: string, page = 1, pageSize = 20) =>
    request<{ items: AccountEvent[]; total: number; page: number; page_size: number }>(
      `/api/admin/v1/accounts/${encodeURIComponent(id)}/events?page=${page}&page_size=${pageSize}`,
    ),
  refreshCredential: (id: string) =>
    request<PublicAccount>(`/api/admin/v1/accounts/${encodeURIComponent(id)}/refresh-token`, {
      method: "POST",
    }),
  refreshQuota: (id: string) =>
    request<QuotaRefreshResult>(`/api/admin/v1/accounts/${encodeURIComponent(id)}/refresh-quota`, {
      method: "POST",
    }),
  exportCredential: (id: string) =>
    downloadAttachment(
      `/api/admin/v1/accounts/${encodeURIComponent(id)}/credentials/export`,
      `grok2api-account-${id}.json`,
    ),
  importPreview: (accounts: ImportAccount[]) =>
    request<ImportResult>("/api/admin/v1/accounts/import/preview", {
      method: "POST",
      body: JSON.stringify({ accounts, dry_run: true }),
    }),
  importCommit: (accounts: ImportAccount[]) =>
    request<ImportResult>("/api/admin/v1/accounts/import", {
      method: "POST",
      body: JSON.stringify({ accounts, dry_run: false }),
    }),
};

export type ImportAccount = {
  id?: string;
  key?: string;
  access_token?: string;
  refresh_token?: string;
  expires_in?: number;
  expires_at?: string;
  email?: string;
  oidc_issuer?: string;
  oidc_client_id?: string;
  user_id?: string;
  team_id?: string;
};

export type ImportItem = {
  index: number;
  status: string;
  account_id?: string;
  message?: string;
};

export type ImportResult = {
  added: number;
  updated: number;
  invalid: number;
  applied: boolean;
  items: ImportItem[];
};
