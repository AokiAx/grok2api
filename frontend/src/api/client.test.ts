import { afterEach, describe, expect, it, vi } from "vitest";

function envelope(data: unknown, status = 200): Response {
  return new Response(
    JSON.stringify(
      status >= 400
        ? { ok: false, data: null, error: { code: "unauthorized", message: "Unauthorized" } }
        : { ok: true, data, error: null },
    ),
    { status, headers: { "Content-Type": "application/json" } },
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

  it("calls logout before clearing the in-memory bearer token", async () => {
    const calls: Array<{ path: string; authorization: string | null }> = [];
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const path = String(input);
      calls.push({ path, authorization: new Headers(init?.headers).get("Authorization") });
      if (path.endsWith("/auth/login")) return envelope({ tokens: { accessToken: "active" } });
      if (path.endsWith("/auth/logout")) return envelope({ loggedOut: true });
      return envelope({ id: "admin-1", username: "admin" });
    });
    vi.stubGlobal("fetch", fetchMock);

    const { adminApi } = await loadClient();
    await adminApi.login("secret", true);
    await adminApi.logout();
    await adminApi.me();

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
