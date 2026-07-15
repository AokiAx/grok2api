import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";

const apiMocks = vi.hoisted(() => ({
  accounts: vi.fn(),
  recoverAccount: vi.fn(),
  deleteAccount: vi.fn(),
  batchAccounts: vi.fn(),
  updateAccount: vi.fn(),
  accountEvents: vi.fn(),
  refreshCredential: vi.fn(),
  refreshQuota: vi.fn(),
  exportCredential: vi.fn(),
  exportAllAccounts: vi.fn(),
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

describe("AccountsPage batch administration", () => {
  beforeEach(() => {
    apiMocks.accounts.mockResolvedValue({
      accounts: [
        {
          id: "a1",
          email: "a1@example.test",
          user_id: "u1",
          team_id: "",
          pool: "ready",
          unavailable_reason: "",
          last_error_code: "",
          quota_actual: 0,
          quota_limit: 100,
          request_count: 1,
          active: 0,
          max_active: 1,
          priority: 0,
          has_refresh_token: true,
        },
        {
          id: "a2",
          email: "a2@example.test",
          user_id: "u2",
          team_id: "",
          pool: "ready",
          unavailable_reason: "",
          last_error_code: "",
          quota_actual: 0,
          quota_limit: 100,
          request_count: 2,
          active: 0,
          max_active: 1,
          priority: 0,
          has_refresh_token: true,
        },
      ],
      total: 2,
      count: 2,
      page: 1,
      page_size: 20,
      total_pages: 1,
      pool: "",
      q: "",
      summary: {},
    });
    apiMocks.batchAccounts.mockResolvedValue({ updated: 2, deleted: 0, ids: ["a1", "a2"] });
    apiMocks.updateAccount.mockImplementation(async (_id, update) => ({
      id: "a1",
      email: "a1@example.test",
      user_id: "u1",
      team_id: "",
      pool: "ready",
      unavailable_reason: "",
      last_error_code: "",
      quota_actual: 0,
      quota_limit: 100,
      request_count: 1,
      active: 0,
      priority: update.priority ?? 0,
      max_active: update.max_active ?? 1,
      has_refresh_token: true,
    }));
    apiMocks.accountEvents.mockResolvedValue({ items: [], total: 0, page: 1, page_size: 20 });
    apiMocks.refreshCredential.mockResolvedValue({ id: "a1", pool: "ready" });
    apiMocks.refreshQuota.mockResolvedValue({ account_id: "a1", actual: 1, limit: 100, observed: true });
    apiMocks.exportCredential.mockResolvedValue(undefined);
    apiMocks.exportAllAccounts.mockResolvedValue({ total: 2, exported: 2, failed: 0, failures: [] });
    vi.spyOn(window, "confirm").mockReturnValue(true);
  });

  it("selects the visible page and disables the accounts as one batch", async () => {
    const user = userEvent.setup();
    render(<AccountsPage />);

    expect(await screen.findByText("a1@example.test")).toBeInTheDocument();
    await user.click(screen.getByRole("checkbox", { name: "选择全部账号" }));
    await user.click(screen.getByRole("button", { name: "批量禁用" }));

    expect(apiMocks.batchAccounts).toHaveBeenCalledWith(["a1", "a2"], "disable");
  });

  it("edits scheduler controls from the account detail panel", async () => {
    const user = userEvent.setup();
    render(<AccountsPage />);

    await user.click(await screen.findByText("a1@example.test"));
    const priority = screen.getByRole("spinbutton", { name: "优先级" });
    const concurrency = screen.getByRole("spinbutton", { name: "最大并发" });
    await user.clear(priority);
    await user.type(priority, "25");
    await user.clear(concurrency);
    await user.type(concurrency, "3");
    await user.click(screen.getByRole("button", { name: "保存账号设置" }));

    expect(apiMocks.updateAccount).toHaveBeenCalledWith("a1", { priority: 25, max_active: 3 });
  });

  it("runs explicit credential and quota maintenance actions", async () => {
    const user = userEvent.setup();
    render(<AccountsPage />);

    await user.click(await screen.findByText("a1@example.test"));
    await user.click(screen.getByRole("button", { name: "刷新 Token" }));
    await user.click(screen.getByRole("button", { name: "刷新额度" }));
    await user.click(screen.getByRole("button", { name: "导出凭据" }));

    expect(apiMocks.refreshCredential).toHaveBeenCalledWith("a1");
    expect(apiMocks.refreshQuota).toHaveBeenCalledWith("a1");
    expect(apiMocks.exportCredential).toHaveBeenCalledWith("a1");
  });

  it("opens import menu actions for oauth, file import, and bulk export", async () => {
    const user = userEvent.setup();
    render(<AccountsPage />);

    await screen.findByText("a1@example.test");
    await user.click(screen.getByRole("button", { name: "导入与导出" }));
    expect(screen.getByRole("menuitem", { name: /Device OAuth/i })).toBeInTheDocument();
    expect(screen.getByRole("menuitem", { name: /导入账号文件/ })).toBeInTheDocument();
    expect(screen.getByRole("menuitem", { name: /导出所有账号/ })).toBeInTheDocument();

    await user.click(screen.getByRole("menuitem", { name: /Device OAuth/i }));
    expect(await screen.findByText("Build Device OAuth")).toBeInTheDocument();
    expect(screen.queryByText("凭证文件")).not.toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "导入与导出" }));
    await user.click(screen.getByRole("menuitem", { name: /导入账号文件/ }));
    expect(await screen.findByText("凭证文件")).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "导入与导出" }));
    await user.click(screen.getByRole("menuitem", { name: /导出所有账号/ }));
    expect(apiMocks.exportAllAccounts).toHaveBeenCalled();
  });
});
