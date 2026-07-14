import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";

const apiMocks = vi.hoisted(() => ({
  accounts: vi.fn(),
  recoverAccount: vi.fn(),
  deleteAccount: vi.fn(),
  batchAccounts: vi.fn(),
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
});
