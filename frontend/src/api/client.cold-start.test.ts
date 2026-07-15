import { afterEach, describe, expect, it, vi } from "vitest";

function envelope(
  data: unknown,
  status = 200,
  error = { code: "unauthorized", message: "Unauthorized" },
): Response {
  return new Response(
    JSON.stringify(
      status >= 400
        ? { ok: false, data: null, error }
        : { ok: true, data, error: null },
    ),
    { status, headers: { "Content-Type": "application/json" } },
  );
}

afterEach(() => {
  vi.unstubAllGlobals();
  vi.resetModules();
});

describe("admin authentication cold start", () => {
  it("uses the HttpOnly refresh cookie to restore a session before retrying me", async () => {
    const calls: Array<{ path: string; authorization: string | null }> = [];
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const path = String(input);
      const authorization = new Headers(init?.headers).get("Authorization");
      calls.push({ path, authorization });

      if (path.endsWith("/auth/refresh")) {
        return envelope({ tokens: { accessToken: "restored-from-cookie" } });
      }
      if (path.endsWith("/auth/me") && authorization === "Bearer restored-from-cookie") {
        return envelope({ id: "admin-1", username: "admin" });
      }
      if (path.endsWith("/auth/me")) return envelope(null, 401);
      throw new Error(`Unexpected request: ${path}`);
    });
    vi.stubGlobal("fetch", fetchMock);
    vi.resetModules();

    const { adminApi } = await import("@/api/client");

    await expect(adminApi.me()).resolves.toEqual({ id: "admin-1", username: "admin" });
    expect(calls).toEqual([
      { path: "/api/admin/v1/auth/me", authorization: null },
      { path: "/api/admin/v1/auth/refresh", authorization: null },
      { path: "/api/admin/v1/auth/me", authorization: "Bearer restored-from-cookie" },
    ]);
  });

  it("does not publish session invalidation when no cold-start cookie exists", async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const path = String(input);
      if (path.endsWith("/auth/me") || path.endsWith("/auth/refresh")) {
        return envelope(null, 401);
      }
      throw new Error(`Unexpected request: ${path}`);
    });
    vi.stubGlobal("fetch", fetchMock);
    vi.resetModules();

    const { adminApi, subscribeAdminSessionInvalidated } = await import("@/api/client");
    const invalidated = vi.fn();
    const unsubscribe = subscribeAdminSessionInvalidated(invalidated);

    await expect(adminApi.me()).rejects.toMatchObject({ status: 401 });
    unsubscribe();

    expect(fetchMock.mock.calls.map(([path]) => String(path))).toEqual([
      "/api/admin/v1/auth/me",
      "/api/admin/v1/auth/refresh",
    ]);
    expect(invalidated).not.toHaveBeenCalled();
  });
});
