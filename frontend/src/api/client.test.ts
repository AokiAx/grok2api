import { afterEach, describe, expect, it, vi } from "vitest";

function envelope(
  data: unknown,
  status = 200,
  error = { code: "unauthorized", message: "Unauthorized" },
  headers: Record<string, string> = {},
): Response {
  return new Response(
    JSON.stringify(
      status >= 400
        ? { ok: false, data: null, error }
        : { ok: true, data, error: null },
    ),
    { status, headers: { "Content-Type": "application/json", ...headers } },
  );
}

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((done) => {
    resolve = done;
  });
  return { promise, resolve };
}

async function loadClient() {
  vi.resetModules();
  return import("@/api/client");
}

afterEach(() => {
  vi.unstubAllGlobals();
  localStorage.clear();
  sessionStorage.clear();
});

describe("admin authentication transport", () => {
  it("clears legacy persisted tokens at startup and keeps login access tokens in memory", async () => {
    localStorage.setItem("grok2api.admin.token", "legacy-local");
    localStorage.setItem("grok2api.admin.remember", "1");
    sessionStorage.setItem("grok2api.admin.token", "legacy-session");
    const fetchMock = vi.fn().mockResolvedValue(
      envelope({
        admin: { id: "admin-1", username: "admin" },
        tokens: { accessToken: "memory-only" },
      }),
    );
    vi.stubGlobal("fetch", fetchMock);

    const { adminApi } = await loadClient();
    await adminApi.login("correct horse", false);

    expect(localStorage.getItem("grok2api.admin.token")).toBeNull();
    expect(sessionStorage.getItem("grok2api.admin.token")).toBeNull();
    expect(localStorage.getItem("grok2api.admin.remember")).toBeNull();
    expect(fetchMock).toHaveBeenCalledWith(
      "/api/admin/v1/auth/login",
      expect.objectContaining({
        method: "POST",
        credentials: "include",
        body: JSON.stringify({ username: "admin", password: "correct horse", remember: false }),
      }),
    );
  });

  it("uses one refresh request for concurrent 401 responses and retries each request once", async () => {
    const calls: Array<{ path: string; authorization: string | null }> = [];
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const path = String(input);
      const authorization = new Headers(init?.headers).get("Authorization");
      calls.push({ path, authorization });
      if (path.endsWith("/auth/login")) {
        return envelope({ tokens: { accessToken: "expired" } });
      }
      if (path.endsWith("/auth/refresh")) {
        await Promise.resolve();
        return envelope({ accessToken: "fresh" });
      }
      if (authorization === "Bearer expired") return envelope(null, 401);
      if (path.endsWith("/auth/me")) return envelope({ id: "admin-1", username: "admin" });
      return envelope({ version: "1", api_version: "v1", default_model: "grok", auth_required: true });
    });
    vi.stubGlobal("fetch", fetchMock);

    const { adminApi } = await loadClient();
    await adminApi.login("secret", true);
    await Promise.all([adminApi.me(), adminApi.system()]);

    expect(calls.filter((call) => call.path.endsWith("/auth/refresh"))).toHaveLength(1);
    expect(calls.filter((call) => call.authorization === "Bearer fresh")).toEqual(
      expect.arrayContaining([
        expect.objectContaining({ path: "/api/admin/v1/auth/me" }),
        expect.objectContaining({ path: "/api/admin/v1/system" }),
      ]),
    );
    expect(fetchMock).toHaveBeenCalledWith(
      "/api/admin/v1/auth/refresh",
      expect.objectContaining({ method: "POST", credentials: "include" }),
    );
  });

  it("does not loop when refresh is rejected", async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const path = String(input);
      if (path.endsWith("/auth/login")) return envelope({ tokens: { accessToken: "expired" } });
      return envelope(null, 401);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { adminApi } = await loadClient();
    await adminApi.login("secret", true);
    await expect(adminApi.me()).rejects.toMatchObject({ status: 401 });

    expect(fetchMock).toHaveBeenCalledTimes(3);
    expect(fetchMock.mock.calls.filter(([path]) => String(path).endsWith("/auth/refresh"))).toHaveLength(1);
  });

  it("retries one refresh conflict so another tab's winning cookie can restore the session", async () => {
    let refreshCalls = 0;
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const path = String(input);
      const authorization = new Headers(init?.headers).get("Authorization");
      if (path.endsWith("/auth/login")) return envelope({ tokens: { accessToken: "expired" } });
      if (path.endsWith("/auth/refresh")) {
        refreshCalls += 1;
        if (refreshCalls === 1) {
          return envelope(null, 409, { code: "refresh_conflict", message: "Refresh already rotated" });
        }
        return envelope({ tokens: { accessToken: "winner-token" } });
      }
      if (path.endsWith("/auth/me") && authorization === "Bearer expired") return envelope(null, 401);
      return envelope({ id: "admin-1", username: "admin" });
    });
    vi.stubGlobal("fetch", fetchMock);

    const { adminApi } = await loadClient();
    await adminApi.login("secret", false);
    await adminApi.me();

    expect(refreshCalls).toBe(2);
    expect(fetchMock.mock.calls.some(([, init]) => new Headers(init?.headers).get("Authorization") === "Bearer winner-token")).toBe(true);
  });

  it("stops after one refresh-conflict retry and publishes final session invalidation", async () => {
    const invalidated = vi.fn();
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const path = String(input);
      if (path.endsWith("/auth/login")) return envelope({ tokens: { accessToken: "expired" } });
      if (path.endsWith("/auth/refresh")) {
        return envelope(null, 409, { code: "refresh_conflict", message: "Refresh already rotated" });
      }
      return envelope(null, 401);
    });
    vi.stubGlobal("fetch", fetchMock);

    const { adminApi, subscribeAdminSessionInvalidated } = await loadClient();
    const unsubscribe = subscribeAdminSessionInvalidated(invalidated);
    await adminApi.login("secret", false);
    await expect(adminApi.me()).rejects.toMatchObject({ status: 401 });
    unsubscribe();

    expect(fetchMock.mock.calls.filter(([path]) => String(path).endsWith("/auth/refresh"))).toHaveLength(2);
    expect(invalidated).toHaveBeenCalledOnce();
  });

  it("publishes final session invalidation when the refreshed request is still unauthorized", async () => {
    const invalidated = vi.fn();
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const path = String(input);
      const authorization = new Headers(init?.headers).get("Authorization");
      if (path.endsWith("/auth/login")) return envelope({ tokens: { accessToken: "expired" } });
      if (path.endsWith("/auth/refresh")) return envelope({ tokens: { accessToken: "fresh" } });
      if (path.endsWith("/auth/me") && authorization === "Bearer expired") return envelope(null, 401);
      if (path.endsWith("/auth/me") && authorization === "Bearer fresh") return envelope(null, 401);
      return envelope({ id: "admin-1", username: "admin" });
    });
    vi.stubGlobal("fetch", fetchMock);

    const { adminApi, subscribeAdminSessionInvalidated } = await loadClient();
    const unsubscribe = subscribeAdminSessionInvalidated(invalidated);
    await adminApi.login("secret", false);
    await expect(adminApi.me()).rejects.toMatchObject({ status: 401 });
    unsubscribe();

    expect(fetchMock.mock.calls.filter(([path]) => String(path).endsWith("/auth/refresh"))).toHaveLength(1);
    expect(invalidated).toHaveBeenCalledOnce();
  });

  it("preserves Retry-After on structured admin API errors", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(
      envelope(
        null,
        429,
        { code: "login_rate_limited", message: "Too many attempts" },
        { "Retry-After": "37" },
      ),
    ));

    const { adminApi } = await loadClient();

    await expect(adminApi.login("wrong", false)).rejects.toMatchObject({
      status: 429,
      code: "login_rate_limited",
      retryAfter: "37",
    });
  });

  it("uses the authenticated refresh transport for credential attachment downloads", async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const path = String(input);
      const authorization = new Headers(init?.headers).get("Authorization");
      if (path.endsWith("/auth/login")) return envelope({ tokens: { accessToken: "expired" } });
      if (path.endsWith("/auth/refresh")) return envelope({ tokens: { accessToken: "fresh" } });
      if (authorization === "Bearer expired") return envelope(null, 401);
      return new Response("credential", {
        status: 200,
        headers: { "Content-Disposition": "attachment; filename=account.json" },
      });
    });
    vi.stubGlobal("fetch", fetchMock);
    const createObjectURL = vi.fn(() => "blob:credential");
    const revokeObjectURL = vi.fn();
    vi.stubGlobal("URL", Object.assign(URL, { createObjectURL, revokeObjectURL }));
    const click = vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(() => undefined);

    const { adminApi } = await loadClient();
    await adminApi.login("secret", true);
    await adminApi.exportCredential("a1");

    expect(fetchMock.mock.calls.filter(([path]) => String(path).endsWith("/auth/refresh"))).toHaveLength(1);
    expect(click).toHaveBeenCalledOnce();
    expect(createObjectURL).toHaveBeenCalledOnce();
    expect(revokeObjectURL).toHaveBeenCalledWith("blob:credential");
  });

  it("clears the bearer token before a hanging logout request while preserving its captured header", async () => {
    const logoutResponse = deferred<Response>();
    const calls: Array<{ path: string; authorization: string | null }> = [];
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const path = String(input);
      calls.push({ path, authorization: new Headers(init?.headers).get("Authorization") });
      if (path.endsWith("/auth/login")) return envelope({ tokens: { accessToken: "active" } });
      if (path.endsWith("/auth/logout")) return logoutResponse.promise;
      return envelope({ id: "admin-1", username: "admin" });
    });
    vi.stubGlobal("fetch", fetchMock);

    const { adminApi } = await loadClient();
    await adminApi.login("secret", true);
    const pendingLogout = adminApi.logout();
    await Promise.resolve();
    await adminApi.me();
    logoutResponse.resolve(envelope({ loggedOut: true }));
    await pendingLogout;

    expect(calls.find((call) => call.path.endsWith("/auth/logout"))?.authorization).toBe("Bearer active");
    expect(calls.at(-1)).toEqual({ path: "/api/admin/v1/auth/me", authorization: null });
  });

  it("does not let an in-flight refresh restore authentication after logout completes", async () => {
    const refreshResponse = deferred<Response>();
    const refreshStarted = deferred<void>();
    const calls: Array<{ path: string; authorization: string | null }> = [];
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const path = String(input);
      const authorization = new Headers(init?.headers).get("Authorization");
      calls.push({ path, authorization });
      if (path.endsWith("/auth/login")) return envelope({ tokens: { accessToken: "active" } });
      if (path.endsWith("/auth/refresh")) {
        refreshStarted.resolve();
        return refreshResponse.promise;
      }
      if (path.endsWith("/auth/logout")) return envelope({ loggedOut: true });
      if (path.endsWith("/auth/me") && authorization === "Bearer active") return envelope(null, 401);
      if (path.endsWith("/auth/me")) return envelope({ id: "admin-1", username: "admin" });
      return envelope({ version: "1", api_version: "v1", default_model: "grok", auth_required: true });
    });
    vi.stubGlobal("fetch", fetchMock);

    const { adminApi } = await loadClient();
    await adminApi.login("secret", true);
    const pendingRequest = adminApi.me();
    const rejectedRequest = expect(pendingRequest).rejects.toMatchObject({ status: 401 });
    await refreshStarted.promise;

    await adminApi.logout();
    refreshResponse.resolve(envelope({ tokens: { accessToken: "stale-refresh-result" } }));

    await rejectedRequest;
    await adminApi.system();
    expect(calls.filter((call) => call.path.endsWith("/auth/me"))).toHaveLength(1);
    expect(calls.at(-1)).toEqual({ path: "/api/admin/v1/system", authorization: null });
  });
});

describe("client-key administration API", () => {
  it("maps list, create, detail, update, and revoke contracts", async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const path = String(input);
      const method = init?.method || "GET";
      if (path.includes("/client-keys?") && method === "GET") {
        return envelope({ items: [], total: 0, page: 1, page_size: 20 });
      }
      if (path.endsWith("/client-keys") && method === "POST") {
        return envelope({
          id: "ck_1",
          name: "automation",
          origin: "managed",
          key_prefix: "g2a_abcd",
          model_policy: "all",
          model_scopes: [],
          rpm_limit: 0,
          max_concurrent: 0,
          secret: "g2a_secret",
        }, 201);
      }
      if (path.endsWith("/client-keys/ck_1/revoke")) {
        return envelope({ id: "ck_1", name: "automation", revoked_at: "2026-07-15T00:00:00Z" });
      }
      if (path.endsWith("/client-keys/ck_1") && method === "PATCH") {
        return envelope({ id: "ck_1", name: "renamed", model_policy: "allowlist", model_scopes: ["grok-4"] });
      }
      return envelope({ id: "ck_1", name: "automation", model_policy: "all", model_scopes: [] });
    });
    vi.stubGlobal("fetch", fetchMock);

    const { adminApi } = await loadClient();
    await adminApi.clientKeys({ q: "auto", origin: "managed", page: 1, page_size: 20 });
    await adminApi.createClientKey({
      name: "automation",
      model_policy: "all",
      model_scopes: [],
      rpm_limit: 0,
      max_concurrent: 0,
      expires_at: null,
    });
    await adminApi.clientKey("ck_1");
    await adminApi.updateClientKey("ck_1", {
      name: "renamed",
      model_policy: "allowlist",
      model_scopes: ["grok-4"],
      rpm_limit: 60,
      max_concurrent: 2,
      expires_at: null,
    });
    await adminApi.revokeClientKey("ck_1");

    expect(fetchMock).toHaveBeenCalledWith(
      "/api/admin/v1/client-keys?q=auto&origin=managed&page=1&page_size=20",
      expect.objectContaining({ credentials: "include" }),
    );
    expect(fetchMock).toHaveBeenCalledWith(
      "/api/admin/v1/client-keys",
      expect.objectContaining({
        method: "POST",
        body: JSON.stringify({
          name: "automation",
          model_policy: "all",
          model_scopes: [],
          rpm_limit: 0,
          max_concurrent: 0,
          expires_at: null,
        }),
      }),
    );
    expect(fetchMock).toHaveBeenCalledWith(
      "/api/admin/v1/client-keys/ck_1",
      expect.objectContaining({ method: "PATCH" }),
    );
    expect(fetchMock).toHaveBeenCalledWith(
      "/api/admin/v1/client-keys/ck_1/revoke",
      expect.objectContaining({ method: "POST" }),
    );
  });
});
