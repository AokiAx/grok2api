import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { LoginPage } from "@/pages/LoginPage";

const authMocks = vi.hoisted(() => ({
  login: vi.fn(),
  auth: {
    ready: true,
    meta: { auth_required: true, setup_required: false, version: "1", api_version: "v1" },
  },
}));

vi.mock("@/auth/AuthContext", () => ({
  useAuth: () => ({ ...authMocks.auth, login: authMocks.login }),
  useIsAuthenticated: () => false,
}));

function renderPage() {
  return render(<MemoryRouter><LoginPage /></MemoryRouter>);
}

beforeEach(() => {
  authMocks.login.mockResolvedValue(undefined);
  authMocks.auth.ready = true;
  authMocks.auth.meta = {
    auth_required: true,
    setup_required: false,
    version: "1",
    api_version: "v1",
  };
});

describe("LoginPage", () => {
  it("submits the administrator password and remember preference", async () => {
    const user = userEvent.setup();
    renderPage();

    await user.type(screen.getByLabelText("管理员密码"), "secret");
    await user.click(screen.getByRole("checkbox", { name: "记住登录" }));
    await user.click(screen.getByRole("button", { name: "进入面板" }));

    expect(authMocks.login).toHaveBeenCalledWith("secret", false);
  });

  it("surfaces setup-required metadata without treating remember as browser storage", () => {
    authMocks.auth.meta = {
      auth_required: true,
      setup_required: true,
      version: "1",
      api_version: "v1",
    };

    renderPage();

    expect(screen.getByRole("status")).toHaveTextContent("管理员账号尚未初始化");
    expect(screen.getByText("记住登录")).toBeInTheDocument();
  });
});
