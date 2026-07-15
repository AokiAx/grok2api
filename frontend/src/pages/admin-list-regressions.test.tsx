import { act, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";

const apiMocks = vi.hoisted(() => ({
  accounts: vi.fn(),
  batchAccounts: vi.fn(),
  accountEvents: vi.fn(),
  clientKeys: vi.fn(),
}));

vi.mock("@/api/client", async (importOriginal) => {
  const original = await importOriginal<typeof import("@/api/client")>();
  return {
    ...original,
    adminApi: {
      ...original.adminApi,
      ...apiMocks,
    },
  };
});

import { AccountsPage } from "@/pages/AccountsPage";
import { ClientKeysPage } from "@/pages/ClientKeysPage";

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((done) => {
    resolve = done;
  });
  return { promise, resolve };
}

function account(id: string, pool = "ready") {
  return {
    id,
    email: `${id}@example.test`,
    user_id: `user-${id}`,
    team_id: "",
    pool,
    unavailable_reason: "",
    last_error_code: "",
    quota_actual: 0,
    quota_limit: 100,
    request_count: 1,
    active: 0,
    max_active: 1,
    priority: 0,
    has_refresh_token: true,
  };
}

function accountPage(
  ids: string[],
  { page = 1, totalPages = 1, pool = "", q = "" } = {},
) {
  return {
    accounts: ids.map((id) => account(id, pool || "ready")),
    total: totalPages > 1 ? totalPages : ids.length,
    count: ids.length,
    page,
    page_size: 20,
    total_pages: totalPages,
    pool,
    q,
    summary: {},
  };
}

function clientKey(id: string, name: string) {
  return {
    id,
    name,
    origin: "managed",
    key_prefix: `g2a_${id}`,
    model_policy: "all",
    model_scopes: [],
    rpm_limit: 60,
    max_concurrent: 2,
    expires_at: null,
    revoked_at: null,
    last_used_at: null,
    created_at: "2026-07-15T00:00:00Z",
    updated_at: "2026-07-15T00:00:00Z",
  };
}

function clientKeyPage(id: string, name: string) {
  return { items: [clientKey(id, name)], total: 1, page: 1, page_size: 20 };
}

beforeEach(() => {
  vi.clearAllMocks();
  apiMocks.batchAccounts.mockResolvedValue({ updated: 1, deleted: 0, ids: [] });
  apiMocks.accountEvents.mockResolvedValue({ items: [], total: 0, page: 1, page_size: 20 });
  vi.spyOn(window, "confirm").mockReturnValue(true);
});

describe("AccountsPage query-scoped selection", () => {
  it("never includes an account hidden by pagination in a later batch action", async () => {
    apiMocks.accounts.mockImplementation(async ({ page }: { page: number }) => (
      page === 2
        ? accountPage(["page-two"], { page: 2, totalPages: 2 })
        : accountPage(["page-one"], { page: 1, totalPages: 2 })
    ));
    const user = userEvent.setup();
    render(<AccountsPage />);

    await user.click(await screen.findByRole("checkbox", { name: "选择账号 page-one" }));
    await user.click(screen.getByRole("button", { name: "下一页" }));
    await user.click(await screen.findByRole("checkbox", { name: "选择账号 page-two" }));
    await user.click(screen.getByRole("button", { name: "批量禁用" }));

    expect(apiMocks.batchAccounts).toHaveBeenCalledWith(["page-two"], "disable");
  });

  it("never includes an account hidden by a pool filter in a later batch action", async () => {
    apiMocks.accounts.mockImplementation(async ({ pool }: { pool: string }) => (
      pool === "unavailable"
        ? accountPage(["filtered-account"], { pool: "unavailable" })
        : accountPage(["visible-before-filter"])
    ));
    const user = userEvent.setup();
    render(<AccountsPage />);

    await user.click(await screen.findByRole("checkbox", { name: "选择账号 visible-before-filter" }));
    await user.click(screen.getByRole("button", { name: "unavailable" }));
    await user.click(await screen.findByRole("checkbox", { name: "选择账号 filtered-account" }));
    await user.click(screen.getByRole("button", { name: "批量恢复" }));

    expect(apiMocks.batchAccounts).toHaveBeenCalledWith(["filtered-account"], "recover");
  });
});

describe("admin list request ordering", () => {
  it("keeps the latest Accounts query visible when an older request resolves last", async () => {
    const slow = deferred<ReturnType<typeof accountPage>>();
    const fast = deferred<ReturnType<typeof accountPage>>();
    apiMocks.accounts.mockImplementation(({ q }: { q: string }) => {
      if (q === "slow") return slow.promise;
      if (q === "fast") return fast.promise;
      return Promise.resolve(accountPage(["initial-account"]));
    });
    const user = userEvent.setup();
    render(<AccountsPage />);
    await screen.findByText("initial-account@example.test");

    const search = screen.getByLabelText("搜索账号");
    await user.type(search, "slow");
    await user.click(screen.getByRole("button", { name: "查询" }));
    await waitFor(() => expect(apiMocks.accounts).toHaveBeenCalledWith(
      expect.objectContaining({ q: "slow" }),
    ));
    await user.clear(search);
    await user.type(search, "fast");
    await user.click(screen.getByRole("button", { name: "查询" }));
    await waitFor(() => expect(apiMocks.accounts).toHaveBeenCalledWith(
      expect.objectContaining({ q: "fast" }),
    ));

    await act(async () => fast.resolve(accountPage(["latest-account"], { q: "fast" })));
    expect(await screen.findByText("latest-account@example.test")).toBeInTheDocument();
    await act(async () => slow.resolve(accountPage(["stale-account"], { q: "slow" })));

    await waitFor(() => {
      expect(screen.getByText("latest-account@example.test")).toBeInTheDocument();
      expect(screen.queryByText("stale-account@example.test")).not.toBeInTheDocument();
    });
  });

  it("keeps the latest ClientKeys query visible when an older request resolves last", async () => {
    const slow = deferred<ReturnType<typeof clientKeyPage>>();
    const fast = deferred<ReturnType<typeof clientKeyPage>>();
    apiMocks.clientKeys.mockImplementation(({ q }: { q: string }) => {
      if (q === "slow") return slow.promise;
      if (q === "fast") return fast.promise;
      return Promise.resolve(clientKeyPage("initial", "initial key"));
    });
    const user = userEvent.setup();
    render(<ClientKeysPage />);
    await screen.findByText("initial key");

    const search = screen.getByLabelText("搜索客户端密钥");
    await user.type(search, "slow");
    await user.click(screen.getByRole("button", { name: "查询" }));
    await waitFor(() => expect(apiMocks.clientKeys).toHaveBeenCalledWith(
      expect.objectContaining({ q: "slow" }),
    ));
    await user.clear(search);
    await user.type(search, "fast");
    await user.click(screen.getByRole("button", { name: "查询" }));
    await waitFor(() => expect(apiMocks.clientKeys).toHaveBeenCalledWith(
      expect.objectContaining({ q: "fast" }),
    ));

    await act(async () => fast.resolve(clientKeyPage("latest", "latest key")));
    expect(await screen.findByText("latest key")).toBeInTheDocument();
    await act(async () => slow.resolve(clientKeyPage("stale", "stale key")));

    await waitFor(() => {
      expect(screen.getByText("latest key")).toBeInTheDocument();
      expect(screen.queryByText("stale key")).not.toBeInTheDocument();
    });
  });
});
