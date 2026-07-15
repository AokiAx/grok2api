import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { LoginPage } from "@/pages/LoginPage";
import { AdminApiError } from "@/api/client";

const authMocks = vi.hoisted(() => ({
  login: vi.fn(),
  refreshMeta: vi.fn(),
  auth: {
    ready: true,
    meta: { auth_required: true, setup_required: false, version: "1", api_version: "v1" } as {
      auth_required: boolean;
      setup_required: boolean;
      version: string;
      api_version: string;
    } | null,
    error: null as string | null,
  },
}));

vi.mock("@/auth/AuthContext", () => ({
  useAuth: () => ({ ...authMocks.auth, login: authMocks.login, refreshMeta: authMocks.refreshMeta }),
  useIsAuthenticated: () => false,
}));

function renderPage() {
  return render(<MemoryRouter><LoginPage /></MemoryRouter>);
}

beforeEach(() => {
  vi.clearAllMocks();
  authMocks.login.mockResolvedValue(undefined);
  authMocks.auth.ready = true;
  authMocks.auth.error = null;
  authMocks.auth.meta = {
    auth_required: true,
    setup_required: false,
    version: "1",
    api_version: "v1",
  };
});

describe("LoginPage", () => {
  it("defaults remember to false and submits that explicit preference", async () => {
    const user = userEvent.setup();
    renderPage();

    expect(screen.getByRole("checkbox", { name: "记住登录" })).not.toBeChecked();
    await user.type(screen.getByLabelText("管理员密码"), "secret");
    await user.click(screen.getByRole("button", { name: "进入面板" }));

    expect(authMocks.login).toHaveBeenCalledWith("secret", false);
  });

  it("shows PowerShell and Bash bootstrap commands and removes the login form during setup", () => {
    authMocks.auth.meta = {
      auth_required: true,
      setup_required: true,
      version: "1",
      api_version: "v1",
    };

    renderPage();

    expect(screen.getByRole("status")).toHaveTextContent("bootstrap-admin");
    expect(screen.getByText("PowerShell")).toBeInTheDocument();
    expect(screen.getByText("Bash")).toBeInTheDocument();
    expect(screen.getByText(/^\$env:ADMIN_PASSWORD \| docker compose run --rm -T app bootstrap-admin/)).toBeInTheDocument();
    expect(screen.getByText(/^printf '%s\\n' "\$ADMIN_PASSWORD" \| docker compose run --rm -T app bootstrap-admin/)).toBeInTheDocument();
    expect(screen.queryByLabelText("管理员密码")).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "进入面板" })).not.toBeInTheDocument();
  });

  it("renders meta failures with an explicit retry action", async () => {
    authMocks.auth.meta = null;
    authMocks.auth.error = "无法连接管理服务";
    const user = userEvent.setup();

    renderPage();
    expect(screen.getByRole("alert")).toHaveTextContent("无法加载服务状态");
    await user.click(screen.getByRole("button", { name: "重试" }));

    expect(authMocks.refreshMeta).toHaveBeenCalledOnce();
  });

  it("maps login rate limits and Retry-After to an actionable message", async () => {
    authMocks.login.mockRejectedValueOnce(
      new AdminApiError(429, "login_rate_limited", "", "18"),
    );
    const user = userEvent.setup();
    renderPage();

    await user.type(screen.getByLabelText("管理员密码"), "wrong");
    await user.click(screen.getByRole("button", { name: "进入面板" }));

    expect(await screen.findByRole("alert")).toHaveTextContent("18 秒后重试");
  });
});
