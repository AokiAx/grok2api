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
      return false;
    }
  }
  return false;
}

function refreshSingleFlight(generation = sessionGeneration): Promise<boolean> {
  if (generation !== sessionGeneration) return Promise.resolve(false);
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
  const hadAccessToken = authenticated && Boolean(accessToken);
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
  if (response.status === 401 && authenticated && (options.retryUnauthorized ?? true)) {
    if (await refreshSingleFlight(generation)) {
      const retried = await transport(path, init, { authenticated: true, retryUnauthorized: false });
      if (retried.status === 401) invalidateSession(generation);
      return retried;
    }
    if (hadAccessToken) invalidateSession(generation);
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

async function readDownloadResponse(path: string): Promise<Response> {
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
  return response;
}

function filenameFromDisposition(disposition: string, fallbackName: string): string {
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
  return filename;
}

function triggerBrowserDownload(blob: Blob, filename: string): void {
  const objectURL = URL.createObjectURL(blob);
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

async function downloadAttachment(path: string, fallbackName: string): Promise<void> {
  const response = await readDownloadResponse(path);
  const filename = filenameFromDisposition(response.headers.get("Content-Disposition") || "", fallbackName);
  triggerBrowserDownload(await response.blob(), filename);
}

/** Credential export endpoint returns raw JSON (not admin envelope). */
async function fetchCredentialExport(id: string): Promise<CredentialExport> {
  const response = await readDownloadResponse(
    `/api/admin/v1/accounts/${encodeURIComponent(id)}/credentials/export`,
  );
  const text = await response.text();
  try {
    return JSON.parse(text) as CredentialExport;
  } catch {
    throw new AdminApiError(response.status, "invalid_json", text || "Invalid credential export");
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
  period?: string;
  usage?: {
    requests?: number;
    successfulRequests?: number;
    failedRequests?: number;
    sampledRequests?: number;
    usageSource?: "upstream" | "none" | string;
    inputTokens?: number;
    cachedInputTokens?: number;
    outputTokens?: number;
    tokens?: number;
    p95DurationMs?: number;
    successRate?: number;
  };
  series?: Array<{
    bucketStart?: string;
    start?: string;
    end?: string;
    requests?: number;
    failures?: number;
    tokens?: number;
    inputTokens?: number;
    cachedInputTokens?: number;
    outputTokens?: number;
    models?: Array<{
      model?: string;
      tokens?: number;
      inputTokens?: number;
      cachedInputTokens?: number;
      outputTokens?: number;
    }>;
  }>;
  topModels?: Array<{
    name?: string;
    model?: string;
    count?: number;
    requests?: number;
    tokens?: number;
    inputTokens?: number;
    cachedInputTokens?: number;
    outputTokens?: number;
  }>;
  topAccounts?: Array<{
    name?: string;
    count?: number;
    requests?: number;
    tokens?: number;
    inputTokens?: number;
    cachedInputTokens?: number;
    outputTokens?: number;
  }>;
  recentFailures?: Array<{
    requestId?: string;
    startedAt?: string;
    model?: string;
    accountId?: string;
    statusCode?: number;
    errorType?: string;
    errorCode?: string;
    path?: string;
    durationMs?: number;
  }>;
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

export type ManagedModel = {
  id: string;
  upstream_id: string;
  name: string;
  api_backend: string;
  context_window: number;
  supports_reasoning_effort: boolean;
  reasoning_efforts: string[];
  supports_backend_search: boolean;
  owned_by: string;
  enabled: boolean;
  aliases: string[];
  source: string;
  created_at?: string;
  updated_at?: string;
};

export type ModelsList = {
  count: number;
  enabled: number;
  models: ManagedModel[];
};

export type SettingsDocument = {
  revision: number;
  updated_at: string;
  updated_by?: string;
  pool: {
    max_concurrent: number;
    max_attempts: number;
    strategy: string;
    active_size: number;
    sticky: boolean;
    sticky_ttl_minutes: number;
    quota_retry_minutes: number;
    rate_retry_seconds: number;
  };
  timeouts: {
    request_timeout_sec: number;
    acquire_timeout_sec: number;
  };
  audit: {
    retention_days: number;
  };
  proxy: {
    url: string;
    enabled: boolean;
    runtime_status: string;
    note?: string;
  };
  client_keys: {
    default_rpm_limit: number;
    default_max_concurrent: number;
  };
  device_auth: {
    issuer: string;
    client_id: string;
    scope: string;
  };
  debug_trace: {
    enabled: boolean;
    dir: string;
    errors_only: boolean;
  };
};


export type DeviceAuthSession = {
  id: string;
  status: string;
  issuer: string;
  client_id: string;
  scope?: string;
  user_code: string;
  verification_uri: string;
  verification_uri_complete?: string;
  interval_sec: number;
  expires_at: string;
  created_at: string;
  updated_at: string;
  completed_at?: string;
  last_error?: string;
  account_id?: string;
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
    const generation = sessionGeneration;
    if (!await refreshSingleFlight(generation)) {
      invalidateSession(generation);
      throw new AdminApiError(401, "unauthorized", "Session refresh failed");
    }
  },
  logout: async () => {
    const token = accessToken;
    sessionGeneration += 1;
    clearAccessToken();
    const headers = new Headers();
    if (token) headers.set("Authorization", `Bearer ${token}`);
    return request<{ loggedOut?: boolean; logged_out?: boolean }>(
      "/api/admin/v1/auth/logout",
      { method: "POST", headers },
      { retryUnauthorized: false },
    );
  },
  me: () => request<AdminIdentity>("/api/admin/v1/auth/me"),
  dashboard: (period?: "24h" | "7d" | "30d") =>
    request<Dashboard>(
      `/api/admin/v1/dashboard${period ? `?period=${encodeURIComponent(period)}` : ""}`,
    ),
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
  fetchCredentialExport: (id: string) => fetchCredentialExport(id),
  exportAllAccounts: async (
    onProgress?: (done: number, total: number) => void,
  ): Promise<ExportAllResult> => {
    const ids: string[] = [];
    let page = 1;
    let totalPages = 1;
    while (page <= totalPages) {
      const batch = await adminApi.accounts({ page, page_size: 100 });
      totalPages = Math.max(1, batch.total_pages || 1);
      for (const item of batch.accounts || []) {
        if (item.id) ids.push(item.id);
      }
      page += 1;
    }

    const accounts: CredentialExport[] = [];
    const failures: Array<{ id: string; message: string }> = [];
    for (let index = 0; index < ids.length; index += 1) {
      const id = ids[index];
      try {
        accounts.push(await fetchCredentialExport(id));
      } catch (error) {
        failures.push({
          id,
          message: error instanceof AdminApiError ? error.message : String(error),
        });
      }
      onProgress?.(index + 1, ids.length);
    }

    const stamp = new Date().toISOString().slice(0, 19).replace(/[:T]/g, "-");
    const payload = {
      exported_at: new Date().toISOString(),
      count: accounts.length,
      failed: failures.length,
      failures,
      accounts,
    };
    triggerBrowserDownload(
      new Blob([JSON.stringify(payload, null, 2)], { type: "application/json" }),
      `grok2api-accounts-export-${stamp}.json`,
    );
    return {
      total: ids.length,
      exported: accounts.length,
      failed: failures.length,
      failures,
    };
  },
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

  // ---- models registry ----
  models: (includeDisabled = true) =>
    request<ModelsList>(
      `/api/admin/v1/models${includeDisabled ? "?include_disabled=true" : ""}`,
    ),
  model: (id: string) =>
    request<ManagedModel>(`/api/admin/v1/models/${encodeURIComponent(id)}`),
  putModel: (id: string, body: Partial<ManagedModel> & { enabled?: boolean }) =>
    request<ManagedModel>(`/api/admin/v1/models/${encodeURIComponent(id)}`, {
      method: "PUT",
      body: JSON.stringify(body),
    }),
  patchModel: (id: string, body: Partial<ManagedModel> & { enabled?: boolean }) =>
    request<ManagedModel>(`/api/admin/v1/models/${encodeURIComponent(id)}`, {
      method: "PATCH",
      body: JSON.stringify(body),
    }),

  // ---- settings center ----
  settings: () => request<SettingsDocument>("/api/admin/v1/settings"),
  putSettings: (body: {
    expected_revision: number;
    pool: SettingsDocument["pool"];
    timeouts: SettingsDocument["timeouts"];
    audit: SettingsDocument["audit"];
    proxy: SettingsDocument["proxy"];
    client_keys: SettingsDocument["client_keys"];
    device_auth: SettingsDocument["device_auth"];
    debug_trace: SettingsDocument["debug_trace"];
  }) =>
    request<SettingsDocument>("/api/admin/v1/settings", {
      method: "PUT",
      body: JSON.stringify(body),
    }),
  // ---- Build Device OAuth ----
  startDeviceAuth: (input: { issuer?: string; client_id?: string; scope?: string } = {}) =>
    request<DeviceAuthSession>("/api/admin/v1/device-auth/sessions", {
      method: "POST",
      body: JSON.stringify(input),
    }),
  getDeviceAuth: (id: string) =>
    request<DeviceAuthSession>(`/api/admin/v1/device-auth/sessions/${encodeURIComponent(id)}`),
  pollDeviceAuth: (id: string) =>
    request<DeviceAuthSession>(`/api/admin/v1/device-auth/sessions/${encodeURIComponent(id)}/poll`, {
      method: "POST",
    }),
  cancelDeviceAuth: (id: string) =>
    request<DeviceAuthSession>(`/api/admin/v1/device-auth/sessions/${encodeURIComponent(id)}/cancel`, {
      method: "POST",
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

/** Raw credential payload from GET .../credentials/export (not admin envelope). */
export type CredentialExport = {
  id: string;
  key: string;
  refresh_token?: string;
  expires_at?: string;
  email?: string;
  oidc_issuer?: string;
  oidc_client_id?: string;
  user_id?: string;
  team_id?: string;
};

export type ExportAllResult = {
  total: number;
  exported: number;
  failed: number;
  failures: Array<{ id: string; message: string }>;
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
