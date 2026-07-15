/** Admin API v1 client — see docs/ADMIN_API_V1.md */

export type AdminEnvelope<T> = {
  ok: boolean;
  data: T | null;
  error: { code: string; message: string } | null;
};

export class AdminApiError extends Error {
  code: string;
  status: number;
  retryAfter: string | null;

  constructor(status: number, code: string, message: string, retryAfter: string | null = null) {
    super(message);
    this.name = "AdminApiError";
    this.status = status;
    this.code = code;
    this.retryAfter = retryAfter;
  }
}

const LEGACY_TOKEN_KEY = "grok2api.admin.token";
const LEGACY_REMEMBER_KEY = "grok2api.admin.remember";
let accessToken = "";
let sessionGeneration = 0;
let refreshFlight: { generation: number; promise: Promise<boolean> } | null = null;
const sessionInvalidatedListeners = new Set<() => void>();

export function subscribeAdminSessionInvalidated(listener: () => void): () => void {
  sessionInvalidatedListeners.add(listener);
  return () => sessionInvalidatedListeners.delete(listener);
}

function clearLegacyAuthStorage(): void {
  try {
    localStorage.removeItem(LEGACY_TOKEN_KEY);
    localStorage.removeItem(LEGACY_REMEMBER_KEY);
    sessionStorage.removeItem(LEGACY_TOKEN_KEY);
    sessionStorage.removeItem(LEGACY_REMEMBER_KEY);
  } catch {
    /* Storage may be disabled; the in-memory session still works. */
  }
}

function setAccessToken(token: string): void {
  accessToken = token.trim();
}

function clearAccessToken(): void {
  accessToken = "";
}

function invalidateSession(generation: number): void {
  if (generation !== sessionGeneration) return;
  sessionGeneration += 1;
  clearAccessToken();
  sessionInvalidatedListeners.forEach((listener) => listener());
}

clearLegacyAuthStorage();

type TokenPayload = {
  accessToken?: string;
  access_token?: string;
  token?: string;
  tokens?: {
    accessToken?: string;
    access_token?: string;
  };
};

function extractAccessToken(data: TokenPayload): string {
  return (
    data.tokens?.accessToken ||
    data.tokens?.access_token ||
    data.accessToken ||
    data.access_token ||
    data.token ||
    ""
  ).trim();
}

async function parseEnvelope<T>(response: Response): Promise<T> {
  const text = await response.text();
  let body: AdminEnvelope<T> | null = null;
  try {
    body = text ? (JSON.parse(text) as AdminEnvelope<T>) : null;
  } catch {
    throw new AdminApiError(
      response.status,
      "invalid_json",
      text || response.statusText,
      response.headers.get("Retry-After"),
    );
  }
  if (!body) {
    throw new AdminApiError(
      response.status,
      "empty",
      "Empty response",
      response.headers.get("Retry-After"),
    );
  }
  if (!response.ok || !body.ok || body.error) {
    throw new AdminApiError(
      response.status,
      body.error?.code || "error",
      body.error?.message || "Request failed",
      response.headers.get("Retry-After"),
    );
  }
  return body.data as T;
}

async function refreshAccessToken(generation: number): Promise<boolean> {
  for (let attempt = 0; attempt < 2; attempt += 1) {
    try {
      const response = await fetch("/api/admin/v1/auth/refresh", {
        method: "POST",
        credentials: "include",
      });
      const data = await parseEnvelope<TokenPayload>(response);
      const nextToken = extractAccessToken(data);
      if (!nextToken) {
        throw new AdminApiError(response.status, "invalid_session", "Refresh response omitted access token");
      }
      if (generation !== sessionGeneration) return false;
      setAccessToken(nextToken);
      return true;
    } catch (error) {
      const isConflict = error instanceof AdminApiError
        && error.status === 409
        && error.code === "refresh_conflict";
      if (isConflict && attempt === 0) {
        await new Promise((resolve) => window.setTimeout(resolve, 50));
        if (generation !== sessionGeneration) return false;
        continue;
      }
      invalidateSession(generation);
      return false;
    }
  }
  invalidateSession(generation);
  return false;
}

function refreshSingleFlight(): Promise<boolean> {
  const generation = sessionGeneration;
  if (refreshFlight?.generation === generation) return refreshFlight.promise;

  const promise = refreshAccessToken(generation).finally(() => {
    if (refreshFlight?.promise === promise) refreshFlight = null;
  });
  refreshFlight = { generation, promise };
  return promise;
}

type TransportOptions = {
  authenticated?: boolean;
  retryUnauthorized?: boolean;
};

async function transport(
  path: string,
  init: RequestInit = {},
  options: TransportOptions = {},
): Promise<Response> {
  const generation = sessionGeneration;
  const authenticated = options.authenticated ?? true;
  const headers = new Headers(init.headers);
  if (!headers.has("Content-Type") && init.body) {
    headers.set("Content-Type", "application/json");
  }
  if (authenticated && accessToken) {
    headers.set("Authorization", `Bearer ${accessToken}`);
  } else if (!authenticated) {
    headers.delete("Authorization");
  }

  const response = await fetch(path, { ...init, headers, credentials: "include" });
  if (
    response.status === 401 &&
    authenticated &&
    (options.retryUnauthorized ?? true) &&
    await refreshSingleFlight()
  ) {
    const retried = await transport(path, init, { authenticated: true, retryUnauthorized: false });
    if (retried.status === 401) invalidateSession(generation);
    return retried;
  }
  return response;
}

async function request<T>(
  path: string,
  init: RequestInit = {},
  options: TransportOptions = {},
): Promise<T> {
  return parseEnvelope<T>(await transport(path, init, options));
}

async function downloadAttachment(path: string, fallbackName: string): Promise<void> {
  const response = await transport(path);
  if (!response.ok) {
    const text = await response.text();
    try {
      const body = JSON.parse(text) as AdminEnvelope<unknown>;
      throw new AdminApiError(
        response.status,
        body.error?.code || "download_failed",
        body.error?.message || "Download failed",
        response.headers.get("Retry-After"),
      );
    } catch (error) {
      if (error instanceof AdminApiError) throw error;
      throw new AdminApiError(
        response.status,
        "download_failed",
        text || response.statusText,
        response.headers.get("Retry-After"),
      );
    }
  }

  const disposition = response.headers.get("Content-Disposition") || "";
  const encodedName = disposition.match(/filename\*=UTF-8''([^;]+)/i)?.[1];
  const quotedName = disposition.match(/filename="([^"]+)"/i)?.[1];
  const plainName = disposition.match(/filename=([^;]+)/i)?.[1]?.trim();
  let filename = quotedName || plainName || fallbackName;
  if (encodedName) {
    try {
      filename = decodeURIComponent(encodedName);
    } catch {
      filename = encodedName;
    }
  }
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
  setup_required?: boolean;
  version: string;
  api_version: string;
  panel_paths?: string[];
};

export type AdminIdentity = {
  id: string;
  username: string;
};

export type LoginResult = TokenPayload & {
  auth_required?: boolean;
  token_type?: string;
  admin?: AdminIdentity;
  tokens?: {
    accessToken?: string;
    access_token?: string;
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

export type ClientKeyModelPolicy = "all" | "allowlist";

export type ClientKey = {
  id: string;
  name: string;
  origin: "managed" | "config_api_key";
  key_prefix: string;
  model_policy: ClientKeyModelPolicy;
  model_scopes: string[];
  rpm_limit: number;
  max_concurrent: number;
  expires_at: string | null;
  revoked_at: string | null;
  last_used_at: string | null;
  created_at: string;
  updated_at: string;
  secret?: string;
};

export type ClientKeysPage = {
  items: ClientKey[];
  total: number;
  page: number;
  page_size: number;
};

export type ClientKeyInput = {
  name: string;
  model_policy: ClientKeyModelPolicy;
  model_scopes: string[];
  rpm_limit: number;
  max_concurrent: number;
  expires_at: string | null;
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
  meta: () => request<SystemMeta>("/api/admin/v1/system/meta", {}, { authenticated: false }),
  login: async (password: string, remember: boolean) => {
    const data = await request<LoginResult>(
      "/api/admin/v1/auth/login",
      {
        method: "POST",
        body: JSON.stringify({ username: "admin", password, remember }),
      },
      { authenticated: false, retryUnauthorized: false },
    );
    const token = extractAccessToken(data);
    if (!token) {
      throw new AdminApiError(200, "invalid_session", "Login response omitted access token");
    }
    sessionGeneration += 1;
    setAccessToken(token);
    return data;
  },
  refresh: async () => {
    if (!await refreshSingleFlight()) {
      throw new AdminApiError(401, "unauthorized", "Session refresh failed");
    }
  },
  logout: async () => {
    try {
      return await request<{ loggedOut?: boolean; logged_out?: boolean }>(
        "/api/admin/v1/auth/logout",
        { method: "POST" },
      );
    } finally {
      sessionGeneration += 1;
      clearAccessToken();
    }
  },
  me: () => request<AdminIdentity>("/api/admin/v1/auth/me"),
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
  clientKeys: (params: { q?: string; origin?: string; page?: number; page_size?: number }) => {
    const query = new URLSearchParams();
    if (params.q) query.set("q", params.q);
    if (params.origin) query.set("origin", params.origin);
    if (params.page) query.set("page", String(params.page));
    if (params.page_size) query.set("page_size", String(params.page_size));
    const suffix = query.toString();
    return request<ClientKeysPage>(`/api/admin/v1/client-keys${suffix ? `?${suffix}` : ""}`);
  },
  createClientKey: (input: ClientKeyInput) =>
    request<ClientKey>("/api/admin/v1/client-keys", {
      method: "POST",
      body: JSON.stringify(input),
    }),
  clientKey: (id: string) =>
    request<ClientKey>(`/api/admin/v1/client-keys/${encodeURIComponent(id)}`),
  updateClientKey: (id: string, input: ClientKeyInput) =>
    request<ClientKey>(`/api/admin/v1/client-keys/${encodeURIComponent(id)}`, {
      method: "PATCH",
      body: JSON.stringify(input),
    }),
  revokeClientKey: (id: string) =>
    request<ClientKey>(`/api/admin/v1/client-keys/${encodeURIComponent(id)}/revoke`, {
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
